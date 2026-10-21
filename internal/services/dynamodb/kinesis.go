package dynamodb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	streamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// A Kinesis streaming destination copies every change to a table's items into
// a Kinesis data stream of the same account and Region, whether or not the
// table has a DynamoDB stream. A table has at most two destinations that are
// not disabled, and one per stream. TinyStack turns a destination on at once:
// Enable reports ENABLING and Describe ACTIVE, and likewise DISABLING and
// DISABLED, UPDATING and ACTIVE.
//
// Each change is delivered after the transaction that made it commits, once
// per ACTIVE destination, as a record of the JSON form DynamoDB writes, with
// the SHA-256 of the item's key, in hex, as the partition key, so that every
// change to an item reaches one shard. A change to the same item made at the
// same time can reach the stream in either order. If delivery fails the item
// write stands; when the stream no longer exists, the destination becomes
// ENABLE_FAILED, and enabling it again, once the stream exists, replaces it.

const (
	destinationActive   = "ACTIVE"
	destinationDisabled = "DISABLED"
	destinationFailed   = "ENABLE_FAILED"
	precisionMillis     = "MILLISECOND"
	precisionMicros     = "MICROSECOND"
	// maxDestinations is how many destinations a table can have at once.
	maxDestinations = 2
)

// destination is a table's Kinesis streaming destination.
type destination struct {
	StreamARN   string
	Status      string
	Description string `json:",omitempty"`
	Precision   string
}

// UseKinesis lets tables stream to Kinesis. Call it before serving requests.
func (s *Service) UseKinesis(streams *kinesis.Service) { s.kinesis = streams }

// kinesisTarget checks that arn names a Kinesis stream in the caller's account
// and Region.
func kinesisTarget(ctx context.Context, arn *string) (string, error) {
	scope, err := scopeOf(ctx)
	if err != nil {
		return "", err
	}

	streamScope, _, ok := kinesis.ParseStreamARN(common.Value(arn))
	if !ok || streamScope != scope {
		return "", awserror.Validation("StreamArn must name a Kinesis data stream in this account and Region: %s",
			common.Value(arn))
	}

	return *arn, nil
}

// precisionOf validates a timestamp precision, which defaults to MILLISECOND.
func precisionOf(p *string) (string, error) {
	precision := precisionMillis
	if p != nil {
		precision = *p
	}

	if precision != precisionMillis && precision != precisionMicros {
		return "", awserror.Validation("ApproximateCreationDateTimePrecision must be MILLISECOND or MICROSECOND")
	}

	return precision, nil
}

func (t *table) destination(arn string) int {
	return slices.IndexFunc(t.Destinations, func(d destination) bool { return d.StreamARN == arn })
}

func (t *table) activeDestinations() int {
	n := 0
	for _, d := range t.Destinations {
		if d.Status == destinationActive {
			n++
		}
	}

	return n
}

// changeDestination loads the table, hands fn its destination for arn (or -1)
// and saves the table when fn succeeds.
func (s *Service) changeDestination(
	ctx context.Context,
	nameOrARN *string,
	fn func(t *table) error,
) (name string, err error) {
	key, err := tableNamed(ctx, nameOrARN)
	if err != nil {
		return "", err
	}

	_, name = tableScope(key)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(t); fnErr != nil {
			return fnErr
		}

		return save(tx, key, t)
	})

	return name, err
}

func destinationNotFound(arn string) error {
	return awserror.New("ResourceNotFoundException",
		"Requested resource not found: no Kinesis streaming destination "+arn)
}

// EnableKinesisStreamingDestination streams a table's changes to an existing
// Kinesis data stream.
func (s *Service) EnableKinesisStreamingDestination(
	ctx context.Context,
	in *api.KinesisStreamingDestinationInput,
) (*api.KinesisStreamingDestinationOutput, error) {
	arn, err := kinesisTarget(ctx, in.StreamArn)
	if err != nil {
		return nil, err
	}

	var precision *string
	if in.EnableKinesisStreamingConfiguration != nil {
		precision = in.EnableKinesisStreamingConfiguration.ApproximateCreationDateTimePrecision
	}

	precisionName, err := precisionOf(precision)
	if err != nil {
		return nil, err
	}

	if _, shardsErr := s.kinesis.Shards(ctx, arn); shardsErr != nil {
		if isCode(shardsErr, "ResourceNotFoundException") {
			return nil, awserror.New("ResourceNotFoundException",
				"Requested resource not found: Stream: "+arn+" not found")
		}

		return nil, shardsErr
	}

	name, err := s.changeDestination(ctx, in.TableName, func(t *table) error {
		i := t.destination(arn)
		if i >= 0 && t.Destinations[i].Status == destinationActive {
			return awserror.New("ResourceInUseException", "Table is already streaming to "+arn)
		}

		if t.activeDestinations() >= maxDestinations {
			return awserror.New("LimitExceededException",
				fmt.Sprintf("Table can stream to at most %d Kinesis data streams", maxDestinations))
		}

		d := destination{StreamARN: arn, Status: destinationActive, Precision: precisionName}
		if i < 0 {
			t.Destinations = append(t.Destinations, d)
		} else {
			t.Destinations[i] = d
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.KinesisStreamingDestinationOutput{
		TableName:         &name,
		StreamArn:         &arn,
		DestinationStatus: new("ENABLING"),
		EnableKinesisStreamingConfiguration: &api.EnableKinesisStreamingConfiguration{
			ApproximateCreationDateTimePrecision: &precisionName,
		},
	}, nil
}

// DisableKinesisStreamingDestination stops streaming a table's changes to a
// stream. The destination stays listed as DISABLED.
func (s *Service) DisableKinesisStreamingDestination(
	ctx context.Context,
	in *api.KinesisStreamingDestinationInput,
) (*api.KinesisStreamingDestinationOutput, error) {
	if in.EnableKinesisStreamingConfiguration != nil {
		return nil, awserror.Validation("EnableKinesisStreamingConfiguration cannot be used to disable a destination")
	}

	arn, err := kinesisTarget(ctx, in.StreamArn)
	if err != nil {
		return nil, err
	}

	name, err := s.changeDestination(ctx, in.TableName, func(t *table) error {
		i := t.destination(arn)
		if i < 0 {
			return destinationNotFound(arn)
		}

		if t.Destinations[i].Status == destinationDisabled {
			return awserror.New("ResourceInUseException", "Table is not streaming to "+arn)
		}

		t.Destinations[i].Status, t.Destinations[i].Description = destinationDisabled, ""
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.KinesisStreamingDestinationOutput{
		TableName: &name, StreamArn: &arn, DestinationStatus: new("DISABLING"),
	}, nil
}

// UpdateKinesisStreamingDestination changes the timestamp precision of an
// active destination.
func (s *Service) UpdateKinesisStreamingDestination(
	ctx context.Context,
	in *api.UpdateKinesisStreamingDestinationInput,
) (*api.UpdateKinesisStreamingDestinationOutput, error) {
	arn, err := kinesisTarget(ctx, in.StreamArn)
	if err != nil {
		return nil, err
	}

	if in.UpdateKinesisStreamingConfiguration == nil ||
		in.UpdateKinesisStreamingConfiguration.ApproximateCreationDateTimePrecision == nil {
		return nil, awserror.Validation("UpdateKinesisStreamingConfiguration.ApproximateCreationDateTimePrecision " +
			"is required")
	}

	precision, err := precisionOf(in.UpdateKinesisStreamingConfiguration.ApproximateCreationDateTimePrecision)
	if err != nil {
		return nil, err
	}

	name, err := s.changeDestination(ctx, in.TableName, func(t *table) error {
		i := t.destination(arn)
		if i < 0 {
			return destinationNotFound(arn)
		}

		if t.Destinations[i].Status != destinationActive {
			return awserror.New("ResourceInUseException", "Table is not actively streaming to "+arn)
		}

		t.Destinations[i].Precision = precision
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateKinesisStreamingDestinationOutput{
		TableName:         &name,
		StreamArn:         &arn,
		DestinationStatus: new("UPDATING"),
		UpdateKinesisStreamingConfiguration: &api.UpdateKinesisStreamingConfiguration{
			ApproximateCreationDateTimePrecision: &precision,
		},
	}, nil
}

// DescribeKinesisStreamingDestination lists a table's destinations, disabled
// ones included.
func (s *Service) DescribeKinesisStreamingDestination(
	ctx context.Context,
	in *api.DescribeKinesisStreamingDestinationInput,
) (*api.DescribeKinesisStreamingDestinationOutput, error) {
	key, err := tableNamed(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	_, name := tableScope(key)
	out := &api.DescribeKinesisStreamingDestinationOutput{TableName: &name}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		for _, d := range t.Destinations {
			listed := api.KinesisDataStreamDestination{
				StreamArn:                            new(d.StreamARN),
				DestinationStatus:                    new(d.Status),
				ApproximateCreationDateTimePrecision: new(d.Precision),
			}
			if d.Description != "" {
				listed.DestinationStatusDescription = new(d.Description)
			}

			out.KinesisDataStreamDestinations = append(out.KinesisDataStreamDestinations, listed)
		}

		return nil
	})

	return out, err
}

// kinesisPut is a change bound for one Kinesis stream.
type kinesisPut struct {
	table, stream, partitionKey string
	data                        []byte
}

// outbox collects the Kinesis records of the item changes of one request while
// its transaction runs, for deliver to send once it commits, and the write
// capacity the changes consumed.
type outbox struct {
	used tally
	puts []kinesisPut
}

// kinesisRecord is the JSON a Kinesis record holds for a change.
type kinesisRecord struct {
	AwsRegion    string          `json:"awsRegion"`
	EventID      string          `json:"eventID"`
	EventName    string          `json:"eventName"`
	UserIdentity *kinesisSubject `json:"userIdentity"`
	RecordFormat string          `json:"recordFormat"`
	TableName    string          `json:"tableName"`
	EventSource  string          `json:"eventSource"`
	Dynamodb     kinesisChange   `json:"dynamodb"`
}

type kinesisSubject struct {
	Type        string `json:"type"`
	PrincipalID string `json:"principalId"`
}

type kinesisChange struct {
	Keys     map[string]streamsapi.AttributeValue `json:"Keys"`
	NewImage map[string]streamsapi.AttributeValue `json:"NewImage,omitempty"`
	OldImage map[string]streamsapi.AttributeValue `json:"OldImage,omitempty"`
	// ApproximateCreationDateTime is in the destination's precision since the
	// Unix epoch.
	ApproximateCreationDateTime int64 `json:"ApproximateCreationDateTime"`
	SizeBytes                   int64 `json:"SizeBytes"`
}

// add queues the change from old to it (either nil) of the item stored at
// record, as a record for each of the table's active destinations. by is the
// service that made the change, if any.
func (o *outbox) add(key, record string, t *table, old, it item, by *streamsapi.Identity) error {
	if t.activeDestinations() == 0 || reflect.DeepEqual(old, it) {
		return nil
	}

	scope, name := tableScope(key)
	event, keys := changeOf(t, old, it)
	var size int64
	for _, image := range []item{keys, old, it} {
		n, err := itemSize(image)
		if err != nil {
			return err
		}

		size += n
	}

	body := kinesisRecord{
		AwsRegion: scope.Region, EventName: event, RecordFormat: "application/json", TableName: name,
		Dynamodb: kinesisChange{SizeBytes: size}, EventSource: "aws:dynamodb",
	}
	if by != nil {
		body.UserIdentity = &kinesisSubject{Type: common.Value(by.Type), PrincipalID: common.Value(by.PrincipalId)}
	}

	var err error
	if body.Dynamodb.Keys, err = convert[map[string]streamsapi.AttributeValue](keys); err != nil {
		return err
	}

	if it != nil {
		if body.Dynamodb.NewImage, err = convert[map[string]streamsapi.AttributeValue](it); err != nil {
			return err
		}
	}

	if old != nil {
		if body.Dynamodb.OldImage, err = convert[map[string]streamsapi.AttributeValue](old); err != nil {
			return err
		}
	}

	var id [16]byte
	_, _ = rand.Read(id[:])
	body.EventID = hex.EncodeToString(id[:])
	digest := sha256.Sum256([]byte(strings.TrimPrefix(record, itemRecord(key, ""))))
	partitionKey := hex.EncodeToString(digest[:])
	now := time.Now()
	for _, d := range t.Destinations {
		if d.Status != destinationActive {
			continue
		}

		body.Dynamodb.ApproximateCreationDateTime = now.UnixMilli()
		if d.Precision == precisionMicros {
			body.Dynamodb.ApproximateCreationDateTime = now.UnixMicro()
		}

		data, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return fmt.Errorf("encode Kinesis record: %w", marshalErr)
		}

		o.puts = append(o.puts, kinesisPut{table: key, stream: d.StreamARN, partitionKey: partitionKey, data: data})
	}

	return nil
}

// deliver puts the queued records into their streams. A failure never fails
// the write: it is logged, and a stream that does not exist fails its
// destination.
func (s *Service) deliver(ctx context.Context, out *outbox) {
	if len(out.puts) == 0 {
		return
	}

	// The items are written; a client that gave up has not undone that. The
	// outbox is emptied, so a caller that shares it across writes, such as a
	// PartiQL batch, delivers each change once.
	ctx = context.WithoutCancel(ctx)
	puts := out.puts
	out.puts = nil
	for _, p := range puts {
		err := s.kinesis.Put(ctx, p.stream, p.data, p.partitionKey)
		if err == nil {
			continue
		}

		s.logger.Warn("deliver DynamoDB change to Kinesis", "table", p.table, "stream", p.stream, "error", err)
		if isCode(err, "ResourceNotFoundException") {
			s.failDestination(ctx, p.table, p.stream)
		}
	}
}

// failDestination marks the active destination of the table stored at key as
// failed, because its stream is gone.
func (s *Service) failDestination(ctx context.Context, key, arn string) {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		i := t.destination(arn)
		if i < 0 || t.Destinations[i].Status != destinationActive {
			return nil
		}

		t.Destinations[i].Status, t.Destinations[i].Description = destinationFailed, "Kinesis data stream not found"
		return save(tx, key, t)
	})
	if err != nil {
		s.logger.Warn("fail DynamoDB Kinesis destination", "table", key, "stream", arn, "error", err)
	}
}
