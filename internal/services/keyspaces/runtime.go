package keyspaces

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	runtimeOwnerKey = "aws/keyspaces/runtime-owner"
	runtimeSeedKey  = "aws/keyspaces/runtime-seeded"
	runtimeIndex    = "aws/keyspaces/index/"
	runtimeQueue    = "aws/keyspaces/queue/"
)

// Runtime executes Cassandra schema changes outside storage transactions.
type Runtime interface {
	Ensure(context.Context, request.Scope) (string, error)
	Apply(context.Context, request.Scope, string) error
	Cleanup(context.Context, bool) error
}

type runtimeState struct {
	Endpoint string
	Error    string
	Sequence uint64
}

type change struct {
	Key        string
	Statements []string
	Sequence   uint64
}

func runtimeIndexKey(scope request.Scope) string {
	return runtimeIndex + scope.AccountID + "/" + scope.Region
}

func queuePrefix(scope request.Scope) string {
	return runtimeQueue + scope.AccountID + "/" + scope.Region + "/"
}

func enqueue(
	tx storage.WriteTx, scope request.Scope, key string, statements []string, initialize bool,
) (uint64, error) {
	index := runtimeIndexKey(scope)
	state, found, err := storage.Read[runtimeState](tx, index)
	if err != nil {
		return 0, err
	}

	if !found {
		state = &runtimeState{}
	}

	state.Sequence++
	if err := storage.Write(tx, index, state); err != nil {
		return 0, err
	}

	jobKey := fmt.Sprintf("%s%020d", queuePrefix(scope), state.Sequence)
	job := change{Key: key, Statements: statements, Sequence: state.Sequence}
	if err := storage.Write(tx, jobKey, job); err != nil {
		return 0, err
	}

	if initialize {
		tx.Put(runtimeSeedKey, []byte("true"))
	}

	return state.Sequence, nil
}

func (s *Service) UseDocker(ctx context.Context, logger *slog.Logger) error {
	owner, err := dockercli.RuntimeOwner(ctx, s.store, runtimeOwnerKey)
	if err != nil {
		return err
	}

	s.runtime = &dockerRuntime{owner: owner}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, seeded := tx.Get(runtimeSeedKey); !seeded {
			return s.seedRuntime(tx)
		}

		for _, key := range tx.Keys(runtimeIndex) {
			state, _, e := storage.Read[runtimeState](tx, key)
			if e != nil {
				return e
			}

			state.Endpoint, state.Error = "", ""
			if e := storage.Write(tx, key, state); e != nil {
				return e
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("prepare Keyspaces runtime: %w", err)
	}

	s.bg = background.New()
	s.bg.Every(time.Second, func(ctx context.Context) { s.visit(ctx, logger) })
	return nil
}

// seedRuntime turns existing records into their initial CQL schema in dependency order.
func (s *Service) seedRuntime(tx storage.WriteTx) error {
	type entry struct {
		record *record
		scope  request.Scope
		key    string
		rank   int64
	}

	var entries []entry
	for _, key := range tx.Keys("") {
		account, region, rest, ok := storage.Scoped(key, serviceName)
		if !ok {
			continue
		}

		kind, _, _ := strings.Cut(rest, "/")
		r, _, err := storage.Read[record](tx, key)
		if err != nil {
			return err
		}

		e := entry{key: key, scope: request.Scope{AccountID: account, Region: region}, record: r}
		switch kind {
		case kindKeyspace:
			e.rank = 0
		case kindType:
			e.rank = r.Depth + 1
		case kindTable:
			e.rank = math.MaxInt64
		default:
			continue
		}

		entries = append(entries, e)
	}

	slices.SortStableFunc(entries, func(a, b entry) int {
		return cmp.Compare(a.rank, b.rank)
	})
	for _, e := range entries {
		statement, err := seedStatement(tx, e.scope, e.record)
		if err != nil {
			return err
		}

		if err := s.writeChange(tx, e.scope, e.key, e.record, creating, statement); err != nil {
			return err
		}
	}

	tx.Put(runtimeSeedKey, []byte("true"))
	return nil
}

func seedStatement(tx storage.ReadTx, scope request.Scope, r *record) (string, error) {
	if r.Replication != nil {
		return createKeyspaceCQL(r.Name), nil
	}

	defined, err := definedTypes(tx, scope, r.Keyspace)
	if err != nil {
		return "", err
	}

	if r.Table == nil {
		return createTypeCQL(r, defined)
	}

	types, _, err := schemaTypes(r.Table.SchemaDefinition, defined)
	if err != nil {
		return "", err
	}

	return createTableCQL(r.Table, types), nil
}

func (s *Service) visit(ctx context.Context, logger *slog.Logger) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	var scopes []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		scopes = tx.Keys(runtimeIndex)
		return nil
	}); err != nil {
		return
	}

	for _, key := range scopes {
		account, region, _ := strings.Cut(strings.TrimPrefix(key, runtimeIndex), "/")
		scope := request.Scope{AccountID: account, Region: region}
		if err := s.reconcile(ctx, scope); err != nil && ctx.Err() == nil {
			logger.Warn("Keyspaces runtime reconciliation failed", "account", account, "region", region, "error", err)
		}
	}
}

func (s *Service) reconcile(ctx context.Context, scope request.Scope) error {
	// Limit the work per tick so a busy scope leaves room for other accounts and Regions.
	for range 32 {
		state, job, key, needed, err := s.nextChange(ctx, scope)
		if err != nil || !needed {
			return err
		}

		endpoint := state.Endpoint
		if endpoint == "" {
			endpoint, err = s.runtime.Ensure(ctx, scope)
		}

		if err == nil && job != nil {
			for _, statement := range job.Statements {
				if err = s.runtime.Apply(ctx, scope, statement); err != nil {
					break
				}
			}
		}

		if saveErr := s.finishChange(ctx, scope, endpoint, key, job, err); saveErr != nil {
			return saveErr
		}

		// Runtime errors are recorded in the inventory. Leave the job queued for the next visit.
		if err != nil || job == nil {
			return err
		}
	}

	return nil
}

func (s *Service) nextChange(
	ctx context.Context, scope request.Scope,
) (state *runtimeState, job *change, key string, needed bool, err error) {
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		state, found, err = storage.Read[runtimeState](tx, runtimeIndexKey(scope))
		if err != nil || !found {
			return err
		}

		keys := tx.Keys(queuePrefix(scope))
		if len(keys) == 0 {
			needed = state.Endpoint == "" && len(tx.Keys(scope.Prefix(serviceName, kindKeyspace))) != 0
			return nil
		}

		key, needed = keys[0], true
		job, _, err = storage.Read[change](tx, key)
		return err
	})
	return state, job, key, needed, err
}

func (s *Service) finishChange(
	ctx context.Context, scope request.Scope, endpoint, key string, job *change, runtimeErr error,
) error {
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		index := runtimeIndexKey(scope)
		state, found, err := storage.Read[runtimeState](tx, index)
		if err != nil || !found {
			return err
		}

		state.Endpoint, state.Error = endpoint, ""
		if runtimeErr != nil {
			// A stale endpoint triggers a fresh container/readiness check before retry.
			state.Endpoint, state.Error = "", runtimeErr.Error()
			return storage.Write(tx, index, state)
		}

		if job != nil {
			r, exists, e := storage.Read[record](tx, job.Key)
			if e != nil {
				return e
			}

			if exists && r.Pending == job.Sequence {
				r.Pending = 0
				if e := storage.Write(tx, job.Key, r); e != nil {
					return e
				}
			}

			tx.Delete(key)
		}

		return storage.Write(tx, index, state)
	})
}

func (s *Service) Stop() {
	if s.bg == nil || !s.bg.Stop() {
		return
	}

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.runtime.Cleanup(ctx, false); err != nil {
		slog.Warn("Keyspaces Docker shutdown", "error", err)
	}
}

func (s *Service) Reset(ctx context.Context) {
	if s.runtime == nil {
		return
	}

	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, prefix := range []string{runtimeIndex, runtimeQueue} {
			for _, key := range tx.Keys(prefix) {
				tx.Delete(key)
			}
		}

		return nil
	})
	if err != nil {
		slog.Warn("Keyspaces runtime reset", "error", err)
	}

	if err = s.runtime.Cleanup(ctx, true); err != nil {
		slog.Warn("Keyspaces Docker reset", "error", err)
	}
}
