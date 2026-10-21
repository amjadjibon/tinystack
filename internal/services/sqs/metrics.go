package sqs

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// The AWS/SQS metrics a queue reports, with the dimension QueueName.
const (
	metricNamespace = "AWS/SQS"

	metricSent       = "NumberOfMessagesSent"
	metricReceived   = "NumberOfMessagesReceived"
	metricDeleted    = "NumberOfMessagesDeleted"
	metricEmpty      = "NumberOfEmptyReceives"
	metricSentSize   = "SentMessageSize"
	metricVisible    = "ApproximateNumberOfMessagesVisible"
	metricNotVisible = "ApproximateNumberOfMessagesNotVisible"
	metricDelayed    = "ApproximateNumberOfMessagesDelayed"
	metricOldestAge  = "ApproximateAgeOfOldestMessage"
)

// sampleInterval is how often the approximate gauges are sampled, as AWS does once a minute.
var sampleInterval = time.Minute

// inactiveAfter is how long a queue without requests keeps reporting its gauges; AWS stops after six hours.
const inactiveAfter = 6 * time.Hour

// UseMetrics has the service report its AWS/SQS metrics to sink. Call it before serving requests.
func (s *Service) UseMetrics(sink awsmetrics.Sink) { s.metrics.Use(sink) }

// publish reports metrics of the queue stored under key, after the transaction that caused them has committed,
// and keeps the queue's gauges sampled while it is in use.
func (s *Service) publish(key string, data ...awsmetrics.Datum) {
	scope, name := keyScope(key)
	dims := []awsmetrics.Dimension{{Name: "QueueName", Value: name}}
	for i := range data {
		data[i].Dimensions = dims
	}

	s.metrics.Publish(scope, metricNamespace, data...)
	s.mu.Lock()
	s.active[key] = s.now()
	s.mu.Unlock()
}

func keyScope(key string) (scope request.Scope, name string) {
	parts := strings.SplitN(key, "/", 4)
	return request.Scope{AccountID: parts[0], Region: parts[1]}, parts[3]
}

func counter(name string, value int) awsmetrics.Datum {
	return awsmetrics.Datum{Name: name, Unit: awsmetrics.Count, Value: float64(value)}
}

// publishReceive reports a receive: the messages it returned, or an empty receive.
func (s *Service) publishReceive(key string, messages int) {
	if messages == 0 {
		s.publish(key, counter(metricEmpty, 1))
		return
	}

	s.publish(key, counter(metricReceived, messages))
}

func messageSize(size int64) awsmetrics.Datum {
	return awsmetrics.Datum{Name: metricSentSize, Unit: awsmetrics.Bytes, Value: float64(size)}
}

// sample reports the gauges of every queue that has been used recently.
func (s *Service) sample(ctx context.Context) {
	s.mu.Lock()
	now := s.now()
	var keys []string
	for key, last := range s.active {
		if now.Sub(last) > inactiveAfter {
			delete(s.active, key)
			continue
		}

		keys = append(keys, key)
	}

	s.mu.Unlock()
	for _, key := range keys {
		var q *queue
		err := s.store.View(ctx, func(tx storage.ReadTx) (err error) {
			q, err = load(tx, key)
			return err
		})
		if err != nil {
			s.mu.Lock()
			delete(s.active, key)
			s.mu.Unlock()
			continue
		}

		s.reportGauges(key, q, now.UnixMilli())
	}
}

// reportGauges reports a queue's approximate gauges, which count the same messages as its attributes.
func (s *Service) reportGauges(key string, q *queue, now int64) {
	visible, inFlight, delayed := q.counts(now)
	var age float64
	for i := range q.Messages {
		if msg := &q.Messages[i]; msg.VisibleAt <= now || msg.ReceiveCount > 0 {
			age = max(age, float64(now-msg.SentAt)/1000)
		}
	}

	scope, name := keyScope(key)
	dims := []awsmetrics.Dimension{{Name: "QueueName", Value: name}}
	s.metrics.Publish(scope, metricNamespace,
		awsmetrics.Datum{Name: metricVisible, Unit: awsmetrics.Count, Dimensions: dims, Value: float64(visible)},
		awsmetrics.Datum{Name: metricNotVisible, Unit: awsmetrics.Count, Dimensions: dims, Value: float64(inFlight)},
		awsmetrics.Datum{Name: metricDelayed, Unit: awsmetrics.Count, Dimensions: dims, Value: float64(delayed)},
		awsmetrics.Datum{Name: metricOldestAge, Unit: awsmetrics.Seconds, Dimensions: dims, Value: age},
	)
}

// counts splits the queue's messages into those a receive can return, those in flight, and those still delayed.
func (q *queue) counts(now int64) (visible, inFlight, delayed int64) {
	for i := range q.Messages {
		msg := &q.Messages[i]
		switch {
		case msg.VisibleAt <= now:
			visible++
		case msg.ReceiveCount > 0:
			inFlight++
		default:
			delayed++
		}
	}

	return visible, inFlight, delayed
}
