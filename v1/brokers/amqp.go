package brokers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/GetStream/machinery/v1/common"
	"github.com/GetStream/machinery/v1/config"
	"github.com/GetStream/machinery/v1/log"
	"github.com/GetStream/machinery/v1/tasks"
	"github.com/streadway/amqp"
	"github.com/GetStream/go-lzo"
	"bytes"
)

const encodingLZO = "lzo"

// AMQPBroker represents an AMQP broker
type AMQPBroker struct {
	Broker
	*common.AMQPConnector
}

// NewAMQPBroker creates new AMQPBroker instance
func NewAMQPBroker(cnf *config.Config) Interface {
	return &AMQPBroker{
		Broker:        New(cnf),
		AMQPConnector: common.NewAMQPConnector(cnf.Broker, cnf.TLSConfig),
	}
}


func (b *AMQPBroker) shouldCompress(payload []byte) bool {
	return len(payload) > 100
}

// StartConsuming enters a loop and waits for incoming messages
func (b *AMQPBroker) StartConsuming(consumerTag string, concurrency int, taskProcessor TaskProcessor) (bool, error) {
	b.startConsuming(consumerTag, taskProcessor)

	channel, queue, _, err := b.Exchange(
		b.cnf.AMQP.Exchange,     // exchange name
		b.cnf.AMQP.ExchangeType, // exchange type
		b.cnf.DefaultQueue,      // queue name
		true,                    // queue durable
		false,                   // queue delete when unused
		b.cnf.AMQP.BindingKey, // queue binding key
		nil, // exchange declare args
		nil, // queue declare args
		amqp.Table(b.cnf.AMQP.QueueBindingArgs), // queue binding args
	)
	if err != nil {
		b.retryFunc(b.retryStopChan)
		return b.retry, err
	}
	defer channel.Close()

	if err = channel.Qos(
		b.cnf.AMQP.PrefetchCount,
		0,     // prefetch size
		false, // global
	); err != nil {
		return b.retry, fmt.Errorf("channel qos error: %s", err)
	}

	deliveries, err := channel.Consume(
		queue.Name,  // queue
		consumerTag, // consumer tag
		false,       // auto-ack
		false,       // exclusive
		false,       // no-local
		false,       // no-wait
		nil,         // arguments
	)
	if err != nil {
		return b.retry, fmt.Errorf("queue consume error: %s", err)
	}

	log.INFO.Print("[*] Waiting for messages. To exit press CTRL+C")

	if err := b.consume(deliveries, concurrency, taskProcessor); err != nil {
		return b.retry, err
	}

	return b.retry, nil
}

// StopConsuming quits the loop
func (b *AMQPBroker) StopConsuming() {
	b.stopConsuming()
}

// Publish places a new message on the default queue
func (b *AMQPBroker) Publish(signature *tasks.Signature) error {
	b.AdjustRoutingKey(signature)

	// Check the ETA signature field, if it is set and it is in the future,
	// delay the task
	if signature.ETA != nil {
		now := time.Now().UTC()

		if signature.ETA.After(now) {
			delayMs := int64(signature.ETA.Sub(now) / time.Millisecond)

			return b.delay(signature, delayMs)
		}
	}

	message, err := json.Marshal(signature)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %s", err)
	}

	compressed := b.shouldCompress(message)

	channel, _, confirmsChan, err := b.Exchange(
		b.cnf.AMQP.Exchange,     // exchange name
		b.cnf.AMQP.ExchangeType, // exchange type
		b.cnf.DefaultQueue,      // queue name
		true,                    // queue durable
		false,                   // queue delete when unused
		b.cnf.AMQP.BindingKey, // queue binding key
		nil, // exchange declare args
		nil, // queue declare args
		amqp.Table(b.cnf.AMQP.QueueBindingArgs), // queue binding args
	)
	if err != nil {
		return err
	}
	defer channel.Close()

	publishing := amqp.Publishing{
		Headers:      amqp.Table(signature.Headers),
		ContentType:  "application/json",
		Body:         message,
		DeliveryMode: amqp.Persistent,
	}

	if compressed {
		publishing.Body = lzo.Compress1X(message)
		publishing.ContentEncoding = encodingLZO
	}

	if err := channel.Publish(
		b.cnf.AMQP.Exchange,  // exchange name
		signature.RoutingKey, // routing key
		false,                // mandatory
		false,                // immediate
		publishing,
	); err != nil {
		return err
	}

	confirmed := <-confirmsChan

	if confirmed.Ack {
		return nil
	}

	return fmt.Errorf("failed delivery of delivery tag: %v", confirmed.DeliveryTag)
}

// consume takes delivered messages from the channel and manages a worker pool
// to process tasks concurrently
func (b *AMQPBroker) consume(deliveries <-chan amqp.Delivery, concurrency int, taskProcessor TaskProcessor) error {
	if concurrency < 1 {
		//XXX: assuming no constraints on concurrency
		//     (modeled by having an unrealistically high max worker cap)
		concurrency = math.MaxInt64
	}

	pool := semaphore.NewWeighted(int64(concurrency))
	errorsChan := make(chan error)
	quitChan := make(chan struct{})

	// Use wait group to make sure task processing completes on interrupt signal
	var wg sync.WaitGroup
	defer wg.Wait()
	// Signal to spawned goroutines that error are no longer handled
	defer close(quitChan)

	for {
		select {
		case amqpErr := <-b.AMQPConnector.ErrChan():
			return amqpErr
		case err := <-errorsChan:
			return err
		case d := <-deliveries:
			// Getting worker from pool (blocks until one is available)
			// This shouldn't return any errors since it fails only if the context is
			// canceled/contains error but just in case we handle failure anyway
			if err := pool.Acquire(context.TODO(), 1); err != nil {
				return err
			}

			wg.Add(1)

			// Consume the task inside a go routine so multiple tasks
			// can be processed concurrently
			go func() {
				err := b.consumeOne(d, taskProcessor)

				wg.Done()
				// give worker back to pool
				pool.Release(1)

				if err != nil {
					for {
						select {
						case <-quitChan:
							// main loop exited; ignoring error
							return
						case errorsChan <- err:
							// propagated error to main loop
							return
						default:
							// everything is blocked can't do anything but spin
						}
					}
				}
			}()
		case <-b.stopChan:
			return nil
		}
	}
}

// consumeOne processes a single message using TaskProcessor
func (b *AMQPBroker) consumeOne(d amqp.Delivery, taskProcessor TaskProcessor) error {
	var err error

	if len(d.Body) == 0 {
		d.Nack(false, false)                           // multiple, requeue
		return errors.New("received an empty message") // RabbitMQ down?
	}

	log.INFO.Printf("Received new message")

	body := d.Body

	// Decompress body if necessary
	if d.ContentEncoding == encodingLZO {
		r := bytes.NewReader(body)
		body, err = lzo.Decompress1X(r, len(body), 0)
		if err != nil {
			d.Nack(false, false)
			return err
		}
	}

	// Unmarshal message body into signature struct
	signature := new(tasks.Signature)
	if err := json.Unmarshal(body, signature); err != nil {
		d.Nack(false, false)
		return err
	}

	// If the task is not registered, we nack it and requeue,
	// there might be different workers for processing specific tasks
	if !b.IsTaskRegistered(signature.Name) {
		d.Nack(false, !b.cnf.AMQP.DropUnregisteredTasks) // multiple, requeue
		if b.cnf.AMQP.DropUnregisteredTasks {
			log.WARNING.Printf("Discarded unknown message: %s", signature.Name)
		} else {
			log.WARNING.Printf("Requeued unknown message: %s", signature.Name)
		}
		return nil
	}

	d.Ack(false) // multiple
	return taskProcessor.Process(signature)
}

// delay a task by delayDuration milliseconds, the way it works is a new queue
// is created without any consumers, the message is then published to this queue
// with appropriate ttl expiration headers, after the expiration, it is sent to
// the proper queue with consumers
func (b *AMQPBroker) delay(signature *tasks.Signature, delayMs int64) error {
	if delayMs <= 0 {
		return errors.New("Cannot delay task by 0ms")
	}

	message, err := json.Marshal(signature)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %s", err)
	}

	// It's necessary to redeclare the queue each time (to zero its TTL timer).
	queueName := fmt.Sprintf(
		"delay.%d.%s.%s",
		delayMs, // delay duration in mileseconds
		b.cnf.AMQP.Exchange,
		b.cnf.AMQP.BindingKey, // routing key
	)
	declareQueueArgs := amqp.Table{
		// Exchange where to send messages after TTL expiration.
		"x-dead-letter-exchange": b.cnf.AMQP.Exchange,
		// Routing key which use when resending expired messages.
		"x-dead-letter-routing-key": b.cnf.AMQP.BindingKey,
		// Time in milliseconds
		// after that message will expire and be sent to destination.
		"x-message-ttl": delayMs,
		// Time after that the queue will be deleted.
		"x-expires": delayMs * 2,
	}
	channel, _, _, err := b.Exchange(
		b.cnf.AMQP.Exchange,                     // exchange name
		b.cnf.AMQP.ExchangeType,                 // exchange type
		queueName,                               // queue name
		true,                                    // queue durable
		false,                                   // queue delete when unused
		queueName,                               // queue binding key
		nil,                                     // exchange declare args
		declareQueueArgs,                        // queue declare args
		amqp.Table(b.cnf.AMQP.QueueBindingArgs), // queue binding args
	)
	if err != nil {
		return err
	}
	defer channel.Close()

	if err := channel.Publish(
		b.cnf.AMQP.Exchange, // exchange
		queueName,           // routing key
		false,               // mandatory
		false,               // immediate
		amqp.Publishing{
			Headers:      amqp.Table(signature.Headers),
			ContentType:  "application/json",
			Body:         message,
			DeliveryMode: amqp.Persistent,
		},
	); err != nil {
		return err
	}

	return nil
}
