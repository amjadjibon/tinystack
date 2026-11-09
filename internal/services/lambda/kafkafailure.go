package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
)

var kafkaDestination = regexp.MustCompile(`^kafka://[^.]([a-zA-Z0-9\-_.]{0,248})$`)

func validKafkaDestination(destination string) bool {
	if kafkaDestination.MatchString(destination) {
		return true
	}

	parts := strings.SplitN(destination, ":", 6)
	return len(parts) == 6 && strings.HasPrefix(destination, "arn:") && strings.HasPrefix(parts[1], "aws") &&
		parts[2] == "s3" && parts[3] == "" && parts[4] == "" && parts[5] != "" && !strings.Contains(parts[5], "/")
}

func kafkaProvisioned(mp *mapping, config *api.ProvisionedPollerConfig, required bool) error {
	if config != nil {
		minimum, maximum := int64(1), int64(200)
		if mp.ProvisionedPollerConfig != nil {
			minimum = common.Value(mp.ProvisionedPollerConfig.MinimumPollers)
			maximum = common.Value(mp.ProvisionedPollerConfig.MaximumPollers)
		}

		if config.MinimumPollers != nil {
			minimum = *config.MinimumPollers
		}

		if config.MaximumPollers != nil {
			maximum = *config.MaximumPollers
		}

		if minimum < 1 || minimum > 200 || maximum < 1 || maximum > 2000 || minimum > maximum {
			return invalid(
				"Kafka MinimumPollers must be 1-200, MaximumPollers 1-2000, and minimum must not exceed maximum",
			)
		}

		mp.ProvisionedPollerConfig = &api.ProvisionedPollerConfig{
			MinimumPollers: new(minimum),
			MaximumPollers: new(maximum),
		}
	}

	if required && mp.ProvisionedPollerConfig == nil {
		return invalid("Kafka retry controls and FunctionResponseTypes require ProvisionedPollerConfig")
	}

	return nil
}

// kafkaFailures uses AWS's topic-partition string and integral offset identifier.
// Invalid identifiers fail the entire batch, as they do for streams and queues.
func kafkaFailures(payload []byte, records []*kgo.Record) ([]*kgo.Record, bool) {
	var response struct {
		Failures []struct {
			ID struct {
				Partition *string `json:"partition"`
				Offset    *int64  `json:"offset"`
			} `json:"itemIdentifier"`
		} `json:"batchItemFailures"`
	}

	if len(payload) == 0 {
		return nil, true
	}

	if json.Unmarshal(payload, &response) != nil {
		return records, false
	}

	ids := map[string]bool{}
	for _, item := range response.Failures {
		if item.ID.Partition == nil || item.ID.Offset == nil {
			return records, false
		}

		ids[fmt.Sprintf("%s:%d", *item.ID.Partition, *item.ID.Offset)] = true
	}

	var failed []*kgo.Record
	for _, r := range records {
		id := fmt.Sprintf("%s-%d:%d", r.Topic, r.Partition, r.Offset)
		if ids[id] {
			failed = append(failed, r)
			delete(ids, id)
		}
	}

	if len(ids) != 0 {
		return records, false
	}

	return failed, true
}

// KafkaBatchInfo follows the invocation record documented in kafka-on-failure.
type kafkaBatchInfo struct {
	RecordsInfo      map[string]*kafkaPartitionInfo `json:"recordsInfo"`
	client           *kgo.Client
	EventSourceArn   string `json:"eventSourceArn"`
	BootstrapServers string `json:"bootstrapServers"`
	payload          []byte
	records          []*kgo.Record
	BatchSize        int `json:"batchSize"`
	PayloadSize      int `json:"payloadSize"`
}

type kafkaPartitionInfo struct {
	FirstRecordOffset    string `json:"firstRecordOffset"`
	LastRecordOffset     string `json:"lastRecordOffset"`
	FirstRecordTimestamp string `json:"firstRecordTimestamp"`
	LastRecordTimestamp  string `json:"lastRecordTimestamp"`
}

func kafkaInfo(mp *mapping, servers []string, records []*kgo.Record, client *kgo.Client) *kafkaBatchInfo {
	payload := kafkaEvent(mp, servers, records)
	info := &kafkaBatchInfo{
		records:     records,
		RecordsInfo: map[string]*kafkaPartitionInfo{}, EventSourceArn: mp.EventSourceArn,
		BootstrapServers: strings.Join(servers, ","), BatchSize: len(records), PayloadSize: len(payload),
		payload: payload, client: client,
	}

	for _, r := range records {
		key := fmt.Sprintf("%s-%d", r.Topic, r.Partition)
		part := info.RecordsInfo[key]
		if part == nil {
			part = &kafkaPartitionInfo{
				FirstRecordOffset:    strconv.FormatInt(r.Offset, 10),
				FirstRecordTimestamp: arrivalText(r.Timestamp),
			}

			info.RecordsInfo[key] = part
		}

		part.LastRecordOffset, part.LastRecordTimestamp = strconv.FormatInt(r.Offset, 10), arrivalText(r.Timestamp)
	}

	return info
}

// processKafkaBatch resolves a fetched batch before its offsets are committed.
// Age, retry budgets, splitting and the destination envelope are shared with streams.
func (s *Service) processKafkaBatch(ctx context.Context, scope request.Scope, mp *mapping,
	servers []string, records []*kgo.Record, client *kgo.Client,
) {
	attempts := int64(0)
	for len(records) > 0 && ctx.Err() == nil {
		var live, expired []*kgo.Record
		for _, r := range records {
			if mp.tooOld(r.Timestamp, s.now()) {
				expired = append(expired, r)
			} else {
				live = append(live, r)
			}
		}

		if len(expired) > 0 {
			s.discard(
				ctx,
				scope,
				mp,
				conditionRecordAge,
				"KafkaBatchInfo",
				int(attempts),
				kafkaInfo(mp, servers, expired, client),
			)
		}

		records = live
		if len(records) == 0 {
			return
		}

		payload := kafkaEvent(mp, servers, records)
		result, _, _, err := s.run(ctx, scope, mp.FunctionName, mp.Qualifier, payload)
		failure := invokeError(result, err)
		s.deliveries.Add(delivery.ViaEventSourceMapping, mp.EventSourceArn, mp.functionARN(scope), payload, failure)
		attempts++
		failed, valid := records, false
		if failure == nil {
			failed, valid = nil, true
			if mp.ReportFailures {
				failed, valid = kafkaFailures(result.Payload, records)
			}
		}

		outcome := resultOK
		if len(failed) > 0 {
			outcome = resultCallFailed
		}

		s.recordResult(ctx, scope, mp.UUID, outcome)
		if len(failed) == 0 {
			return
		}

		if size, split := mp.nextSize(len(failed), !valid); split {
			s.processKafkaBatch(ctx, scope, mp, servers, failed[:size], client)
			s.processKafkaBatch(ctx, scope, mp, servers, failed[size:], client)
			return
		}

		if !mp.retryBatch(attempts, false) {
			s.discard(
				ctx,
				scope,
				mp,
				conditionStreamRetries,
				"KafkaBatchInfo",
				int(attempts),
				kafkaInfo(mp, servers, failed, client),
			)
			return
		}

		records = failed
		sleep(ctx, pollBackoff)
	}
}

// Kafka topic destinations receive one envelope per original record, preserving its key.
func sendKafkaFailure(ctx context.Context, mp *mapping, record map[string]any, info *kafkaBatchInfo) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, r := range info.records {
		record["payload"] = json.RawMessage(kafkaEvent(mp, strings.Split(info.BootstrapServers, ","), []*kgo.Record{r}))
		record["KafkaBatchInfo"] = kafkaTopicBatchInfo{
			EventSourceArn:   info.EventSourceArn,
			BootstrapServers: info.BootstrapServers,
			BatchSize:        1,
			PayloadSize:      info.PayloadSize,
			RecordInfo: kafkaTopicRecordInfo{
				Offset:    strconv.FormatInt(r.Offset, 10),
				Timestamp: arrivalText(r.Timestamp),
			},
		}

		body, err := json.Marshal(record)
		if err != nil {
			return err
		}

		if err := info.client.ProduceSync(ctx, &kgo.Record{
			Topic: strings.TrimPrefix(mp.OnFailure, "kafka://"),
			Key:   r.Key, Value: body,
		}).FirstErr(); err != nil {
			return err
		}
	}

	return nil
}

type kafkaTopicBatchInfo struct {
	EventSourceArn   string               `json:"eventSourceArn"`
	BootstrapServers string               `json:"bootstrapServers"`
	RecordInfo       kafkaTopicRecordInfo `json:"recordInfo"`
	BatchSize        int                  `json:"batchSize"`
	PayloadSize      int                  `json:"payloadSize"`
}

type kafkaTopicRecordInfo struct {
	Offset    string `json:"offset"`
	Timestamp string `json:"timestamp"`
}
