// Package kinesis implements Kinesis Data Streams without HTTP dependencies.
// Streams become ACTIVE at once, and resharding closes shards and opens
// their children in the same transaction. Resources belong to an account and
// region:
//
//	<scope>/kinesis/stream/<name>                         (the stream with its shards and consumers)
//	<scope>/kinesis/record/<stream>/<shard ID>/<sequence number>
//
// Sequence numbers are fixed-width decimal strings, so a shard's records list
// in order. Records older than the retention period are skipped by reads and
// deleted by later writes. Lambda and Firehose read streams in-process through
// Shards and Read.
package kinesis

import (
	"context"
	"crypto/md5" // #nosec G501 -- Kinesis maps partition keys to hash keys with MD5.
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service          = "kinesis"
	kindStream       = "stream"
	kindRecord       = "record"
	statusActive     = "ACTIVE"
	modeProvisioned  = "PROVISIONED"
	modeOnDemand     = "ON_DEMAND"
	onDemandShards   = 4
	defaultRetention = 24
	maxRetention     = 8760
	maxShards        = 500
	maxRecord        = 1 << 20
	maxBatch         = 500
	iteratorLife     = 5 * time.Minute
	encryptionKMS    = "KMS"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

// shard is one shard of a stream.
type shard struct {
	ID             string
	Parent         string `json:",omitempty"`
	AdjacentParent string `json:",omitempty"`
	StartHash      string
	EndHash        string
	// End is the last sequence number of a closed shard; open shards have none.
	End    string `json:",omitempty"`
	Index  int
	Next   int64
	Closed bool `json:",omitempty"`
}

type consumer struct {
	Name    string
	ARN     string
	Created float64
}

// stream is a stream's record.
type stream struct {
	Tags       map[string]string
	Name       string
	Mode       string
	Encryption string
	KeyID      string `json:",omitempty"`
	Metrics    []string
	Shards     []shard
	Consumers  []consumer
	Created    float64
	Retention  int64
	NextShard  int
}

// record is one data record.
type record struct {
	PartitionKey string
	Sequence     string
	Data         []byte
	Arrival      float64
}

func invalidArgument(format string, args ...any) error {
	return awserror.New("InvalidArgumentException", fmt.Sprintf(format, args...))
}

func notFound(scope request.Scope, name string) error {
	return awserror.New("ResourceNotFoundException",
		fmt.Sprintf("Stream %s under account %s not found.", name, scope.AccountID))
}

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Metadata{}, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func prefix(scope request.Scope, kind string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/"
}

func streamKey(scope request.Scope, name string) string { return prefix(scope, kindStream) + name }

func recordPrefix(scope request.Scope, name, shardID string) string {
	return prefix(scope, kindRecord) + name + "/" + shardID + "/"
}

// StreamARN is a stream's ARN.
func StreamARN(scope request.Scope, name string) string {
	return "arn:aws:kinesis:" + scope.Region + ":" + scope.AccountID + ":stream/" + name
}

// ParseStreamARN returns the scope and name of a stream's ARN.
func ParseStreamARN(arn string) (request.Scope, string, bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != service {
		return request.Scope{}, "", false
	}

	name, ok := strings.CutPrefix(parts[5], "stream/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return request.Scope{}, "", false
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, name, true
}

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

func read[T any](tx storage.ReadTx, k string) (v *T, found bool, err error) {
	data, ok := tx.Get(k)
	if !ok {
		return nil, false, nil
	}

	v = new(T)
	if err = json.Unmarshal(data, v); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", k, err)
	}

	return v, true, nil
}

func write(tx storage.WriteTx, k string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	tx.Put(k, data)
	return nil
}

func unix(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

var streamName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// streamTarget resolves the stream a request names by StreamName or
// StreamARN, which must agree and be in the caller's account and region.
func streamTarget(m *request.Metadata, name, arn *string) (string, error) {
	if arn != nil {
		scope, fromARN, ok := ParseStreamARN(*arn)
		if !ok || scope != m.Scope {
			return "", invalidArgument("Invalid StreamARN: %s", *arn)
		}

		if name != nil && *name != fromARN {
			return "", invalidArgument("StreamName and StreamARN name different streams")
		}

		return fromARN, nil
	}

	if !streamName.MatchString(value(name)) {
		return "", invalidArgument("StreamName or StreamARN is required")
	}

	return *name, nil
}

func loadStream(tx storage.ReadTx, scope request.Scope, name string) (*stream, error) {
	st, found, err := read[stream](tx, streamKey(scope, name))
	if err != nil || !found {
		if err == nil {
			err = notFound(scope, name)
		}

		return nil, err
	}

	return st, nil
}

// change runs fn on the stream a request names and stores it.
func (s *Service) change(ctx context.Context, name, arn *string, fn func(tx storage.WriteTx, scope request.Scope,
	st *stream) error,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	target, err := streamTarget(&m, name, arn)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, loadErr := loadStream(tx, m.Scope, target)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, m.Scope, st); fnErr != nil {
			return fnErr
		}

		return write(tx, streamKey(m.Scope, st.Name), st)
	})
}

// view runs fn on the stream a request names.
func (s *Service) view(ctx context.Context, name, arn *string, fn func(tx storage.ReadTx, scope request.Scope,
	st *stream) error,
) error {
	m, err := identity(ctx)
	if err != nil {
		return err
	}

	target, err := streamTarget(&m, name, arn)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		st, loadErr := loadStream(tx, m.Scope, target)
		if loadErr != nil {
			return loadErr
		}

		return fn(tx, m.Scope, st)
	})
}

// maxHash is the largest hash key, 2^128 - 1.
var maxHash = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

// ranges splits the hash key space from start to end into n even ranges.
func ranges(start, end *big.Int, n int) [][2]*big.Int {
	width := new(big.Int).Add(new(big.Int).Sub(end, start), big.NewInt(1))
	step := new(big.Int).Div(width, big.NewInt(int64(n)))
	out := make([][2]*big.Int, n)
	for i := range n {
		lo := new(big.Int).Add(start, new(big.Int).Mul(step, big.NewInt(int64(i))))
		hi := new(big.Int).Sub(new(big.Int).Add(lo, step), big.NewInt(1))
		if i == n-1 {
			hi = new(big.Int).Set(end)
		}

		out[i] = [2]*big.Int{lo, hi}
	}

	return out
}

func hashOf(s string) *big.Int {
	n, _ := new(big.Int).SetString(s, 10)
	return n
}

// openShard adds a new open shard over a hash range.
func (st *stream) openShard(lo, hi *big.Int, parent, adjacent string) {
	st.Shards = append(st.Shards, shard{
		ID: fmt.Sprintf("shardId-%012d", st.NextShard), Index: st.NextShard, StartHash: lo.String(),
		EndHash: hi.String(), Parent: parent, AdjacentParent: adjacent,
	})
	st.NextShard++
}

// sequence is the sequence number of a shard's nth record: fixed-width
// decimal, so a shard's sequence numbers sort as text.
func sequence(index int, n int64) string { return fmt.Sprintf("49%010d%044d", index, n) }

func (sh *shard) close() {
	sh.Closed, sh.End = true, sequence(sh.Index, sh.Next)
}

// open returns the open shards.
func (st *stream) open() []*shard {
	var out []*shard
	for i := range st.Shards {
		if !st.Shards[i].Closed {
			out = append(out, &st.Shards[i])
		}
	}

	return out
}

func (st *stream) shard(id string) *shard {
	for i := range st.Shards {
		if st.Shards[i].ID == id {
			return &st.Shards[i]
		}
	}

	return nil
}

// route returns the open shard whose hash range holds a record's hash key:
// its explicit hash key, or the MD5 of its partition key.
func (st *stream) route(partitionKey, explicit string) (*shard, error) {
	if n := len(partitionKey); n < 1 || n > 256 {
		return nil, invalidArgument("PartitionKey must be 1 to 256 characters")
	}

	var key *big.Int
	if explicit != "" {
		var ok bool
		if key, ok = new(big.Int).SetString(explicit, 10); !ok || key.Sign() < 0 || key.Cmp(maxHash) > 0 {
			return nil, invalidArgument("Invalid ExplicitHashKey: %s", explicit)
		}
	} else {
		digest := md5.Sum([]byte(partitionKey)) // #nosec G401 -- Kinesis's partition key hash.
		key = new(big.Int).SetBytes(digest[:])
	}

	for _, sh := range st.open() {
		if key.Cmp(hashOf(sh.StartHash)) >= 0 && key.Cmp(hashOf(sh.EndHash)) <= 0 {
			return sh, nil
		}
	}

	return nil, invalidArgument("No open shard holds the hash key")
}

func checkTags(tags map[string]string) error {
	if err := tagging.Check(tags); err != nil {
		return invalidArgument("%s", err.Error())
	}

	return nil
}
