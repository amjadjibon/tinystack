package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
)

func (s *Service) kafkaEndpoints(ctx context.Context, scope request.Scope, mp *mapping) ([]string, error) {
	if mp.SelfManagedEventSource != nil {
		return mp.SelfManagedEventSource.Endpoints["KAFKA_BOOTSTRAP_SERVERS"], nil
	}

	out, err := s.brokerCall(
		ctx,
		scope,
		mp,
		"kafka",
		"GetBootstrapBrokers",
		map[string]any{"ClusterArn": mp.EventSourceArn},
	)
	if err != nil {
		return nil, err
	}

	text, _ := out["BootstrapBrokerString"].(string)
	if text == "" || strings.Contains(text, ".amazonaws.com") {
		return nil, fmt.Errorf("MSK has no plaintext data-plane endpoint")
	}

	return strings.Split(text, ","), nil
}

type kafkaEventRecord struct {
	Topic         string              `json:"topic"`
	TimestampType string              `json:"timestampType"`
	Key           []byte              `json:"key"`
	Value         []byte              `json:"value"`
	Headers       []map[string][]int8 `json:"headers"`
	Offset        int64               `json:"offset"`
	Timestamp     int64               `json:"timestamp"`
	Partition     int32               `json:"partition"`
}

func kafkaRecord(r *kgo.Record) kafkaEventRecord {
	// Lambda reports header values as Java's signed bytes.
	headers := make([]map[string][]int8, 0, len(r.Headers))
	for _, h := range r.Headers {
		value := make([]int8, len(h.Value))
		for i, b := range h.Value {
			value[i] = int8(b) // #nosec G115 -- wrapping to a signed byte is the intent
		}

		headers = append(headers, map[string][]int8{h.Key: value})
	}

	typ := "CREATE_TIME"
	if r.Attrs.TimestampType() == 1 {
		typ = "LOG_APPEND_TIME"
	}

	return kafkaEventRecord{
		Topic: r.Topic, Partition: r.Partition, Offset: r.Offset, Timestamp: r.Timestamp.UnixMilli(),
		TimestampType: typ, Key: r.Key, Value: r.Value, Headers: headers,
	}
}

func kafkaEvent(mp *mapping, servers []string, records []*kgo.Record) []byte {
	grouped := map[string][]kafkaEventRecord{}
	for _, r := range records {
		key := fmt.Sprintf("%s-%d", r.Topic, r.Partition)
		grouped[key] = append(grouped[key], kafkaRecord(r))
	}

	source := "aws:kafka"
	if mp.SelfManagedEventSource != nil {
		source = "SelfManagedKafka"
	}

	payload, _ := json.Marshal(map[string]any{
		"eventSource": source, "eventSourceArn": mp.EventSourceArn,
		"bootstrapServers": strings.Join(servers, ","), "records": grouped,
	})
	return payload
}

func (s *Service) consumeKafka(ctx context.Context, scope request.Scope, mp *mapping) error {
	servers, err := s.kafkaEndpoints(ctx, scope, mp)
	if err != nil {
		return err
	}

	group := mp.UUID
	if configured := consumerGroup(mp); configured != nil {
		group = *configured
	}

	offset := kgo.NewOffset().AtStart()
	if mp.StartingPosition == positionLatest {
		offset = kgo.NewOffset().AtEnd()
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(servers...), kgo.ConsumerGroup(group), kgo.ConsumeTopics(mp.Topics...),
		kgo.ConsumeResetOffset(offset), kgo.DisableAutoCommit(), kgo.BlockRebalanceOnPoll(),
	}

	user, password, err := s.brokerCredentials(ctx, scope, mp)
	if err != nil {
		return err
	}

	if len(mp.SourceAccessConfigurations) > 0 {
		switch common.Value(mp.SourceAccessConfigurations[0].Type) {
		case "BASIC_AUTH":
			opts = append(opts, kgo.SASL(plain.Auth{User: user, Pass: password}.AsMechanism()))
		case "SASL_SCRAM_256_AUTH":
			opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: password}.AsSha256Mechanism()))
		case "SASL_SCRAM_512_AUTH":
			opts = append(opts, kgo.SASL(scram.Auth{User: user, Pass: password}.AsSha512Mechanism()))
		}
	}

	client, err := kgo.NewClient(opts...)
	if err != nil {
		return err
	}

	defer client.CloseAllowingRebalance()
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err = client.Ping(pingCtx)
	cancel()
	if err != nil {
		return err
	}

	filters := mp.filters()
	for ctx.Err() == nil {
		fetches := client.PollRecords(ctx, int(mp.BatchSize))
		if err := fetches.Err(); err != nil {
			return err
		}

		all := fetches.Records()
		if len(all) == 0 {
			client.AllowRebalance()
			continue
		}

		if window := mp.batchingWindow(); window > 0 && int64(len(all)) < mp.BatchSize {
			batchCtx, cancel := context.WithTimeout(ctx, window)
			for int64(len(all)) < mp.BatchSize && batchCtx.Err() == nil {
				more := client.PollRecords(batchCtx, int(mp.BatchSize)-len(all))
				all = append(all, more.Records()...)
				if more.Err() != nil {
					break
				}
			}

			cancel()
		}

		var batch []*kgo.Record
		for _, r := range all {
			encoded, _ := json.Marshal(kafkaRecord(r))
			var fields map[string]any
			_ = json.Unmarshal(encoded, &fields)
			for name, raw := range map[string][]byte{"key": r.Key, "value": r.Value} {
				var decoded any
				if json.Unmarshal(raw, &decoded) == nil {
					fields[name] = decoded
				} else {
					fields[name] = string(raw)
				}
			}

			doc, _ := json.Marshal(fields)
			if keep(filters, doc) {
				batch = append(batch, r)
			}
		}

		s.processKafkaBatch(ctx, scope, mp, servers, batch, client)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		commitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := client.CommitRecords(commitCtx, all...)
		cancel()
		if err != nil {
			return err
		}

		client.AllowRebalance()
	}

	return ctx.Err()
}

func (s *Service) invokeBroker(ctx context.Context, scope request.Scope, mp *mapping, payload []byte) bool {
	result, _, _, err := s.run(ctx, scope, mp.FunctionName, mp.Qualifier, payload)
	failure := invokeError(result, err)
	s.deliveries.Add(delivery.ViaEventSourceMapping, mp.EventSourceArn, mp.functionARN(scope), payload, failure)
	outcome := resultOK
	if failure != nil {
		outcome = resultCallFailed
	}

	s.recordResult(ctx, scope, mp.UUID, outcome)
	return failure == nil
}
