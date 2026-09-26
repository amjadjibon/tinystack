package ec2

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Record kinds. Each resource is one record under
// "<account>/<region>/ec2/<kind>/<ID>".
const (
	kindVPC    = "vpc"
	kindSubnet = "subnet"
	// kindDefaults marks a Region whose default VPC was created, so deleting
	// the default VPC does not bring it back.
	kindDefaults = "defaults"
)

func key(scope request.Scope, kind, id string) string {
	return scope.AccountID + "/" + scope.Region + "/ec2/" + kind + "/" + id
}

// newID returns an EC2 resource ID: prefix, a dash, and 17 hex digits.
func newID(prefix string) string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])[:17]
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

// list decodes every record of kind in scope, ordered by ID.
func list[T any](tx storage.ReadTx, scope request.Scope, kind string) ([]*T, error) {
	var out []*T
	for _, k := range tx.Keys(key(scope, kind, "")) {
		v, _, err := read[T](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	return out, nil
}

// notFound is EC2's error for a missing resource, such as
// InvalidVpcID.NotFound.
func notFound(code, noun, id string) error {
	return awserror.New(code, fmt.Sprintf("The %s ID '%s' does not exist", noun, id))
}

// selectIDs keeps the records whose ID is in ids, in ID order, and fails on
// the first ID that names none. No ids selects every record.
func selectIDs[T any](records []*T, id func(*T) string, ids []string, missing func(string) error) ([]*T, error) {
	if len(ids) == 0 {
		return records, nil
	}

	for _, want := range ids {
		if !slices.ContainsFunc(records, func(r *T) bool { return id(r) == want }) {
			return nil, missing(want)
		}
	}

	return slices.DeleteFunc(records, func(r *T) bool { return !slices.Contains(ids, id(r)) }), nil
}

// page returns the records after the NextToken, at most maxResults of them
// (all when it is nil), and the token of the next page. records are ordered
// by ID.
func page[T any](records []*T, id func(*T) string, token *string, maxResults *int64, limit int64) (
	selected []*T,
	next *string,
	err error,
) {
	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, invalidParameter("The token '" + *token + "' is invalid.")
		}

		records = slices.DeleteFunc(records, func(r *T) bool { return strings.Compare(id(r), string(after)) <= 0 })
	}

	if maxResults == nil {
		return records, nil, nil
	}

	if *maxResults < 5 || *maxResults > limit {
		return nil, nil, invalidParameter(fmt.Sprintf("Value ( %d ) for parameter maxResults is invalid. "+
			"Expecting a value between 5 and %d.", *maxResults, limit))
	}

	if int64(len(records)) <= *maxResults {
		return records, nil, nil
	}

	records = records[:*maxResults]
	return records, new(base64.RawURLEncoding.EncodeToString([]byte(id(records[len(records)-1])))), nil
}

// view runs fn in a read transaction on the caller's Region, after creating
// its default VPC if it has none yet.
func (s *Service) view(ctx context.Context, dry *bool, fn func(tx storage.ReadTx, scope request.Scope) error) error {
	m, err := s.begin(ctx, dry)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, m.Scope) })
}

// update runs fn in a write transaction on the caller's Region, after
// creating its default VPC if it has none yet.
func (s *Service) update(
	ctx context.Context,
	dry *bool,
	fn func(tx storage.WriteTx, scope request.Scope) error,
) error {
	m, err := s.begin(ctx, dry)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, m.Scope) })
}

// begin resolves the caller, answers DryRun, and makes sure the Region has
// its defaults.
func (s *Service) begin(ctx context.Context, dry *bool) (request.Metadata, error) {
	m, err := caller(ctx, dry)
	if err != nil {
		return m, err
	}

	return m, s.defaults(ctx, m.Scope)
}
