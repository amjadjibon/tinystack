package kafka

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Spec identifies one real broker and its persistent data volume.
type Spec struct {
	ARN, Owner, Name, Volume string
	Plain, TLS               bool
}

// Started contains the published loopback listener ports.
type Started struct{ Port, TLSPort int64 }

// Runtime performs Docker I/O outside storage transactions.
type Runtime interface {
	Ensure(context.Context, *Spec) (Started, error)
	Probe(context.Context, *Spec) error
	Resources(context.Context, string) ([]*Spec, error)
	Remove(context.Context, *Spec, bool) error
	Close()
}

type runtimeRecord struct {
	ARN           string
	Port, TLSPort int64
}

const (
	runtimePrefix = "aws/kafka/runtime/"
	ownerKey      = "aws/kafka/store-owner"
)

func runtimeKey(arn string) string { return runtimePrefix + arn }
func resourceSpec(arn, owner string) *Spec {
	hash := sha256.Sum256([]byte(owner + arn))
	name := fmt.Sprintf("tinystack-kafka-%x", hash[:16])
	return &Spec{ARN: arn, Owner: owner, Name: name, Volume: name + "-data"}
}

// UseRuntime recovers cluster records against their volumes and scopes orphan cleanup to this store.
func (s *Service) UseRuntime(rt Runtime, logger *slog.Logger) error {
	err := s.store.Update(context.Background(), func(tx storage.WriteTx) error {
		owner, found, err := storage.Read[string](tx, ownerKey)
		if err != nil {
			return err
		}

		if !found {
			owner = new(common.RandomID(32))
			if err := storage.Write(tx, ownerKey, *owner); err != nil {
				return err
			}
		}

		s.owner = *owner
		for _, key := range tx.Keys("") {
			_, _, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, clusterKind+"/") {
				continue
			}

			r, _, readErr := storage.Read[api.ClusterInfo](tx, key)
			if readErr != nil {
				return readErr
			}

			if authErr := dockerAuthentication(r); authErr != nil {
				return authErr
			}

			r.State = new(statusCreating)
			if err := storage.Write(tx, key, r); err != nil {
				return err
			}

			if err := storage.Write(tx, runtimeKey(*r.ClusterArn), runtimeRecord{ARN: *r.ClusterArn}); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	s.runtime, s.logger = rt, logger
	s.bg = background.New()
	s.bg.Every(time.Second, s.visit)
	return nil
}

func dockerAuthentication(r *api.ClusterInfo) error {
	l := clusterListeners(r)
	if l.iam || l.scram || l.mtls {
		return invalid(
			"Docker clusters support unauthenticated PLAINTEXT, TLS or TLS_PLAINTEXT only; IAM, SCRAM and mTLS are unsupported",
		)
	}

	if !l.plain && !l.tls {
		return invalid("At least one client authentication mode must be enabled")
	}

	return nil
}

func (s *Service) Stop() {
	if s.bg != nil {
		s.bg.Stop()
	}

	if s.runtime != nil {
		s.runtime.Close()
	}
}

func (s *Service) Reset(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime == nil {
		return
	}

	specs, err := s.runtime.Resources(ctx, s.owner)
	if err == nil {
		for _, spec := range specs {
			if err = s.runtime.Remove(ctx, spec, true); err != nil && s.logger != nil {
				s.logger.Error("kafka reset", "err", err)
			}
		}
	}

	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, key := range tx.Keys(runtimePrefix) {
			tx.Delete(key)
		}

		return nil
	})
}

func (s *Service) visit(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []*runtimeRecord
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		records, err = storage.Records[runtimeRecord](tx, runtimePrefix)
		return err
	})
	if err != nil {
		return
	}

	if !s.orphansDone {
		specs, listErr := s.runtime.Resources(ctx, s.owner)
		if listErr != nil {
			return
		}

		known := map[string]bool{}
		for _, r := range records {
			known[r.ARN] = true
		}

		for _, spec := range specs {
			if !known[spec.ARN] {
				if err = s.runtime.Remove(ctx, spec, true); err != nil {
					return
				}
			}
		}

		s.orphansDone = true
	}

	for _, rt := range records {
		if ctx.Err() != nil {
			return
		}

		if err = s.advance(ctx, rt); err != nil && ctx.Err() == nil && s.logger != nil {
			s.logger.Error("kafka runtime", "arn", rt.ARN, "err", err)
		}
	}
}

func (s *Service) advance(ctx context.Context, rt *runtimeRecord) error {
	parts := arnPattern.FindStringSubmatch(rt.ARN)
	if parts == nil {
		return invalid("Invalid stored cluster ARN")
	}

	scope := request.Scope{AccountID: parts[2], Region: parts[1]}
	var r *api.ClusterInfo
	err := s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var err error; r, err = read(tx, scope, rt.ARN); return err },
	)
	if err != nil {
		return err
	}

	spec := resourceSpec(rt.ARN, s.owner)
	l := clusterListeners(r)
	spec.Plain, spec.TLS = l.plain, l.tls
	ensureCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	started, err := s.runtime.Ensure(ensureCtx, spec)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	state := statusCreating
	var info *api.StateInfo
	if err != nil {
		state = "FAILED"
		info = &api.StateInfo{Code: new("DockerStartFailed"), Message: new(err.Error())}
	} else {
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		probeErr := s.runtime.Probe(probeCtx, spec)
		probeCancel()
		if probeErr == nil {
			state = statusActive
		}
	}

	gone := false
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		// DeleteCluster may have run while the broker started.
		if _, found := tx.Get(runtimeKey(rt.ARN)); !found {
			gone = true
			return nil
		}

		current, readErr := read(tx, scope, rt.ARN)
		if readErr != nil {
			return readErr
		}

		current.State, current.StateInfo = new(state), info
		if saveErr := save(tx, scope, current); saveErr != nil {
			return saveErr
		}

		return storage.Write(
			tx,
			runtimeKey(rt.ARN),
			runtimeRecord{ARN: rt.ARN, Port: started.Port, TLSPort: started.TLSPort},
		)
	})
	if err != nil || !gone {
		return err
	}

	return s.runtime.Remove(ctx, spec, true)
}
