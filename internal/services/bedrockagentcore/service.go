// Package bedrockagentcore implements the Amazon Bedrock AgentCore data plane: invoking agent runtimes in
// containers, and short-term memory events.
//
// A runtime session is one container, started on the session's first InvokeAgentRuntime and kept until it has been
// idle for the runtime's idleRuntimeSessionTimeout, has lived for its maxLifetime, or is stopped by
// StopRuntimeSession, the deletion of its runtime or endpoint, a reset or shutdown. Without a Runtime
// (bedrockagentcore.runtimes: records) InvokeAgentRuntime fails explicitly.
//
// Events are JSON records under <account>/<region>/bedrockagentcore/:
//
//	event/<memory>/<actor>/<session>/<event>   an event, the actor escaped so it holds no slash
//	event-token/<memory>/<token digest>        the key of the event a client token created
//
// aws/bedrockagentcore/expiry/<time>/<event key> orders the events by expiry time for the sweeper.
package bedrockagentcore

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcore"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/bedrockagentcorecontrol"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "bedrockagentcore"
	kindEvent   = "event"
	kindToken   = "event-token"
	expiryIndex = "aws/bedrockagentcore/expiry/"
	ownerKey    = "aws/bedrockagentcore/runtime-owner"

	// sweepInterval is how often idle sessions and expired events are visited.
	sweepInterval = time.Second
	// startTimeout bounds starting a session's container, which includes pulling its image.
	startTimeout = 5 * time.Minute
	// placeholderKey signs the SDK calls a container makes when its runtime's creator was unsigned or has no
	// configured secret key, as for Lambda functions.
	placeholderKey = "tinystack"
)

var _ api.API = (*Service)(nil)

var _ bedrockagentcorecontrol.Observer = (*Service)(nil)

// Spec describes the container of one runtime session.
type Spec struct {
	Environment map[string]string
	// Owner labels the container so a later TinyStack with the same owner can remove it.
	Owner string
	// Session is the runtime and session IDs the container serves.
	Session string
	Image   string
	// Probe is the path that tells the container is ready; with StrictProbe only a 200 does, otherwise any HTTP
	// answer.
	Probe string
	// Port is the container port the runtime service contract serves the protocol on.
	Port        int
	StrictProbe bool
}

// Started is a running session container: its ID and the loopback address its contract port is published on.
type Started struct {
	ID   string
	Addr string
}

// Runtime runs session containers.
type Runtime interface {
	// Start runs the container and returns once it answers on its contract port.
	Start(ctx context.Context, spec *Spec) (Started, error)
	// Remove removes the container.
	Remove(ctx context.Context, id string) error
	// Clean removes the containers that earlier runs with the owner left behind.
	Clean(ctx context.Context, owner string) error
}

// Service serves the data plane over the control plane's records.
type Service struct {
	now        func() time.Time
	store      storage.Store
	logger     *slog.Logger
	runtime    Runtime
	client     *http.Client
	secretKeys map[string]string
	sessions   map[string]*session
	bg         *background.Group
	endpoint   string
	owner      string
	mu         sync.Mutex
	closed     bool
}

// New constructs the service and starts the event expiry sweeper; Stop ends it. secretKeys holds the configured
// SigV4 secrets by access key, which a container signs its SDK calls with.
func New(store storage.Store, secretKeys map[string]string) *Service {
	return newService(store, secretKeys, time.Now)
}

func newService(store storage.Store, secretKeys map[string]string, now func() time.Time) *Service {
	s := &Service{
		now: now, store: store, logger: slog.Default(), client: &http.Client{}, secretKeys: secretKeys,
		sessions: map[string]*session{}, bg: background.New(),
	}

	s.bg.Every(sweepInterval, s.expireEvents)

	return s
}

func invalid(format string, args ...any) error { return awserror.Validation(format, args...) }

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

func pageLimit(n *int64) int {
	if n == nil {
		return 100
	}

	return int(*n)
}

var errBadToken = awserror.Validation("nextToken is not valid")

func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// Stop ends the background workers and removes the session containers.
func (s *Service) Stop() {
	s.bg.Stop()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.removeSessions(context.Background(), func(*session) bool { return true })
}

// Reset removes every session container before the store is cleared.
func (s *Service) Reset(ctx context.Context) {
	s.removeSessions(ctx, func(*session) bool { return true })
}

// RuntimeDeleted removes the sessions of the runtime.
func (s *Service) RuntimeDeleted(ctx context.Context, scope request.Scope, runtimeID string) {
	s.removeSessions(ctx, func(sess *session) bool { return sess.scope == scope && sess.runtimeID == runtimeID })
}

// EndpointDeleted removes the sessions that started through the endpoint.
func (s *Service) EndpointDeleted(ctx context.Context, scope request.Scope, runtimeID, endpoint string) {
	s.removeSessions(ctx, func(sess *session) bool {
		return sess.scope == scope && sess.runtimeID == runtimeID && sess.endpoint == endpoint
	})
}

// VersionDeleted removes the sessions that run the version.
func (s *Service) VersionDeleted(ctx context.Context, scope request.Scope, runtimeID string, version int) {
	s.removeSessions(ctx, func(sess *session) bool {
		return sess.scope == scope && sess.runtimeID == runtimeID && sess.version == version
	})
}

// MemoryDeleted deletes the events of the memory.
func (s *Service) MemoryDeleted(ctx context.Context, scope request.Scope, memoryID string) {
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(eventPrefix(scope, memoryID)) {
			rec, found, err := storage.Read[eventRecord](tx, key)
			if err != nil {
				return err
			}

			if found {
				tx.Delete(expiryKey(rec.Expires, key))
			}

			tx.Delete(key)
		}

		for _, key := range tx.Keys(tokenPrefix(scope, memoryID)) {
			tx.Delete(key)
		}

		return nil
	})
}

// credentials returns the access key and secret the container signs its SDK calls with.
func (s *Service) credentials(accessKey string) (key, secret string) {
	key, secret = placeholderKey, placeholderKey
	if accessKey != "" {
		key = accessKey
	}

	if v, ok := s.secretKeys[accessKey]; ok {
		secret = v
	}

	return key, secret
}

// newSessionID returns a session ID for a request that brought none.
func newSessionID() string { return common.RandomID(40, common.AlphabetAlphanumeric) }
