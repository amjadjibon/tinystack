package rds

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Spec contains private runtime configuration for one database.
type Spec struct {
	ARN          string
	Name         string
	Volume       string
	Image        string
	Engine       string
	Username     string
	Password     string
	RootPassword string
	Database     string
	Port         int64
}

type Started struct {
	ID   string
	Port int64
}

// Runtime performs container I/O outside storage transactions.
type Runtime interface {
	Resources(context.Context) ([]*Spec, error)
	Copy(context.Context, *Spec, *Spec) error
	Ensure(context.Context, *Spec) (Started, error)
	Probe(context.Context, *Spec, Started) error
	Action(context.Context, *Spec, string) error
	Password(context.Context, *Spec, string) error
	Remove(context.Context, *Spec, bool) error
	Close()
}

func (s *Service) spec(scope request.Scope, r *record) *Spec {
	resourceARN := arn(scope, kindDB, r.Name)
	hash := sha256.Sum256([]byte(resourceARN))
	name := fmt.Sprintf("tinystack-rds-%x", hash[:16])
	image, _, port, _ := Engine(common.Value(r.Instance.Engine), common.Value(r.Instance.EngineVersion))
	database := common.Value(r.Instance.DBName)
	if database == "" {
		database = r.Name
		database = strings.ReplaceAll(database, "-", "_")
	}

	return &Spec{
		ARN: resourceARN, Name: name, Volume: name + "-data", Image: image, Port: port,
		Engine: common.Value(r.Instance.Engine), Username: common.Value(r.Instance.MasterUsername),
		Password: r.Password, RootPassword: r.RootPassword, Database: database,
	}
}

// dataSpec addresses snapshot volumes without referring to a live instance container.
func (s *Service) dataSpec(scope request.Scope, r *record) *Spec {
	spec := s.spec(scope, r)
	if r.Kind == kindSnapshot {
		spec.ARN = arn(scope, kindSnapshot, r.Name)
		hash := sha256.Sum256([]byte(spec.ARN))
		spec.Name = ""
		spec.Volume = fmt.Sprintf("tinystack-rds-snapshot-%x-data", hash[:16])
	}

	return spec
}

func (s *Service) UseRuntime(rt Runtime) error {
	// Persisted endpoints belong to the previous runtime. Invalidate them before
	// serving requests; the poller republishes availability after a fresh probe.
	err := s.store.Update(context.Background(), func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(indexPrefix) {
			parts := strings.Split(strings.TrimPrefix(key, indexPrefix), "/")
			if len(parts) != 3 {
				continue
			}

			scope := request.Scope{AccountID: parts[0], Region: parts[1]}
			r, readErr := readRecord(tx, scope, kindDB, parts[2])
			if readErr != nil {
				return readErr
			}

			r.Container = ""
			r.ReadyAt = time.Time{}
			r.Instance.Endpoint = nil
			status := common.Value(r.Instance.DBInstanceStatus)
			if status != stopped && status != failed {
				r.Instance.DBInstanceStatus = new(creating)
				r.Since = time.Now().UTC()
			}

			if writeErr := writeRecord(tx, scope, r); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("prepare RDS runtime recovery: %w", err)
	}

	s.runtime = rt
	s.bg = background.New()
	s.bg.Every(time.Second, s.visit)
	return nil
}

// Stop waits for background I/O and removes containers, retaining database volumes.
func (s *Service) Stop() {
	if s.bg != nil {
		s.bg.Stop()
	}

	if s.runtime != nil {
		s.runtime.Close()
	}
}

// Reset removes recorded containers and volumes before app.Reset clears the store.
func (s *Service) Reset(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == nil {
		return
	}

	var records []struct {
		r     *record
		scope request.Scope
	}

	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			r, found, err := storage.Read[record](tx, key)
			if err != nil || !found || (r.Kind != kindDB && r.Kind != kindSnapshot) {
				continue
			}

			if (r.Kind == kindSnapshot && r.Volume == "") || r.Instance.DBClusterIdentifier != nil {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}

			records = append(records, struct {
				r     *record
				scope request.Scope
			}{r, scope})
		}

		return nil
	})
	for _, item := range records {
		_ = s.runtime.Remove(ctx, s.dataSpec(item.scope, item.r), true)
	}

	// Drop the index while holding the lifecycle lock so the poller cannot recreate deleted containers.
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(indexPrefix) {
			tx.Delete(key)
		}

		return nil
	})
}

func (s *Service) visit(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.orphansDone {
		resources, err := s.Resources(ctx)
		if err != nil {
			return
		}

		known := make(map[string]bool, len(resources))
		for _, resource := range resources {
			known[resource.ARN] = true
		}

		specs, err := s.runtime.Resources(ctx)
		if err != nil {
			return
		}

		for _, spec := range specs {
			if !known[spec.ARN] {
				if removeErr := s.runtime.Remove(ctx, spec, true); removeErr != nil {
					return
				}
			}
		}

		s.orphansDone = true
	}

	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error { keys = tx.Keys(indexPrefix); return nil }); err != nil {
		return
	}

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		parts := strings.Split(strings.TrimPrefix(key, indexPrefix), "/")
		if len(parts) != 3 {
			continue
		}

		scope := request.Scope{AccountID: parts[0], Region: parts[1]}
		var r *record
		err := s.store.View(
			ctx,
			func(tx storage.ReadTx) error { var e error; r, e = readRecord(tx, scope, kindDB, parts[2]); return e },
		)
		if err != nil {
			continue
		}

		status := common.Value(r.Instance.DBInstanceStatus)
		if status == stopped || status == failed {
			continue
		}

		spec := s.spec(scope, r)
		fresh := !r.Initialized
		ioCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		started, err := s.runtime.Ensure(ioCtx, spec)
		cancel()
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			r.Instance.DBInstanceStatus = new(failed)
			_ = s.runtime.Remove(ctx, spec, fresh)
		} else {
			r.Container = started.ID
			r.Volume = spec.Volume
			r.Instance.Endpoint = &api.Endpoint{Address: new("127.0.0.1"), Port: new(started.Port)}
			probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
			probeErr := s.runtime.Probe(probeCtx, spec, started)
			probeCancel()
			if probeErr == nil {
				r.Initialized = true
				r.Instance.DBInstanceStatus = new(available)
			} else {
				if status == available {
					r.Since = time.Now().UTC()
				}

				r.Instance.DBInstanceStatus = new(creating)
				if time.Since(r.Since) > 2*time.Minute {
					r.Instance.DBInstanceStatus = new(failed)
					_ = s.runtime.Remove(ctx, spec, fresh)
				}
			}
		}

		if ctx.Err() != nil {
			return
		}

		_ = s.store.Update(ctx, func(tx storage.WriteTx) error { return writeRecord(tx, scope, r) })
	}
}
