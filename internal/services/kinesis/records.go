package kinesis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxGetRecords = 10000
	maxGetBytes   = 10 << 20
)

// append stores a record in the shard its keys route it to, deleting the
// shard's records past the retention period, and returns its sequence
// number.
func (s *Service) append(tx storage.WriteTx, scope request.Scope, st *stream, data []byte, partitionKey,
	explicit string,
) (*shard, string, error) {
	if len(data)+len(partitionKey) > maxRecord {
		return nil, "", invalidArgument("A record's data and partition key must be at most 1 MiB")
	}

	sh, err := st.route(partitionKey, explicit)
	if err != nil {
		return nil, "", err
	}

	now := s.now()
	cutoff := common.Unix(now.Add(-time.Duration(st.Retention) * time.Hour))
	keyPrefix := recordPrefix(scope, st.Name, sh.ID)
	for _, k := range tx.Keys(keyPrefix) {
		r, _, readErr := storage.Read[record](tx, k)
		if readErr != nil {
			return nil, "", readErr
		}

		if r.Arrival >= cutoff {
			break
		}

		tx.Delete(k)
	}

	sh.Next++
	seq := sequence(sh.Index, sh.Next)
	r := &record{Data: data, PartitionKey: partitionKey, Sequence: seq, Arrival: common.Unix(now)}
	return sh, seq, storage.Write(tx, keyPrefix+seq, r)
}

func (s *Service) PutRecord(ctx context.Context, in *api.PutRecordInput) (*api.PutRecordOutput, error) {
	out := &api.PutRecordOutput{}
	err := s.change(ctx, in.StreamName, in.StreamARN, func(tx storage.WriteTx, scope request.Scope, st *stream) error {
		sh, seq, err := s.append(
			tx,
			scope,
			st,
			in.Data,
			common.Value(in.PartitionKey),
			common.Value(in.ExplicitHashKey),
		)
		if err != nil {
			return err
		}

		out.ShardId, out.SequenceNumber, out.EncryptionType = new(sh.ID), new(seq), new(st.Encryption)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// PutRecords stores up to 500 records in one transaction. A record that
// cannot be stored is reported in its entry and does not stop the others.
func (s *Service) PutRecords(ctx context.Context, in *api.PutRecordsInput) (*api.PutRecordsOutput, error) {
	if n := len(in.Records); n < 1 || n > maxBatch {
		return nil, invalidArgument("Records must hold 1 to %d records", maxBatch)
	}

	size := 0
	for _, r := range in.Records {
		size += len(r.Data) + len(common.Value(r.PartitionKey))
	}

	if size > 5*maxRecord {
		return nil, invalidArgument("A PutRecords request must be at most 5 MiB")
	}

	out := &api.PutRecordsOutput{FailedRecordCount: new(int64(0))}
	err := s.change(ctx, in.StreamName, in.StreamARN, func(tx storage.WriteTx, scope request.Scope, st *stream) error {
		out.Records, *out.FailedRecordCount = nil, 0
		out.EncryptionType = new(st.Encryption)
		for _, r := range in.Records {
			sh, seq, err := s.append(
				tx,
				scope,
				st,
				r.Data,
				common.Value(r.PartitionKey),
				common.Value(r.ExplicitHashKey),
			)
			var ae *awserror.Error
			if ok := asAWS(err, &ae); ok {
				*out.FailedRecordCount++
				out.Records = append(out.Records, api.PutRecordsResultEntry{
					ErrorCode: new(ae.Code), ErrorMessage: new(ae.Message),
				})
				continue
			}

			if err != nil {
				return err
			}

			out.Records = append(out.Records, api.PutRecordsResultEntry{ShardId: new(sh.ID), SequenceNumber: new(seq)})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func asAWS(err error, target **awserror.Error) bool {
	ae, ok := err.(*awserror.Error) //nolint:errorlint // append returns awserror values directly.
	*target = ae
	return ok
}

// iterator is what a shard iterator encodes: a position in a shard, after
// the sequence number After ("" before the first record).
type iterator struct {
	Account string
	Region  string
	Stream  string
	Shard   string
	After   string
	Expires int64
}

func (it *iterator) encode() *string {
	data, _ := json.Marshal(it)
	return new(base64.RawURLEncoding.EncodeToString(data))
}

func decodeIterator(s string) (*iterator, bool) {
	data, err := base64.RawURLEncoding.DecodeString(s)
	it := &iterator{}
	return it, err == nil && json.Unmarshal(data, it) == nil && it.Stream != ""
}

// inShard reports whether a sequence number is one of a shard's.
func inShard(sh *shard, seq string) bool {
	return len(seq) == len(sequence(0, 0)) && strings.HasPrefix(seq, sequence(sh.Index, 0)[:12])
}

func (s *Service) GetShardIterator(
	ctx context.Context,
	in *api.GetShardIteratorInput,
) (*api.GetShardIteratorOutput, error) {
	var out *api.GetShardIteratorOutput
	err := s.view(ctx, in.StreamName, in.StreamARN, func(tx storage.ReadTx, scope request.Scope, st *stream) error {
		sh := st.shard(common.Value(in.ShardId))
		if sh == nil {
			return awserror.New("ResourceNotFoundException", "Shard "+common.Value(in.ShardId)+" in stream "+st.Name+
				" under account "+scope.AccountID+" does not exist")
		}

		it := &iterator{
			Account: scope.AccountID, Region: scope.Region, Stream: st.Name, Shard: sh.ID,
			Expires: s.now().Add(iteratorLife).UnixMilli(),
		}
		seq := common.Value(in.StartingSequenceNumber)
		switch common.Value(in.ShardIteratorType) {
		case "TRIM_HORIZON":
		case "LATEST":
			it.After = sequence(sh.Index, sh.Next)
		case "AT_SEQUENCE_NUMBER", "AFTER_SEQUENCE_NUMBER":
			if !inShard(sh, seq) {
				return invalidArgument("StartingSequenceNumber %s is not in shard %s", seq, sh.ID)
			}

			it.After = seq
			if common.Value(in.ShardIteratorType) == "AT_SEQUENCE_NUMBER" {
				it.After = previous(seq)
			}
		case "AT_TIMESTAMP":
			if in.Timestamp == nil {
				return invalidArgument("AT_TIMESTAMP needs a Timestamp")
			}

			for _, k := range tx.Keys(recordPrefix(scope, st.Name, sh.ID)) {
				r, _, readErr := storage.Read[record](tx, k)
				if readErr != nil {
					return readErr
				}

				if r.Arrival >= *in.Timestamp {
					break
				}

				it.After = r.Sequence
			}
		default:
			return invalidArgument("ShardIteratorType must be AT_SEQUENCE_NUMBER, AFTER_SEQUENCE_NUMBER, " +
				"TRIM_HORIZON, LATEST, or AT_TIMESTAMP")
		}

		out = &api.GetShardIteratorOutput{ShardIterator: it.encode()}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// previous is the sequence number just before seq, which sorts after every
// earlier record of the shard.
func previous(seq string) string {
	b := []byte(seq)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] > '0' {
			b[i]--
			break
		}

		b[i] = '9'
	}

	return string(b)
}

// Record is a data record, for in-process readers.
type Record struct {
	Arrival        time.Time
	PartitionKey   string
	SequenceNumber string
	Data           []byte
}

// readShard reads up to limit records of a shard after the sequence number
// after, skipping records past the retention period. It reports whether
// the shard is closed and read to its end, and the arrival time of the
// first record left unread.
func (s *Service) readShard(tx storage.ReadTx, scope request.Scope, st *stream, sh *shard, after string,
	limit int,
) (records []Record, done bool, behind *float64, err error) {
	cutoff := common.Unix(s.now().Add(-time.Duration(st.Retention) * time.Hour))
	size := 0
	for _, k := range tx.Keys(recordPrefix(scope, st.Name, sh.ID)) {
		r, _, readErr := storage.Read[record](tx, k)
		if readErr != nil {
			return nil, false, nil, readErr
		}

		if r.Sequence <= after || r.Arrival < cutoff {
			continue
		}

		if len(records) == limit || size+len(r.Data) > maxGetBytes {
			return records, false, new(r.Arrival), nil
		}

		size += len(r.Data)
		records = append(records, Record{
			Data: r.Data, PartitionKey: r.PartitionKey, SequenceNumber: r.Sequence,
			Arrival: time.UnixMilli(int64(r.Arrival * 1000)),
		})
	}

	return records, sh.Closed, nil, nil
}

func (s *Service) GetRecords(ctx context.Context, in *api.GetRecordsInput) (*api.GetRecordsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit := maxGetRecords
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > maxGetRecords {
			return nil, invalidArgument("Limit must be between 1 and %d", maxGetRecords)
		}

		limit = int(*in.Limit)
	}

	it, ok := decodeIterator(common.Value(in.ShardIterator))
	switch {
	case !ok || it.Account != m.AccountID || it.Region != m.Region:
		return nil, invalidArgument("Invalid ShardIterator")
	case s.now().UnixMilli() > it.Expires:
		return nil, awserror.New("ExpiredIteratorException", "Iterator expired. The iterator was created at time "+
			time.UnixMilli(it.Expires-iteratorLife.Milliseconds()).UTC().Format(time.RFC3339)+
			" while right now it is "+s.now().UTC().Format(time.RFC3339)+" which is further in the future than "+
			"the tolerated delay of 300000 milliseconds.")
	}

	out := &api.GetRecordsOutput{Records: []api.Record{}, MillisBehindLatest: new(int64(0))}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		st, loadErr := loadStream(tx, m.Scope, it.Stream)
		if loadErr != nil {
			return loadErr
		}

		sh := st.shard(it.Shard)
		if sh == nil {
			return awserror.New("ResourceNotFoundException", "Shard "+it.Shard+" does not exist")
		}

		records, done, behind, readErr := s.readShard(tx, m.Scope, st, sh, it.After, limit)
		if readErr != nil {
			return readErr
		}

		for _, r := range records {
			out.Records = append(out.Records, api.Record{
				Data: r.Data, PartitionKey: new(r.PartitionKey), SequenceNumber: new(r.SequenceNumber),
				ApproximateArrivalTimestamp: new(common.Unix(r.Arrival)), EncryptionType: new(st.Encryption),
			})
			it.After = r.SequenceNumber
		}

		if behind != nil {
			out.MillisBehindLatest = new(max(0, s.now().UnixMilli()-int64(*behind*1000)))
		}

		if done {
			out.ChildShards = []api.ChildShard{}
			for i := range st.Shards {
				child := &st.Shards[i]
				if child.Parent == sh.ID || child.AdjacentParent == sh.ID {
					parents := []string{child.Parent}
					if child.AdjacentParent != "" {
						parents = append(parents, child.AdjacentParent)
					}

					out.ChildShards = append(out.ChildShards, api.ChildShard{
						ShardId: new(child.ID), ParentShards: parents,
						HashKeyRange: &api.HashKeyRange{
							StartingHashKey: new(child.StartHash), EndingHashKey: new(child.EndHash),
						},
					})
				}
			}

			return nil
		}

		it.Expires = s.now().Add(iteratorLife).UnixMilli()
		out.NextShardIterator = it.encode()
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Shard describes a shard for in-process readers.
type Shard struct {
	ID             string
	Parent         string
	AdjacentParent string
	// Tip is the last sequence number the shard has given, which readers
	// starting at LATEST read after.
	Tip    string
	Closed bool
}

// Shards returns the shards of the stream an ARN names, oldest first.
func (s *Service) Shards(ctx context.Context, arn string) ([]Shard, error) {
	scope, name, ok := ParseStreamARN(arn)
	if !ok {
		return nil, invalidArgument("Invalid stream ARN %s", arn)
	}

	var out []Shard
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		st, err := loadStream(tx, scope, name)
		if err != nil {
			return err
		}

		for i := range st.Shards {
			sh := &st.Shards[i]
			out = append(out, Shard{
				ID: sh.ID, Parent: sh.Parent, AdjacentParent: sh.AdjacentParent, Closed: sh.Closed,
				Tip: sequence(sh.Index, sh.Next),
			})
		}

		return nil
	})

	return out, err
}

// Records returns up to limit records of a shard of the stream an ARN names,
// after the sequence number after ("" for the oldest), and whether the
// shard is closed and read to its end.
func (s *Service) Records(ctx context.Context, arn, shardID, after string, limit int) ([]Record, bool, error) {
	scope, name, ok := ParseStreamARN(arn)
	if !ok {
		return nil, false, invalidArgument("Invalid stream ARN %s", arn)
	}

	var records []Record
	var done bool
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		st, err := loadStream(tx, scope, name)
		if err != nil {
			return err
		}

		sh := st.shard(shardID)
		if sh == nil {
			return awserror.New("ResourceNotFoundException", "Shard "+shardID+" does not exist")
		}

		records, done, _, err = s.readShard(tx, scope, st, sh, after, limit)
		return err
	})

	return records, done, err
}

// Put stores a record in the stream an ARN names, as PutRecord does, for
// in-process writers.
func (s *Service) Put(ctx context.Context, arn string, data []byte, partitionKey string) error {
	scope, name, ok := ParseStreamARN(arn)
	if !ok {
		return invalidArgument("Invalid stream ARN %s", arn)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, err := loadStream(tx, scope, name)
		if err != nil {
			return err
		}

		if _, _, err = s.append(tx, scope, st, data, partitionKey, ""); err != nil {
			return err
		}

		return storage.Write(tx, streamKey(scope, name), st)
	})
}
