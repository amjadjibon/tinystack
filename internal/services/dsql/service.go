// Package dsql implements Aurora DSQL control-plane records and optional PostgreSQL execution.
package dsql

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dsql"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/localtls"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName   = "dsql"
	kindCluster   = "cluster"
	indexPrefix   = "aws/dsql/index/"
	creating      = "CREATING"
	active        = "ACTIVE"
	updating      = "UPDATING"
	deleting      = "DELETING"
	pendingSetup  = "PENDING_SETUP"
	pendingDelete = "PENDING_DELETE"
)

type Service struct {
	store       storage.Store
	workers     *background.Group
	runtime     *dockerRuntime
	ca          *localtls.Authority
	authorize   TokenAuthorizer
	proxies     map[string]*proxy
	proxyMu     sync.Mutex
	reconcileMu sync.Mutex
}

type record struct {
	api.GetClusterOutput
	Due           time.Time
	Policy        *string
	Backend       *backend
	PolicyVersion string
	ProxyPort     int64
	Activated     bool
}

type work struct {
	Scope request.Scope
	ID    string
}

func New(store storage.Store) *Service {
	s := &Service{store: store, workers: background.New(), proxies: map[string]*proxy{}}
	s.workers.Every(100*time.Millisecond, s.advance)
	return s
}

func (s *Service) Stop() {
	s.workers.Stop()
	s.closeProxies()
	if s.runtime != nil {
		s.runtime.close(context.Background())
	}
}

func invalid(message string) error { return awserror.New("ValidationException", message) }

func conflict(message string) error { return awserror.New("ConflictException", message) }

func missing() error {
	return awserror.New("ResourceNotFoundException", "DSQL cluster or policy does not exist")
}

func key(scope request.Scope, id string) string { return scope.Key(serviceName, kindCluster, id) }

func indexKey(scope request.Scope, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + id
}

func resourceARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:dsql:%s:%s:cluster/%s", scope.Region, scope.AccountID, id)
}

func parseARN(scope request.Scope, arn string) (request.Scope, string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != serviceName ||
		parts[4] != scope.AccountID {
		return scope, "", missing()
	}

	id, ok := strings.CutPrefix(parts[5], "cluster/")
	if !ok || id == "" {
		return scope, "", missing()
	}

	return request.Scope{AccountID: scope.AccountID, Region: parts[3]}, id, nil
}

func read(tx storage.ReadTx, scope request.Scope, id string) (*record, error) {
	r, found, err := storage.Read[record](tx, key(scope, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func write(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, key(scope, common.Value(r.Identifier)), r)
}

func queue(tx storage.WriteTx, scope request.Scope, r *record, status string) error {
	r.Status = new(status)
	r.Due = time.Now().UTC().Add(150 * time.Millisecond)
	if err := write(tx, scope, r); err != nil {
		return err
	}

	return storage.Write(
		tx,
		indexKey(scope, common.Value(r.Identifier)),
		work{Scope: scope, ID: common.Value(r.Identifier)},
	)
}

func (s *Service) get(ctx context.Context, id *string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, common.Value(id))
		return e
	})
	return r, err
}

// mutate retains successful responses, including deletes, independently of resource lifetime.
func mutate[T any](ctx context.Context, s *Service, op string, token *string, in any,
	fn func(storage.WriteTx, request.Scope) (*T, error),
) (*T, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	replayKey := idempotency.Key(scope, serviceName, op, token)
	var out *T
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, found, replayErr := idempotency.Replay[*T](tx, replayKey, in,
			conflict("Client token was already used with different parameters"))
		if replayErr != nil || found {
			out = previous
			return replayErr
		}

		var e error
		out, e = fn(tx, scope)
		if e != nil {
			return e
		}

		return idempotency.Remember(tx, replayKey, in, out)
	})
	return out, err
}

var _ api.API = (*Service)(nil)
