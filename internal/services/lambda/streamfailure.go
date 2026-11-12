package lambda

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
)

// A stream mapping gives up on a batch once its retries run out or its
// records are older than MaximumRecordAgeInSeconds, and sends a record of it
// to its OnFailure destination (an SQS queue, SNS topic or S3 bucket). With
// BisectBatchOnFunctionError, a batch the function fails on is retried in
// halves, which does not count against the retries.

const (
	minRecordAge            = 60
	maxRecordAge            = 604800
	conditionStreamRetries  = "RetryAttemptsExhausted"
	conditionRecordAge      = "RecordAgeExceeded"
	fieldDDBStreamBatchInfo = "DDBStreamBatchInfo"
	fieldKinesisBatchInfo   = "KinesisBatchInfo"
	timestampMillis         = "2006-01-02T15:04:05.000Z"
)

// streamOptions sets the failure handling shared by stream and Kafka mappings.
func streamOptions(destinations *api.DestinationConfig, bisect *bool, age *int64, mp *mapping) error {
	if destinations == nil && bisect == nil && age == nil {
		return nil
	}

	if mp.StartingPosition == "" || brokerARN(mp.EventSourceArn, "mq") {
		return invalid("DestinationConfig, BisectBatchOnFunctionError, and MaximumRecordAgeInSeconds are only " +
			"valid for stream event sources")
	}

	if destinations != nil {
		if destinations.OnSuccess != nil {
			return invalid("Stream event sources support only an OnFailure destination")
		}

		destination := common.Value(onFailure(destinations))
		if service, ok := destinationService(destination); destination != "" &&
			(!ok || service != serviceSQS && service != serviceSNS) && !validS3Destination(destination) &&
			(!mp.brokerSource() || !validKafkaDestination(destination)) {
			return invalid("Unsupported OnFailure destination for this event source")
		}

		if strings.HasPrefix(destination, "kafka://") {
			if mp.ProvisionedPollerConfig == nil {
				return invalid("Kafka topic destinations require ProvisionedPollerConfig")
			}

			for _, topic := range mp.Topics {
				if topic == strings.TrimPrefix(destination, "kafka://") {
					return invalid("Kafka destination must differ from source topic")
				}
			}
		}

		mp.OnFailure = destination
	}

	if bisect != nil {
		mp.Bisect = *bisect
	}

	if age != nil {
		if *age != -1 && (*age < minRecordAge || *age > maxRecordAge) {
			return invalid("MaximumRecordAgeInSeconds must be -1 or between %d and %d", minRecordAge, maxRecordAge)
		}

		mp.MaxRecordAge = *age
	}

	return nil
}

// tooOld reports whether a record that arrived at arrival is past the
// mapping's MaximumRecordAgeInSeconds.
func (mp *mapping) tooOld(arrival, now time.Time) bool {
	return mp.MaxRecordAge > 0 && now.Sub(arrival) > time.Duration(mp.MaxRecordAge)*time.Second
}

// nextSize is the batch size after an attempt: half of a batch the function
// failed on when the mapping bisects, and the mapping's batch size otherwise.
// It reports whether the batch was split, which does not count as a retry.
func (mp *mapping) nextSize(size int, failed bool) (int, bool) {
	if failed && mp.Bisect && size > 1 {
		return size / 2, true
	}

	return int(mp.BatchSize), false
}

// batchInfo describes a batch a stream mapping gave up on.
type batchInfo struct {
	ShardID                         string `json:"shardId"`
	StartSequenceNumber             string `json:"startSequenceNumber"`
	EndSequenceNumber               string `json:"endSequenceNumber"`
	ApproximateArrivalOfFirstRecord string `json:"approximateArrivalOfFirstRecord"`
	ApproximateArrivalOfLastRecord  string `json:"approximateArrivalOfLastRecord"`
	StreamArn                       string `json:"streamArn"`
	payload                         []byte
	BatchSize                       int `json:"batchSize"`
}

// failedBatch is the batch info of a stream or Kafka mapping, which also names the shard or partition and holds
// the original event that an S3 destination receives.
type failedBatch interface {
	shard() string
	event() []byte
}

func (b *batchInfo) shard() string { return b.ShardID }

func (b *batchInfo) event() []byte { return b.payload }

// failureKey is where an S3 destination stores a mapping's failure record, as AWS names it:
// aws/lambda/<mapping UUID>/<shard>/YYYY/MM/DD/YYYY-MM-DDTHH.MM.SS-<UUID>.
func failureKey(mp *mapping, shard string, now time.Time) string {
	now = now.UTC()
	return "aws/lambda/" + mp.UUID + "/" + shard + "/" + now.Format("2006/01/02/2006-01-02T15.04.05-") +
		uuid.NewV4().String()
}

// discard sends the record of a batch a stream mapping gave up on to its
// OnFailure destination, if it has one. field names the batch info, which
// differs between DynamoDB and Kinesis streams and Kafka. Only an S3 destination
// also receives the original event, as an escaped JSON string in payload.
func (s *Service) discard(
	ctx context.Context, scope request.Scope, mp *mapping, condition, field string, attempts int, info failedBatch,
) {
	if mp.OnFailure == "" {
		return
	}

	arn := mp.functionARN(scope)
	record := map[string]any{
		"requestContext": map[string]any{
			"requestId": uuid.NewV4().
				String(),
			"functionArn":            arn,
			"condition":              condition,
			"approximateInvokeCount": attempts,
		},
		"responseContext": map[string]any{
			"statusCode": http.StatusOK, "executedVersion": cmp.Or(mp.Qualifier, latest), "functionError": "Unhandled",
		},
		"version": recordVersion, "timestamp": s.now().UTC().Format(timestampMillis), field: info,
	}

	if condition == conditionRecordAge {
		delete(record, "responseContext")
	}

	key := ""
	if validS3Destination(mp.OnFailure) {
		record["payload"], key = string(info.event()), failureKey(mp, info.shard(), s.now())
	}

	body, err := json.Marshal(record)
	if err == nil {
		if info, ok := info.(*kafkaBatchInfo); ok && strings.HasPrefix(mp.OnFailure, "kafka://") {
			err = sendKafkaFailure(ctx, mp, record, info)
		} else {
			err = s.deliver(ctx, mp.OnFailure, arn, condition, body, nil, key)
		}
	}

	s.deliveries.Add(delivery.ViaLambdaDestination, arn, mp.OnFailure, body, err)
	if err != nil {
		s.logger.Warn("sending a stream batch's failure record failed", "function", arn,
			"destination", mp.OnFailure, "error", err)
	}
}

func arrivalText(t time.Time) string { return t.UTC().Format(timestampMillis) }

// retryBatch is shared by stream and Kafka pollers. Splitting does not consume a retry.
func (mp *mapping) retryBatch(failures int64, split bool) bool {
	limit := common.Value(mp.MaximumRetryAttempts)
	if mp.brokerSource() && limit < 0 && mp.OnFailure != "" {
		limit = 10
	}

	return split || limit < 0 || failures <= limit
}
