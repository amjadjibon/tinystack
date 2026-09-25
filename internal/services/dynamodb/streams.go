package dynamodb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	streamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	iteratorLifetime = 15 * time.Minute
	maxStreams       = 100
	maxShards        = 100
	maxRecords       = 1000
	iteratorTrim     = "TRIM_HORIZON"
	iteratorLatest   = "LATEST"
	iteratorAt       = "AT_SEQUENCE_NUMBER"
	iteratorAfter    = "AFTER_SEQUENCE_NUMBER"
)

var _ streamsapi.API = (*Streams)(nil)

// Streams is the DynamoDB Streams API over the streams the tables of a
// DynamoDB service write. Each stream has a single shard.
type Streams struct{ s *Service }

// Streams returns the Streams API of the service's tables.
func (s *Service) Streams() *Streams { return &Streams{s: s} }

func streamNotFound(arn string) error {
	return awserror.New("ResourceNotFoundException", "Requested resource not found: Stream: "+arn+" not found")
}

// streamRef is a stream the caller named, loaded with its extent.
type streamRef struct {
	*stream
	scope request.Scope
	arn   string
	// first is the first record not yet trimmed and last the last record
	// written; first > last when there is none.
	first, last int64
}

func (r *streamRef) status() string {
	if r.Closed != 0 {
		return "DISABLED"
	}

	return "ENABLED"
}

// open loads the caller's stream named by arn.
func (st *Streams) open(ctx context.Context, tx storage.ReadTx, arn string, now time.Time) (*streamRef, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return nil, fmt.Errorf("missing request identity")
	}

	scope, table, label, ok := ParseStreamARN(arn)
	if !ok {
		return nil, awserror.Validation("Invalid StreamArn: %s", arn)
	}

	if scope != m.Scope {
		return nil, streamNotFound(arn)
	}

	s, found, err := loadStream(tx, streamKey(scope, table, label))
	if err != nil {
		return nil, err
	}

	if !found || s.expired(now) {
		return nil, streamNotFound(arn)
	}

	ref := &streamRef{stream: s, scope: scope, arn: arn, last: s.End}
	if s.Closed == 0 {
		t, loadErr := load(tx, m.AccountID+"/"+m.Region+"/dynamodb/"+table)
		if loadErr != nil {
			return nil, loadErr
		}

		ref.last = t.Sequence
	}

	// Records past the retention period that writes have not trimmed yet are
	// gone all the same.
	ref.first = ref.Head
	for ; ref.first <= ref.last; ref.first++ {
		created, found := recordTime(tx, streamRecordKey(scope, table, label, ref.first))
		if found && now.Sub(created) <= streamRetention {
			break
		}
	}

	return ref, nil
}

// view runs fn on the caller's stream named by arn.
func (st *Streams) view(ctx context.Context, arn *string, fn func(storage.ReadTx, *streamRef) error) error {
	if arn == nil {
		return awserror.Validation("StreamArn is required")
	}

	return st.s.store.View(ctx, func(tx storage.ReadTx) error {
		ref, err := st.open(ctx, tx, *arn, time.Now())
		if err != nil {
			return err
		}

		return fn(tx, ref)
	})
}

// limit validates an optional page size.
func limit(p *int64, fallback, most int64) (int64, error) {
	if p == nil {
		return fallback, nil
	}

	if *p < 1 || *p > most {
		return 0, awserror.Validation("Limit must be between 1 and %d", most)
	}

	return *p, nil
}

// ListStreams lists the caller's streams, or those of one table, in table and
// then creation order.
func (st *Streams) ListStreams(
	ctx context.Context,
	in *streamsapi.ListStreamsInput,
) (*streamsapi.ListStreamsOutput, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return nil, fmt.Errorf("missing request identity")
	}

	n, err := limit(in.Limit, maxStreams, maxStreams)
	if err != nil {
		return nil, err
	}

	prefix := m.AccountID + "/" + m.Region + "/dynamodb-stream/"
	if in.TableName != nil {
		if !tableName.MatchString(*in.TableName) {
			return nil, awserror.Validation("TableName must be 3–255 characters using letters, digits, _, -, or .")
		}

		prefix = streamPrefix(m.Scope, *in.TableName)
	}

	start := ""
	if in.ExclusiveStartStreamArn != nil {
		scope, table, label, valid := ParseStreamARN(*in.ExclusiveStartStreamArn)
		if !valid || scope != m.Scope {
			return nil, awserror.Validation("Invalid ExclusiveStartStreamArn: %s", *in.ExclusiveStartStreamArn)
		}

		start = streamKey(scope, table, label)
	}

	out := &streamsapi.ListStreamsOutput{Streams: []streamsapi.Stream{}}
	now := time.Now()
	err = st.s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(prefix) {
			if k <= start {
				continue
			}

			s, _, loadErr := loadStream(tx, k)
			if loadErr != nil {
				return loadErr
			}

			if s.expired(now) {
				continue
			}

			if int64(len(out.Streams)) == n {
				out.LastEvaluatedStreamArn = out.Streams[n-1].StreamArn
				break
			}

			out.Streams = append(out.Streams, streamsapi.Stream{
				StreamArn:   new(streamARN(m.Scope, s.Table, s.Label)),
				TableName:   new(s.Table),
				StreamLabel: new(s.Label),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DescribeStream describes a stream and its one shard.
func (st *Streams) DescribeStream(
	ctx context.Context,
	in *streamsapi.DescribeStreamInput,
) (*streamsapi.DescribeStreamOutput, error) {
	if _, err := limit(in.Limit, maxShards, maxShards); err != nil {
		return nil, err
	}

	if f := in.ShardFilter; f != nil && (value(f.Type) != "CHILD_SHARDS" || value(f.ShardId) == "") {
		return nil, awserror.Validation("ShardFilter needs Type CHILD_SHARDS and a ShardId")
	}

	out := &streamsapi.DescribeStreamOutput{}
	err := st.view(ctx, in.StreamArn, func(_ storage.ReadTx, ref *streamRef) error {
		keySchema, err := convert[[]streamsapi.KeySchemaElement](ref.KeySchema)
		if err != nil {
			return err
		}

		out.StreamDescription = &streamsapi.StreamDescription{
			StreamArn:               new(ref.arn),
			StreamLabel:             new(ref.Label),
			StreamStatus:            new(ref.status()),
			StreamViewType:          new(ref.ViewType),
			CreationRequestDateTime: new(float64(ref.Created) / 1000),
			TableName:               new(ref.Table),
			KeySchema:               keySchema,
			Shards:                  []streamsapi.Shard{},
		}

		// The one shard has no children, and a page after it is empty.
		if in.ShardFilter != nil || value(in.ExclusiveStartShardId) == ref.Shard {
			return nil
		}

		shard := streamsapi.Shard{
			ShardId: new(ref.Shard),
			SequenceNumberRange: &streamsapi.SequenceNumberRange{
				StartingSequenceNumber: new(formatSequence(ref.Start)),
			},
		}

		if ref.Closed != 0 {
			shard.SequenceNumberRange.EndingSequenceNumber = new(formatSequence(max(ref.End, ref.Start)))
		}

		out.StreamDescription.Shards = append(out.StreamDescription.Shards, shard)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// iterator is the decoded form of a shard iterator: the next sequence number
// to read from a stream's shard, and when the iterator was issued.
type iterator struct {
	Stream   string `json:"s"`
	Shard    string `json:"h"`
	Position int64  `json:"p"`
	Issued   int64  `json:"t"`
}

func (it iterator) encode() *string {
	data, _ := json.Marshal(it)
	return new(base64.RawURLEncoding.EncodeToString(data))
}

func decodeIterator(raw *string) (iterator, error) {
	var it iterator
	data, err := base64.RawURLEncoding.DecodeString(value(raw))
	if err != nil || json.Unmarshal(data, &it) != nil || it.Stream == "" {
		return it, awserror.Validation("Invalid ShardIterator")
	}

	return it, nil
}

func trimmed() error {
	return awserror.New("TrimmedDataAccessException",
		"The operation attempted to read past the oldest stream record in a shard.")
}

// parseSequence validates a sequence number.
func parseSequence(raw *string) (int64, error) {
	s := value(raw)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || len(s) > 40 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, awserror.Validation("Invalid SequenceNumber: %s", s)
	}

	return n, nil
}

// GetShardIterator returns an iterator at the start of the shard's retained
// records, after its last record, or at or after a sequence number.
func (st *Streams) GetShardIterator(
	ctx context.Context,
	in *streamsapi.GetShardIteratorInput,
) (*streamsapi.GetShardIteratorOutput, error) {
	kind := value(in.ShardIteratorType)
	switch kind {
	case iteratorTrim, iteratorLatest:
		if in.SequenceNumber != nil {
			return nil, awserror.Validation("SequenceNumber is only valid for AT_SEQUENCE_NUMBER and " +
				"AFTER_SEQUENCE_NUMBER iterators")
		}
	case iteratorAt, iteratorAfter:
		if in.SequenceNumber == nil {
			return nil, awserror.Validation("SequenceNumber is required for %s iterators", kind)
		}
	default:
		return nil, awserror.Validation("ShardIteratorType must be TRIM_HORIZON, LATEST, AT_SEQUENCE_NUMBER, or " +
			"AFTER_SEQUENCE_NUMBER")
	}

	now := time.Now()
	out := &streamsapi.GetShardIteratorOutput{}
	err := st.view(ctx, in.StreamArn, func(_ storage.ReadTx, ref *streamRef) error {
		if value(in.ShardId) != ref.Shard {
			return awserror.New("ResourceNotFoundException",
				"Requested resource not found: Shard does not exist")
		}

		position := ref.first
		switch kind {
		case iteratorLatest:
			position = ref.last + 1
		case iteratorAt, iteratorAfter:
			n, err := parseSequence(in.SequenceNumber)
			if err != nil {
				return err
			}

			if n < ref.Start || n > ref.last {
				return awserror.Validation("Invalid SequenceNumber: %s is not in the shard", *in.SequenceNumber)
			}

			if n < ref.first {
				return trimmed()
			}

			if position = n; kind == iteratorAfter {
				position++
			}
		}

		out.ShardIterator = iterator{Stream: ref.arn, Shard: ref.Shard, Position: position, Issued: now.UnixMilli()}.
			encode()
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// GetRecords reads the records at an iterator. NextShardIterator is left out
// once a closed shard has no more records.
func (st *Streams) GetRecords(
	ctx context.Context,
	in *streamsapi.GetRecordsInput,
) (*streamsapi.GetRecordsOutput, error) {
	n, err := limit(in.Limit, maxRecords, maxRecords)
	if err != nil {
		return nil, err
	}

	it, err := decodeIterator(in.ShardIterator)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if now.Sub(time.UnixMilli(it.Issued)) > iteratorLifetime {
		return nil, awserror.New("ExpiredIteratorException", "Iterator expired. The iterator was created at "+
			time.UnixMilli(it.Issued).UTC().Format(time.RFC3339)+" but is only valid for 15 minutes")
	}

	out := &streamsapi.GetRecordsOutput{Records: []streamsapi.Record{}}
	err = st.view(ctx, &it.Stream, func(tx storage.ReadTx, ref *streamRef) error {
		if it.Shard != ref.Shard {
			return awserror.Validation("Invalid ShardIterator")
		}

		if it.Position < ref.first {
			return trimmed()
		}

		for ; it.Position <= ref.last && int64(len(out.Records)) < n; it.Position++ {
			if record, found := readRecord(tx, streamRecordKey(ref.scope, ref.Table, ref.Label, it.Position)); found {
				out.Records = append(out.Records, *record)
			}
		}

		if ref.Closed == 0 || it.Position <= ref.End {
			it.Issued = now.UnixMilli()
			out.NextShardIterator = it.encode()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Latest returns the sequence number of the last record in the caller's
// stream named by arn, or "" when it has none, for readers that start after
// it.
func (st *Streams) Latest(ctx context.Context, arn string) (string, error) {
	var latest string
	err := st.view(ctx, &arn, func(_ storage.ReadTx, ref *streamRef) error {
		if ref.last >= ref.Start {
			latest = formatSequence(ref.last)
		}

		return nil
	})

	return latest, err
}
