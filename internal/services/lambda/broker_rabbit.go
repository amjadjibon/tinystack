package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/amjadjibon/tinystack/internal/aws/request"
)

func rabbitEvent(mp *mapping, messages []amqp.Delivery) []byte {
	records := make([]map[string]any, 0, len(messages))
	for i := range messages {
		m := &messages[i]
		records = append(records, map[string]any{
			"data": m.Body, "redelivered": m.Redelivered,
			"basicProperties": map[string]any{
				"contentType":     m.ContentType,
				"contentEncoding": m.ContentEncoding,
				"headers":         m.Headers,
				"deliveryMode":    m.DeliveryMode,
				"priority":        m.Priority,
				"correlationId":   m.CorrelationId,
				"replyTo":         m.ReplyTo,
				"expiration":      m.Expiration,
				"messageId":       m.MessageId,
				"timestamp":       m.Timestamp.Unix(),
				"type":            m.Type,
				"userId":          m.UserId,
				"appId":           m.AppId,
			},
		})
	}

	payload, _ := json.Marshal(map[string]any{
		"eventSource": "aws:rmq", "eventSourceArn": mp.EventSourceArn,
		"rmqMessagesByQueue": map[string]any{mp.Queues[0] + "::/": records},
	})
	return payload
}

func (s *Service) rabbitEndpoint(ctx context.Context, scope request.Scope, mp *mapping) (string, error) {
	out, err := s.brokerCall(
		ctx,
		scope,
		mp,
		"mq",
		"DescribeBroker",
		map[string]any{"BrokerId": brokerID(mp.EventSourceArn)},
	)
	if err != nil {
		return "", err
	}

	// Caller results use JSON-style arrays and objects.
	instances, _ := out["BrokerInstances"].([]any)
	for _, instance := range instances {
		fields, _ := instance.(map[string]any)
		endpoints, _ := fields["Endpoints"].([]any)
		for _, endpoint := range endpoints {
			text, _ := endpoint.(string)
			if strings.HasPrefix(text, "amqp://") || strings.HasPrefix(text, "amqps://") {
				if !strings.Contains(text, ".amazonaws.com") {
					return text, nil
				}
			}
		}
	}

	return "", fmt.Errorf("MQ has no data-plane endpoint")
}

func (s *Service) consumeRabbit(ctx context.Context, scope request.Scope, mp *mapping) error {
	endpoint, err := s.rabbitEndpoint(ctx, scope, mp)
	if err != nil {
		return err
	}

	user, password, err := s.brokerCredentials(ctx, scope, mp)
	if err != nil {
		return err
	}

	address, err := url.Parse(endpoint)
	if err != nil {
		return err
	}

	address.User = url.UserPassword(user, password)
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var transport net.Conn
	conn, err := amqp.DialConfig(address.String(), amqp.Config{Dial: func(network, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: 10 * time.Second}
		c, dialErr := d.DialContext(dialCtx, network, addr)
		if dialErr != nil {
			return nil, dialErr
		}

		if deadlineErr := c.SetDeadline(time.Now().Add(10 * time.Second)); deadlineErr != nil {
			_ = c.Close()
			return nil, deadlineErr
		}

		transport = c
		context.AfterFunc(dialCtx, func() { _ = c.Close() })
		return c, nil
	}})
	if err != nil {
		return err
	}

	defer func() { _ = conn.Close() }()
	if deadlineErr := transport.SetDeadline(time.Time{}); deadlineErr != nil {
		return deadlineErr
	}

	ch, err := conn.Channel()
	if err != nil {
		return err
	}

	if qosErr := ch.Qos(int(mp.BatchSize), 0, false); qosErr != nil {
		return qosErr
	}

	messages, err := ch.ConsumeWithContext(ctx, mp.Queues[0], mp.UUID, false, false, false, false, nil)
	if err != nil {
		return err
	}

	for ctx.Err() == nil {
		var first amqp.Delivery
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-messages:
			if !ok {
				return fmt.Errorf("RabbitMQ consumer closed")
			}

			first = m
		}

		batch := []amqp.Delivery{first}
		window := mp.batchingWindow()

		timer := time.NewTimer(window)
	gather:
		for int64(len(batch)) < mp.BatchSize {
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				break gather
			case m, ok := <-messages:
				if !ok {
					timer.Stop()
					return fmt.Errorf("RabbitMQ consumer closed")
				}

				batch = append(batch, m)
			}
		}

		timer.Stop()
		success := s.invokeBroker(ctx, scope, mp, rabbitEvent(mp, batch))
		for i := range batch {
			if success {
				err = batch[i].Ack(false)
			} else {
				err = batch[i].Nack(false, true)
			}

			if err != nil {
				return err
			}
		}

		if !success {
			sleep(ctx, pollBackoff)
		}
	}

	return ctx.Err()
}
