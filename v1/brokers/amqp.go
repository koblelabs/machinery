package brokers

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/koblelabs/machinery/v1/common"
	"github.com/koblelabs/machinery/v1/config"
	"github.com/koblelabs/machinery/v1/log"
	"github.com/koblelabs/machinery/v1/tasks"
	"github.com/streadway/amqp"
)

// AMQPBroker represents an AMQP broker
type AMQPBroker struct {
	Broker
	common.AMQPConnector
}

// NewAMQPBroker creates new AMQPBroker instance
func NewAMQPBroker(cnf *config.Config) Interface {
	return &AMQPBroker{Broker: New(cnf), AMQPConnector: common.AMQPConnector{}}
}

// StartConsuming enters a loop and waits for incoming messages
func (b *AMQPBroker) StartConsuming(consumerTag string, concurrency int, taskProcessor TaskProcessor) (bool, error) {
	b.startConsuming(consumerTag, taskProcessor)

	conn, channel, queue, _, amqpCloseChan, err := b.Connect(
		b.cnf.Broker,
		b.cnf.TLSConfig,
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
	defer b.Close(channel, conn)

	if err = channel.Qos(
		b.cnf.AMQP.PrefetchCount,
		0,     // prefetch size
		false, // global
	); err != nil {
		return b.retry, fmt.Errorf("Channel qos error: %s", err)
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
		return b.retry, fmt.Errorf("Queue consume error: %s", err)
	}

	log.INFO.Print("[*] Waiting for messages. To exit press CTRL+C")

	if err := b.consume(deliveries, concurrency, taskProcessor, amqpCloseChan); err != nil {
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

	conn, channel, _, confirmsChan, _, err := b.Connect(
		b.cnf.Broker,
		b.cnf.TLSConfig,
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
	defer b.Close(channel, conn)

	if err := channel.Publish(
		b.cnf.AMQP.Exchange,  // exchange name
		signature.RoutingKey, // routing key
		false,                // mandatory
		false,                // immediate
		amqp.Publishing{
			Headers:      amqp.Table(signature.Headers),
			ContentType:  "application/json",
			Body:         message,
			DeliveryMode: amqp.Persistent,
		},
	); err != nil {
		return err
	}

	confirmed := <-confirmsChan

	if confirmed.Ack {
		return nil
	}

	return fmt.Errorf("Failed delivery of delivery tag: %v", confirmed.DeliveryTag)
}

// PurgeQueue ... removes all the items from the queue
func (b *AMQPBroker) PurgeQueue(queueName string) (bool, int, error) {
	conn, channel, _, _, _, err := b.Connect(
		b.cnf.Broker,
		b.cnf.TLSConfig,
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
		return b.retry, 0, err
	}
	defer b.Close(channel, conn)

	if err = channel.Qos(
		b.cnf.AMQP.PrefetchCount,
		0,     // prefetch size
		false, // global
	); err != nil {
		return b.retry, 0, fmt.Errorf("Channel qos error: %s", err)
	}

	n, err := b.DeleteQueue(channel, queueName)

	return b.retry, n, err
}

// consume takes delivered messages from the channel and manages a worker pool
// to process tasks concurrently
func (b *AMQPBroker) consume(deliveries <-chan amqp.Delivery, concurrency int, taskProcessor TaskProcessor, amqpCloseChan <-chan *amqp.Error) error {
	pool := make(chan struct{}, concurrency)

	// initialize worker pool with maxWorkers workers
	go func() {
		for i := 0; i < concurrency; i++ {
			pool <- struct{}{}
		}
	}()

	errorsChan := make(chan error)

	// Use wait group to make sure task processing completes on interrupt signal
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case amqpErr := <-amqpCloseChan:
			return amqpErr
		case err := <-errorsChan:
			return err
		case d := <-deliveries:
			if concurrency > 0 {
				// get worker from pool (blocks until one is available)
				<-pool
			}

			wg.Add(1)

			// Consume the task inside a gotourine so multiple tasks
			// can be processed concurrently
			go func() {
				defer wg.Done()

				if err := b.consumeOne(d, taskProcessor); err != nil {
					errorsChan <- err
				}

				if concurrency > 0 {
					// give worker back to pool
					pool <- struct{}{}
				}
			}()
		case <-b.stopChan:
			return nil
		}
	}
}

// consumeOne processes a single message using TaskProcessor
func (b *AMQPBroker) consumeOne(d amqp.Delivery, taskProcessor TaskProcessor) error {
	if len(d.Body) == 0 {
		d.Nack(false, false)                           // multiple, requeue
		return errors.New("Received an empty message") // RabbitMQ down?
	}

	log.INFO.Printf("Received new message: %s", d.Body)

	// Unmarshal message body into signature struct
	signature := new(tasks.Signature)
	if err := json.Unmarshal(d.Body, signature); err != nil {
		d.Nack(false, false) // multiple, requeue
		return err
	}

	// If the task is not registered, we nack it and requeue,
	// there might be different workers for processing specific tasks
	if !b.IsTaskRegistered(signature.Name) {
		d.Nack(false, true) // multiple, requeue
		return nil
	}

	d.Ack(false) // multiple
	return taskProcessor.Process(signature)
}

// delay a task by delayDuration miliseconds, the way it works is a new queue
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
	queueName := signature.UUID

	declareQueueArgs := amqp.Table{
		// Exchange where to send messages after TTL expiration.
		"x-dead-letter-exchange": b.cnf.AMQP.Exchange,
		// Routing key which use when resending expired messages.
		"x-dead-letter-routing-key": b.cnf.AMQP.BindingKey,
		// Time in milliseconds
		// after that message will expire and be sent to destination.
		"x-message-ttl": delayMs,
		// Time after that the queue will be deleted...3 seconds after queue is unused, it will (hopefully) be cleaned up
		"x-expires": delayMs + 3000,
	}
	conn, channel, _, _, _, err := b.Connect(
		b.cnf.Broker,
		b.cnf.TLSConfig,
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
	defer b.Close(channel, conn)

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
