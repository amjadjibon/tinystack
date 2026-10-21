package cloudwatch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// publishInterval is how often published observations are written to the store.
var publishInterval = time.Second

// publishedSequence is the key suffix of a published series' point for a minute, so each flush finds and
// extends that point. Public ingestion never writes AWS/* namespaces, so it cannot collide.
const publishedSequence = 0

// aggregate is the statistics of the observations of one series within one minute that no flush has written yet.
type aggregate struct {
	scope      request.Scope
	namespace  string
	name       string
	unit       string
	dimensions []awsmetrics.Dimension
	minute     int64
	count      float64
	sum        float64
	min        float64
	max        float64
}

// published collects the observations other services report with Publish. Writing every observation would make a
// hot path such as an SQS send loop pay for a store transaction per message; aggregating them per series and
// minute in memory bounds the store work to one transaction per publishInterval, which touches only the series
// that saw an observation and extends one stored point per series and minute.
type published struct {
	pending map[string]*aggregate
	closed  bool
	mu      sync.Mutex
}

var _ awsmetrics.Sink = (*Service)(nil)

// Publish records observations of the built-in metrics of an AWS/* namespace for the account and Region of
// scope. It only updates memory, so callers may use it on hot paths, but they must call it after their own
// transaction has committed. The next flush stores the observations like any other metric data point: metric
// streams and alarms see them, and ListMetrics, GetMetricStatistics and GetMetricData return them.
func (s *Service) Publish(scope request.Scope, namespace string, data ...awsmetrics.Datum) {
	minute := s.now().Unix() / 60 * 60
	s.pub.mu.Lock()
	defer s.pub.mu.Unlock()
	if s.pub.closed {
		return
	}

	for i := range data {
		d := &data[i]
		key := seriesKey(scope, namespace, d, minute)
		a := s.pub.pending[key]
		if a == nil {
			a = &aggregate{
				scope: scope, namespace: namespace, name: d.Name, unit: d.Unit, dimensions: d.Dimensions,
				minute: minute, min: d.Value, max: d.Value,
			}
			s.pub.pending[key] = a
		}

		a.count++
		a.sum += d.Value
		a.min, a.max = min(a.min, d.Value), max(a.max, d.Value)
	}
}

func seriesKey(scope request.Scope, namespace string, d *awsmetrics.Datum, minute int64) string {
	var b strings.Builder
	b.WriteString(scope.AccountID)
	b.WriteByte(0)
	b.WriteString(scope.Region)
	b.WriteByte(0)
	b.WriteString(namespace)
	b.WriteByte(0)
	b.WriteString(d.Name)
	b.WriteByte(0)
	b.WriteString(d.Unit)
	b.WriteByte(0)
	b.WriteString(strconv.FormatInt(minute, 10))
	for _, dim := range d.Dimensions {
		b.WriteByte(0)
		b.WriteString(dim.Name)
		b.WriteByte('=')
		b.WriteString(dim.Value)
	}

	return b.String()
}

// drain takes what Publish has collected; closing it makes later Publish calls drop their observations.
func (s *Service) drain(closing bool) map[string]*aggregate {
	s.pub.mu.Lock()
	defer s.pub.mu.Unlock()
	batch := s.pub.pending
	s.pub.pending = map[string]*aggregate{}
	s.pub.closed = s.pub.closed || closing
	return batch
}

// flushPublished writes the collected observations in one transaction. It finishes even when the service is
// stopping, because Stop flushes once more after the group ends.
func (s *Service) flushPublished(ctx context.Context) {
	s.flush(ctx, s.drain(false))
}

func (s *Service) flush(ctx context.Context, batch map[string]*aggregate) {
	if len(batch) == 0 {
		return
	}

	now := s.now()
	err := s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
		for _, a := range batch {
			if err := s.storePublished(tx, a, now); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		s.logger.Warn("storing published CloudWatch metrics failed", "error", err)
	}
}

// storePublished adds an aggregate to its series' point for the minute. Streams receive only what the aggregate
// adds, as they do for any other data point.
func (s *Service) storePublished(tx storage.WriteTx, a *aggregate, now time.Time) error {
	dims := make([]api.Dimension, len(a.dimensions))
	for i, d := range a.dimensions {
		dims[i] = api.Dimension{Name: new(d.Name), Value: new(d.Value)}
	}

	rec, id, err := recordMetric(tx, a.scope, a.namespace, a.name, dims, now)
	if err != nil {
		return err
	}

	added := point{
		Unit: a.unit, T: float64(a.minute), Count: a.count, Sum: a.sum, Min: a.min, Max: a.max, Summary: true,
	}
	key := dataPrefix(a.scope, id) + fmt.Sprintf("%015d%010d", a.minute*1000, publishedSequence)
	stored, found, err := storage.Read[point](tx, key)
	if err != nil {
		return err
	}

	total := added
	if found {
		b := &bucket{}
		b.add(stored)
		b.add(&added)
		total = point{Unit: b.unit, T: added.T, Count: b.count, Sum: b.sum, Min: b.min, Max: b.max, Summary: true}
	} else {
		prune(tx, dataPrefix(a.scope, id), now.Add(-retention))
	}

	if err := storage.Write(tx, key, &total); err != nil {
		return err
	}

	return s.queueStreamPoint(tx, a.scope, rec, &added, now)
}

// Reset drops observations not yet stored, which the reset would otherwise bring back.
func (s *Service) Reset(context.Context) { s.drain(false) }
