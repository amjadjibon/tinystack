package opensearch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	indexPrefix = "aws/opensearch/index/"

	tick = time.Second
	// ensureTimeout bounds creating and starting a container, which includes pulling its image.
	ensureTimeout = 10 * time.Minute
	probeTimeout  = 3 * time.Second
	// startTimeout is how long a started container has to become healthy before the domain is failed.
	startTimeout = 5 * time.Minute
)

// Spec describes the container and volume of one domain.
type Spec struct {
	Owner    string
	ARN      string
	Name     string
	Volume   string
	Image    string
	DataPath string
	Env      []string
}

// Started holds the ID and published loopback port of a running container.
type Started struct {
	ID   string
	Port int64
}

// Runtime performs container I/O outside storage transactions.
type Runtime interface {
	// Resources lists the containers and volumes earlier runtimes left behind, with their domain ARNs.
	Resources(ctx context.Context, owner string) ([]*Spec, error)
	// Ensure starts the domain's container, creating it and its volume if needed.
	Ensure(ctx context.Context, spec *Spec) (Started, error)
	// Probe reports whether the engine answers a cluster health request.
	Probe(ctx context.Context, port int64) error
	// Remove removes the container and, if volume, the data volume.
	Remove(ctx context.Context, spec *Spec, volume bool) error
	// Close removes the running containers and keeps the volumes.
	Close()
}

// resourceSpec names the container and volume of the domain with the ARN; the names are stable across restarts.
func (s *Service) resourceSpec(arn string) *Spec {
	hash := sha256.Sum256([]byte(s.owner + arn))
	name := fmt.Sprintf("tinystack-opensearch-%x", hash[:16])

	return &Spec{Owner: s.owner, ARN: arn, Name: name, Volume: name + "-data"}
}

func indexKey(scope request.Scope, name string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + name
}

func parseIndexKey(key string) (scope request.Scope, name string, ok bool) {
	parts := strings.Split(strings.TrimPrefix(key, indexPrefix), "/")
	if len(parts) != 3 {
		return request.Scope{}, "", false
	}

	return request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2], true
}

// ready reports whether the domain's engine answered its last probe.
func (r *domainRecord) ready() bool {
	return r.Container != "" && r.Failure == "" && (!common.Value(r.Status.Processing) || !r.PendingUntil.IsZero())
}

// UseRuntime has every domain run in a container. Domains that already exist, such as those a persistent store
// holds, start again against their volumes: the endpoints they published belong to the previous runtime.
func (s *Service) UseRuntime(rt Runtime, logger *slog.Logger) error {
	owner, err := dockercli.RuntimeOwner(s.store, "aws/opensearch/runtime-owner")
	if err != nil {
		return err
	}

	s.owner = owner
	err = s.store.Update(context.Background(), func(tx storage.WriteTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			kind, name, _ := strings.Cut(rest, "/")
			if kind != domainKind {
				continue
			}

			rec, _, readErr := storage.Read[domainRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			rec.Container, rec.Port, rec.Failure, rec.Since = "", 0, "", time.Now().UTC()
			rec.Status.Endpoint, rec.Status.Processing = nil, new(true)
			if writeErr := storage.Write(tx, key, *rec); writeErr != nil {
				return writeErr
			}

			index := indexKey(request.Scope{AccountID: account, Region: region}, name)
			if writeErr := storage.Write(tx, index, true); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("prepare OpenSearch runtime recovery: %w", err)
	}

	s.runtime, s.logger = rt, logger
	s.bg = background.New()
	s.bg.Every(tick, s.visit)

	return nil
}

// Stop waits for background I/O and removes the containers, retaining the volumes.
func (s *Service) Stop() {
	if s.bg != nil {
		s.bg.Stop()
	}

	if s.runtime != nil {
		s.runtime.Close()
	}
}

// Reset removes the containers and volumes of every domain before app.Reset clears the store.
func (s *Service) Reset(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == nil {
		return
	}

	var arns []string
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if kind, name, _ := strings.Cut(rest, "/"); ok && kind == domainKind {
				arns = append(arns, domainARN(region, account, name))
			}
		}

		for _, key := range tx.Keys(indexPrefix) {
			if scope, name, ok := parseIndexKey(key); ok {
				arns = append(arns, domainARN(scope.Region, scope.AccountID, name))
			}
		}

		return nil
	})

	for _, arn := range arns {
		_ = s.runtime.Remove(ctx, s.resourceSpec(arn), true)
	}

	// Drop the index while holding the lock so the poller cannot recreate the containers.
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(indexPrefix) {
			tx.Delete(key)
		}

		return nil
	})
}

func (s *Service) logError(msg string, err error, args ...any) {
	if s.logger != nil && err != nil {
		s.logger.Error(msg, append(args, "err", err)...)
	}
}

// visit steps every domain toward running, once a second.
func (s *Service) visit(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.orphansDone {
		if err := s.cleanOrphans(ctx); err != nil {
			s.logError("opensearch orphan cleanup failed", err)
			return
		}

		s.orphansDone = true
	}

	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(indexPrefix)
		return nil
	}); err != nil {
		return
	}

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		if err := s.advance(ctx, key); ctx.Err() == nil {
			s.logError("opensearch advance failed", err, "key", key)
		}
	}
}

// cleanOrphans removes the containers and volumes whose domain no longer exists.
func (s *Service) cleanOrphans(ctx context.Context) error {
	resources, err := s.Resources(ctx)
	if err != nil {
		return err
	}

	known := make(map[string]bool, len(resources))
	for _, resource := range resources {
		known[resource.ARN] = true
	}

	specs, err := s.runtime.Resources(ctx, s.owner)
	if err != nil {
		return err
	}

	for _, spec := range specs {
		if !known[spec.ARN] {
			if removeErr := s.runtime.Remove(ctx, spec, true); removeErr != nil {
				return removeErr
			}
		}
	}

	return nil
}

func (s *Service) advance(ctx context.Context, key string) error {
	scope, name, ok := parseIndexKey(key)
	if !ok {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(key)
			return nil
		})
	}

	recordKey := scope.Key(serviceName, domainKind, name)
	var rec *domainRecord
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		rec, _, readErr = storage.Read[domainRecord](tx, recordKey)
		return readErr
	}); err != nil {
		return err
	}

	resource := s.resourceSpec(domainARN(scope.Region, scope.AccountID, name))
	if rec == nil || common.Value(rec.Status.Deleted) {
		// DeleteDomain leaves the record until the container and volume are gone.
		if err := s.runtime.Remove(ctx, resource, true); err != nil {
			return err
		}

		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(recordKey)
			tx.Delete(key)
			return nil
		})
	}

	if rec.Failure != "" {
		return nil
	}

	if rec.ready() && s.probe(ctx, rec.Port) == nil {
		return nil
	}

	spec, err := engineSpec(common.Value(rec.Status.EngineVersion))
	if err != nil {
		return s.fail(ctx, scope, name, resource, rec, err.Error())
	}

	spec.Owner = s.owner
	spec.ARN, spec.Name, spec.Volume = resource.ARN, resource.Name, resource.Volume
	ensureCtx, cancel := context.WithTimeout(ctx, ensureTimeout)
	started, err := s.runtime.Ensure(ensureCtx, spec)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err != nil {
		return s.fail(ctx, scope, name, resource, rec, "container did not start: "+err.Error())
	}

	since := rec.Since
	if rec.Container != started.ID {
		since = time.Now().UTC()
	}

	probeErr := s.probe(ctx, started.Port)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if probeErr != nil && time.Since(since) > startTimeout {
		reason := fmt.Sprintf("engine not ready after %s: %v", startTimeout, probeErr)
		return s.fail(ctx, scope, name, resource, rec, reason)
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, found, readErr := storage.Read[domainRecord](tx, recordKey)
		if readErr != nil || !found || common.Value(current.Status.Deleted) {
			return readErr
		}

		current.Container, current.Port, current.Since = started.ID, started.Port, since
		current.Status.Processing, current.Status.Endpoint = new(true), nil
		current.Status.DomainProcessingStatus = new("Creating")
		if probeErr == nil {
			current.Initialized = true
			current.Status.Processing = new(false)
			current.Status.DomainProcessingStatus = new("Active")
			current.Status.Endpoint = new(fmt.Sprintf("127.0.0.1:%d", started.Port))
		}

		return storage.Write(tx, recordKey, *current)
	})
}

func (s *Service) probe(ctx context.Context, port int64) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	return s.runtime.Probe(ctx, port)
}

// fail removes a domain's container, keeps its volume if an engine ever ran on it, and records why it has no
// endpoint. The domain stays until it is deleted.
func (s *Service) fail(
	ctx context.Context,
	scope request.Scope,
	name string,
	resource *Spec,
	rec *domainRecord,
	reason string,
) error {
	s.logError("opensearch domain failed", errors.New(reason), "domain", name)
	removeErr := s.runtime.Remove(ctx, resource, !rec.Initialized)
	key := scope.Key(serviceName, domainKind, name)
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, found, readErr := storage.Read[domainRecord](tx, key)
		if readErr != nil || !found || common.Value(current.Status.Deleted) {
			return readErr
		}

		current.Container, current.Port, current.Failure = "", 0, reason
		current.Status.Processing, current.Status.Endpoint = new(false), nil
		tx.Delete(indexKey(scope, name))

		return storage.Write(tx, key, *current)
	})
	if err != nil {
		return err
	}

	return removeErr
}
