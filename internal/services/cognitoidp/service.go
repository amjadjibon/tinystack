// Package cognitoidp implements Amazon Cognito user pools, their app clients,
// groups, and users without HTTP dependencies. Pools belong to an account and
// region:
//
//	<scope>/cognitoidp/pool/<pool ID>
//	<scope>/cognitoidp/client/<pool ID>/<client ID>
//	<scope>/cognitoidp/group/<pool ID>/<group name>
//	<scope>/cognitoidp/user/<pool ID>/<username, folded if names ignore case>
//	<scope>/cognitoidp/alias/<pool ID>/<attribute>/<lowercased value> (a username)
//
// Users hold their group names, password hash, and pending confirmation codes.
// Messages a pool would send are recorded in the delivery log instead.
//
// Clients are also indexed partition-wide under aws/cognitoidp/client/<client
// ID>, because the operations a pool's users call unsigned name only a client
// ID or a token, never an account; pools are indexed under
// aws/cognitoidp/pool/<pool ID> for their public keys.
package cognitoidp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service  = "cognitoidp"
	lower    = "abcdefghijklmnopqrstuvwxyz0123456789"
	alphaNum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ" + lower
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	// deliveries records the messages TinyStack would send to users.
	deliveries *delivery.Log
	now        func() time.Time
}

// New returns Cognito, recording the email and SMS messages its pools send in
// deliveries.
func New(store storage.Store, deliveries *delivery.Log) *Service {
	return &Service{store: store, deliveries: deliveries, now: time.Now}
}

var poolID = regexp.MustCompile(`^[\w-]+_[0-9a-zA-Z]+$`)

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

func identity(ctx context.Context) (request.Scope, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Scope{}, fmt.Errorf("missing request identity")
	}

	return m.Scope, nil
}

func prefix(scope request.Scope, kind string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/"
}

// poolKinds are the record kinds a pool owns, each under <kind>/<pool ID>/.
var poolKinds = []string{"client", "user", "alias", "group", "refresh", "session"}

func poolKey(scope request.Scope, id string) string { return prefix(scope, "pool") + id }

func clientKey(scope request.Scope, pool, id string) string {
	return prefix(scope, "client") + pool + "/" + id
}

func userPrefix(scope request.Scope, pool string) string { return prefix(scope, "user") + pool + "/" }

// poolIndexKey holds a pool's account, partition-wide, so its public keys can
// be served by pool ID alone.
func poolIndexKey(id string) string { return "aws/" + service + "/pool/" + id }

// clientIndexKey locates a client, partition-wide, by its ID alone.
func clientIndexKey(id string) string { return "aws/" + service + "/client/" + id }

func poolARN(scope request.Scope, id string) string {
	return "arn:aws:cognito-idp:" + scope.Region + ":" + scope.AccountID + ":userpool/" + id
}

// randomString returns n characters drawn from set.
func randomString(set string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = set[int(b[i])%len(set)]
	}

	return string(b)
}

func seconds(t time.Time) *float64 { return new(float64(t.UnixMilli()) / 1000) }

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

// convert copies the fields two wire shapes share by name, such as a request's
// settings into a stored record, and a record into a response.
func convert[T any](from any) (T, error) {
	var out T
	data, err := json.Marshal(from)
	if err != nil {
		return out, err
	}

	err = json.Unmarshal(data, &out)
	return out, err
}

// page returns the items after the one token names, at most limit of them, and
// the token for the rest, if any. Tokens are the last returned item's ID.
func page[T any](items []T, id func(T) string, token *string, limit int) (selected []T, next *string, err error) {
	start := 0
	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, invalidParameter("The pagination token is invalid.")
		}

		start = len(items)
		for i, item := range items {
			if id(item) > string(after) {
				start = i
				break
			}
		}
	}

	items = items[start:]
	if len(items) <= limit {
		return items, nil, nil
	}

	next = new(base64.RawURLEncoding.EncodeToString([]byte(id(items[limit-1]))))
	return items[:limit], next, nil
}

// limit validates an optional page size of up to maxPage, which it defaults to.
func limit(n *int64, name string) (int, error) {
	if n == nil || *n == 0 {
		return maxPage, nil
	}

	if *n < 0 || *n > maxPage {
		return 0, invalidParameter("%s must be between 0 and %d.", name, maxPage)
	}

	return int(*n), nil
}

// loadPool returns the pool id names in scope.
func loadPool(tx storage.ReadTx, scope request.Scope, id *string) (*pool, error) {
	if id == nil || !poolID.MatchString(*id) || len(*id) > 55 {
		return nil, invalidParameter("1 validation error detected: Value '%s' at 'userPoolId' failed to satisfy "+
			"constraint: Member must satisfy regular expression pattern: [\\w-]+_[0-9a-zA-Z]+", value(id))
	}

	p, ok, err := read[pool](tx, poolKey(scope, *id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("User pool %s does not exist.", *id)
	}

	return p, nil
}

// view runs fn with the pool id names.
func (s *Service) view(ctx context.Context, id *string, fn func(storage.ReadTx, request.Scope, *pool) error) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		p, loadErr := loadPool(tx, scope, id)
		if loadErr != nil {
			return loadErr
		}

		return fn(tx, scope, p)
	})
}

// update runs fn with the pool id names and stores the pool after it.
func (s *Service) update(
	ctx context.Context,
	id *string,
	fn func(storage.WriteTx, request.Scope, *pool) error,
) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		p, loadErr := loadPool(tx, scope, id)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(tx, scope, p); fnErr != nil {
			return fnErr
		}

		return write(tx, poolKey(scope, p.ID), p)
	})
}

// oneOf reports whether v is empty or one of allowed.
func oneOf(v *string, allowed ...string) bool {
	return v == nil || slices.Contains(allowed, *v)
}

// checkName validates a pool or client name.
var namePattern = regexp.MustCompile(`^[\w\s+=,.@-]+$`)

func checkName(name *string, member string) error {
	if name == nil || len(*name) > 128 || !namePattern.MatchString(*name) {
		return invalidParameter("%s must be 1 to 128 characters matching [\\w\\s+=,.@-]+.", member)
	}

	return nil
}

// ids returns the IDs after the scope's prefix, which list in order.
func ids(tx storage.ReadTx, keyPrefix string) []string {
	var out []string
	for _, k := range tx.Keys(keyPrefix) {
		out = append(out, strings.TrimPrefix(k, keyPrefix))
	}

	return out
}
