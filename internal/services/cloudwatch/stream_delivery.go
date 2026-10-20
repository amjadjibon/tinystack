package cloudwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// streamInterval can be shortened by package tests without exposing public configuration.
var streamInterval = time.Second

type streamPoint struct {
	Metric metric
	Point  point
	Minute int64
}

type streamRecord struct {
	Stream     string             `json:"metric_stream_name"`
	Account    string             `json:"account_id"`
	Region     string             `json:"region"`
	Namespace  string             `json:"namespace"`
	Metric     string             `json:"metric_name"`
	Dimensions map[string]string  `json:"dimensions"`
	Value      map[string]float64 `json:"value"`
	Unit       string             `json:"unit"`
	Timestamp  int64              `json:"timestamp"`
}

// AWS JSON format and one-send-per-timestamp behavior:
// https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-metric-streams-formats-json.html
// https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-metric-streams-troubleshoot.html
//
// queueStreamPoint runs in the ingestion transaction: stopped streams do not collect history.
func (s *Service) queueStreamPoint(tx storage.WriteTx, scope request.Scope, mt metric, p *point, now time.Time) error {
	if fromUnix(p.T).Before(now.Add(-48 * time.Hour)) {
		return nil
	}

	streams, err := storage.Records[metricStream](tx, scope.Prefix(service, kindStream))
	if err != nil {
		return err
	}

	for _, st := range streams {
		c := &st.Config
		if common.Value(c.State) != "running" || !streamMatches(c, mt.Namespace, mt.Name) {
			continue
		}

		minute := int64(p.T) / 60 * 60
		suffix := fmt.Sprintf("%015d/%s/%s", minute*1000, metricID(mt.Namespace, mt.Name, mt.Dimensions), p.Unit)
		sent := streamSent(scope, *c.Name)
		if _, exists := tx.Get(sent + suffix); exists {
			continue
		}

		prune(tx, sent, now.Add(-48*time.Hour))
		key := streamPending(scope, *c.Name) + suffix
		pending, found, err := storage.Read[streamPoint](tx, key)
		if err != nil {
			return err
		}

		if !found {
			pending = &streamPoint{Metric: mt, Point: *p, Minute: minute}
		} else {
			b := &bucket{}
			b.add(&pending.Point)
			b.add(p)
			pending.Point = point{
				Unit: b.unit, Values: b.values, Counts: b.counts, Count: b.count,
				Sum: b.sum, Min: b.min, Max: b.max, Summary: b.summary,
			}
		}

		if err := storage.Write(tx, key, pending); err != nil {
			return err
		}
	}

	return nil
}

func streamJSON(scope request.Scope, c *api.GetMetricStreamOutput, p *streamPoint) ([]byte, error) {
	b := &bucket{}
	b.add(&p.Point)
	value := map[string]float64{"max": b.max, "min": b.min, "sum": b.sum, "count": b.count}
	for _, cfg := range c.StatisticsConfigurations {
		for _, mt := range cfg.IncludeMetrics {
			if common.Value(mt.Namespace) != p.Metric.Namespace || common.Value(mt.MetricName) != p.Metric.Name {
				continue
			}

			for _, st := range cfg.AdditionalStatistics {
				f, err := statistic(st)
				if err != nil {
					return nil, err
				}

				if v, ok := f(b); ok {
					value[st] = v
				}
			}
		}
	}

	dims := map[string]string{}
	for _, d := range p.Metric.Dimensions {
		dims[common.Value(d.Name)] = common.Value(d.Value)
	}

	data, err := json.Marshal(streamRecord{
		Stream: *c.Name, Account: scope.AccountID, Region: scope.Region,
		Namespace: p.Metric.Namespace, Metric: p.Metric.Name, Dimensions: dims, Timestamp: p.Minute * 1000,
		Value: value, Unit: p.Point.Unit,
	})
	return append(data, '\n'), err
}

func (s *Service) visitStreams(ctx context.Context) {
	var targets []target
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(streamIndex) {
			parts := strings.SplitN(strings.TrimPrefix(key, streamIndex), "/", 3)
			if len(parts) == 3 {
				targets = append(targets, target{request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2]})
			}
		}

		return nil
	})
	if err != nil {
		s.logger.Warn("listing metric streams failed", "error", err)
		return
	}

	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}

		if err := s.flushStream(ctx, t); err != nil {
			s.logger.Warn("flushing metric stream failed", "stream", t.name, "error", err)
		}
	}
}

// flushStream claims completed minutes before external I/O. CloudWatch does not replay already sent timestamps.
// Failed calls are counted and retained as LastError; Firehose owns destination retry after accepting a record.
func (s *Service) flushStream(ctx context.Context, t target) error {
	var records []any
	var claimed *metricStream
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, found, err := storage.Read[metricStream](tx, streamKey(t.scope, t.name))
		if err != nil || !found || common.Value(st.Config.State) != "running" {
			return err
		}

		prefix := streamPending(t.scope, t.name)
		size := 0
		for _, key := range tx.Keys(prefix) {
			p, _, err := storage.Read[streamPoint](tx, key)
			if err != nil {
				return err
			}

			if p.Minute+60 > s.now().Unix() {
				continue
			}

			data, err := streamJSON(t.scope, &st.Config, p)
			if err != nil {
				return err
			}

			if len(records) == 500 || size+len(data) > 4*1024*1024 {
				break
			}

			records = append(records, map[string]any{"Data": string(data)})
			size += len(data)
			tx.Delete(key)
			tx.Put(streamSent(t.scope, t.name)+strings.TrimPrefix(key, prefix), nil)
		}

		claimed = st
		return nil
	})
	if err != nil || len(records) == 0 {
		return err
	}

	c := &claimed.Config
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: t.scope, Service: "firehose"})
	result, deliveryErr := s.authorizer.Call(ctx, s.caller, catalog.ServiceDelivery{
		Principal: "streams.metrics.cloudwatch.amazonaws.com", SourceARN: *c.Arn, SourceAccount: t.scope.AccountID,
		Role: &catalog.DeliveryRole{
			ARN: *c.RoleArn, Session: t.name[:min(len(t.name), 64)], Service: "firehose", Operation: "PutRecordBatch",
			Params: map[string]any{
				"DeliveryStreamName": strings.SplitN(*c.FirehoseArn, "deliverystream/", 2)[1],
				"Records":            records,
			},
		},
	})

	failedCount := int64(0)
	if deliveryErr != nil {
		failedCount = int64(len(records))
	} else if failed, ok := result["FailedPutCount"].(float64); ok && failed > 0 {
		failedCount = min(int64(failed), int64(len(records)))
		deliveryErr = fmt.Errorf("firehose rejected %d metric stream records", failedCount)
	}

	if deliveryErr != nil {
		s.logger.Warn("metric stream delivery failed", "stream", t.name, "error", deliveryErr)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, found, err := storage.Read[metricStream](tx, streamKey(t.scope, t.name))
		if err != nil || !found || current.Generation != claimed.Generation {
			return err
		}

		if deliveryErr != nil {
			current.Failed += failedCount
			current.LastError = deliveryErr.Error()
		} else {
			current.LastError = ""
		}

		current.Delivered += int64(len(records)) - failedCount

		return storage.Write(tx, streamKey(t.scope, t.name), current)
	})
}
