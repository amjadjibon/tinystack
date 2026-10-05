// Package ecs implements Amazon Elastic Container Service without HTTP
// dependencies. Without a Runtime no container runs: a task settles in the
// state an operation asks for at once, and a service keeps its desired count of
// running tasks as records. With one (UseRuntime), tasks start PENDING and a
// runner starts their containers (runtime.go). Resources belong to an account
// and region:
//
//	<scope>/ecs/cluster/<name>
//	<scope>/ecs/family/<family>                          (the next revision)
//	<scope>/ecs/taskdef/<family>/<zero-padded revision>
//	<scope>/ecs/service/<cluster>/<name>
//	<scope>/ecs/task/<cluster>/<task ID>
//	aws/ecs/index/<account>/<region>/<cluster>/<task ID>  (tasks the runner owns)
//
// Deleted clusters and services stay as INACTIVE records, as AWS reports them,
// and a name can be reused; stopped tasks are listed for an hour.
package ecs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	fieldTags      = "TAGS"
	fieldSettings  = "SETTINGS"
	kindCluster    = "cluster"
	kindService    = "service"
	kindTask       = "task"
	service        = "ecs"
	defaultCluster = "default"
	active         = "ACTIVE"
	inactive       = "INACTIVE"
	missing        = "MISSING"
)

var _ api.API = (*Service)(nil)

type Service struct {
	ctx       context.Context
	store     storage.Store
	runtime   Runtime
	writeLogs LogWriter
	logger    *slog.Logger
	now       func() time.Time
	cancel    context.CancelFunc
	// busy holds the index keys the runner is advancing, and cursors the time
	// of the last log line shipped from each container.
	busy    map[string]bool
	cursors map[string]time.Time
	wg      sync.WaitGroup
	mu      sync.Mutex
	once    sync.Once
}

func New(store storage.Store) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		ctx: ctx, cancel: cancel, store: store, now: time.Now, busy: map[string]bool{},
		cursors: map[string]time.Time{},
	}
}

// Stop ends the runner and removes every container the runtime started.
func (s *Service) Stop() {
	s.once.Do(func() {
		s.cancel()
		s.wg.Wait()
		if s.runtime != nil {
			s.runtime.Close()
		}
	})
}

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func clientError(format string, args ...any) error {
	return awserror.New("ClientException", fmt.Sprintf(format, args...))
}

func clusterNotFound() error {
	return awserror.New("ClusterNotFoundException", "Cluster not found.")
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

func arn(scope request.Scope, resource string) string {
	return "arn:aws:ecs:" + scope.Region + ":" + scope.AccountID + ":" + resource
}

func seconds(t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}

	return new(float64(t.UnixMilli()) / 1000)
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

// records reads every record under keyPrefix, in key order.
func records[T any](tx storage.ReadTx, keyPrefix string) ([]*T, error) {
	var out []*T
	for _, k := range tx.Keys(keyPrefix) {
		v, _, err := read[T](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}

	return out, nil
}

// page returns the items after the one token names, at most limit of them, and
// the token for the rest, if any. Tokens are the last returned item's ID.
func page[T any](items []T, id func(T) string, token *string, limit int) (selected []T, next *string, err error) {
	start := 0
	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, invalidParameter("Invalid token")
		}

		// The item after the token's, or after where it would be in
		// ascending order if it is gone.
		start = len(items)
		for i, item := range items {
			if id(item) == string(after) {
				start = i + 1
				break
			}

			if id(item) > string(after) && start == len(items) {
				start = i
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

// limit validates an optional page size of 1 to 100, which defaults to def.
func limit(n *int64, def int) (int, error) {
	if n == nil {
		return def, nil
	}

	if *n < 1 || *n > 100 {
		return 0, invalidParameter("maxResults must be between 1 and 100.")
	}

	return int(*n), nil
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,255}$`)

func checkName(name *string, member string) error {
	if name == nil || !namePattern.MatchString(*name) {
		return invalidParameter("%s can have up to 255 letters (uppercase and lowercase), numbers, underscores, "+
			"and hyphens.", member)
	}

	return nil
}

// resourceName returns the name a name or an ARN of kind ("cluster/",
// "service/<cluster>/", …) in scope refers to.
func resourceName(scope request.Scope, ref, kind string) (string, bool) {
	if !strings.HasPrefix(ref, "arn:") {
		return ref, namePattern.MatchString(ref)
	}

	name, ok := strings.CutPrefix(ref, arn(scope, kind))
	if !ok {
		// Services also have the old ARN form without the cluster.
		if old, cut := strings.CutPrefix(kind, "service/"); cut && old != "" {
			name, ok = strings.CutPrefix(ref, arn(scope, "service/"))
		}
	}

	return name, ok && namePattern.MatchString(name)
}

func oneOf(v *string, allowed ...string) bool {
	return v == nil || slices.Contains(allowed, *v)
}

// digits returns n random decimal digits.
func digits(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = '0' + b[i]%10
	}

	return string(b)
}

// newID returns 32 random hexadecimal digits, the form of a task ID.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
