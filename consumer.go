// Copyright © 2020 Dmitry Stoletov <info@imega.ru>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mt

import (
	"errors"
	"fmt"

	"github.com/streadway/amqp"
)

type consumer struct {
	handler   func(*Request) error
	conn      *amqp.Connection
	queue     *amqp.Queue
	channel   *amqp.Channel
	options   *Exchange
	dsn       string
	shutdown  bool
	isDialing bool
	log       Logger
}

func newConsumer(log Logger, dsn string, options *Exchange) *consumer {
	return &consumer{
		log:     log,
		dsn:     dsn,
		options: options,
	}
}

var (
	errConnect           = errors.New("failed to connect to amqp")
	errOpenChannel       = errors.New("failed to open channel")
	errDeclareExchange   = errors.New("failed to declare exchange")
	errSetQos            = errors.New("failed to set Qos")
	errConsumeQueue      = errors.New("failed to consume queue")
	errExchangeNameEmpty = errors.New("exchange name is empty")
	errExchangeTypeEmpty = errors.New("exchange type is empty")
	errDeclareQueue      = errors.New("failed to declare queue")
	errBindQueue         = errors.New("failed to bind queue")
)

func (c *consumer) connect() error {
	c.isDialing = true

	conn, err := dial(c.log, c.dsn)
	if err != nil {
		c.isDialing = false

		return fmt.Errorf("%w, %s", errConnect, err)
	}

	c.conn = conn

	ch, err := channelDeclare(c.conn)
	if err != nil {
		return fmt.Errorf("%w, %s", errOpenChannel, err)
	}

	c.channel = ch

	if err := exchangeDeclare(c.channel, c.options); err != nil {
		return fmt.Errorf("%w, %s", errDeclareExchange, err)
	}

	c.isDialing = false

	return nil
}

func (c *consumer) reConnect() (<-chan amqp.Delivery, error) {
	if err := c.connect(); err != nil {
		c.log.Errorf("failed to reconnect: %s", err)
	}

	return c.announce()
}

func (c *consumer) announce() (<-chan amqp.Delivery, error) {
	err := c.channel.Qos(c.options.Queue.PrefetchCount, 0, false)
	if err != nil {
		return nil, fmt.Errorf("%w, %s", errSetQos, err)
	}

	queue, err := queueDeclare(c.channel, c.options)
	if err != nil {
		return nil, fmt.Errorf("%w, %s", errDeclareQueue, err)
	}

	c.queue = queue

	delivery, err := getDelivery(c.channel, c.options.Queue.Name, c.options.Queue.Consumer)
	if err != nil {
		return nil, fmt.Errorf("%w, %s", errConsumeQueue, err)
	}

	return delivery, nil
}

func (c *consumer) handle(deliveries <-chan amqp.Delivery) {
	var err error

	for {
		tag := c.options.Queue.Consumer.Tag

		for d := range deliveries {
			err := c.handler(&Request{Body: d.Body, Header: Header(d.Headers)})
			if err == nil {
				c.log.Debugf("Message ACK, consumerTag=%s", tag)

				if err := d.Ack(false); err != nil {
					c.log.Errorf("Failed ACK message, %s", err)
				}
			} else {
				c.log.Debugf("Message NACK, consumerTag=%s", tag)

				if err := d.Nack(false, c.options.Queue.Consumer.Requeue); err != nil {
					c.log.Errorf("Failed ACK message, %s", err)
				}
			}
		}

		if c.shutdown {
			c.log.Debugf("consumer will close %s", tag)

			err := c.channel.Close()
			if err != nil {
				c.log.Errorf("consumer close channel %s,", tag)
			}

			c.channel = nil

			err = c.conn.Close()
			if err != nil {
				c.log.Errorf("consumer close connection %s,", tag)
			}

			c.conn = nil

			c.log.Debugf("consumer closed %s", tag)

			return
		}

		deliveries, err = c.reConnect()
		if err != nil {
			c.log.Errorf("failed to reconnect: %s", err)
		}

		c.log.Debugf("consumer reconnected %s", c.options.Queue.Consumer.Tag)
	}
}

func getDelivery(
	channel *amqp.Channel,
	queueName string,
	consumer Consume,
) (<-chan amqp.Delivery, error) {
	return channel.Consume(
		queueName,
		consumer.Tag,
		consumer.NoAck,
		consumer.Exclusive,
		false,
		consumer.NoWait,
		consumer.Arguments,
	)
}

func channelDeclare(conn *amqp.Connection) (*amqp.Channel, error) {
	channel, err := conn.Channel()
	if err != nil {
		return nil, err
	}

	return channel, nil
}

func exchangeDeclare(channel *amqp.Channel, opts *Exchange) error {
	if opts.Name == "" {
		return errExchangeNameEmpty
	}

	if opts.Type == "" {
		return errExchangeTypeEmpty
	}

	if err := channel.ExchangeDeclare(
		opts.Name,
		opts.Type,
		opts.Durable,
		opts.AutoDelete,
		opts.Internal,
		opts.NoWait,
		opts.Arguments,
	); err != nil {
		return err
	}

	return nil
}

func queueDeclare(channel *amqp.Channel, opts *Exchange) (*amqp.Queue, error) {
	q, err := channel.QueueDeclare(
		opts.Queue.Name,
		opts.Queue.Durable,
		opts.Queue.AutoDelete,
		false, // exclusive
		opts.Queue.NoWait,
		opts.Queue.Arguments,
	)
	if err != nil {
		return nil, fmt.Errorf("%w, %s", errDeclareQueue, err)
	}

	err = channel.QueueBind(
		opts.Queue.Name,
		opts.Binding.Key,
		opts.Name,
		opts.Binding.NoWait,
		opts.Binding.Arguments,
	)
	if err != nil {
		return nil, fmt.Errorf("%w, %s", errBindQueue, err)
	}

	return &q, nil
}
