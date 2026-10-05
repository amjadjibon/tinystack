package eks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// indexPrefix holds an entry per cluster the runner owns, by endpoint ID.
	indexPrefix = "aws/eks/index/"
	creating    = "CREATING"
	failed      = "FAILED"
	// tick is how often the runner visits the clusters it owns.
	tick = time.Second
)

// Runtime runs clusters' Kubernetes API servers. The runner calls it outside
// transactions.
type Runtime interface {
	// Start runs a cluster's API server and returns once it serves. Only
	// requests with an EKS token from the cluster's account reach it.
	Start(ctx context.Context, cluster Cluster) (Server, error)
	// Running reports whether a server still runs.
	Running(ctx context.Context, id string) (bool, error)
	// Remove deletes a server.
	Remove(ctx context.Context, id string)
	// Close removes every server the runtime started.
	Close()
}

// Cluster is a cluster whose API server a runtime runs.
type Cluster struct {
	Account string
	Name    string
	// Version is the Kubernetes version, such as "1.34".
	Version string
}

// Server is a running API server: its runtime ID, its HTTPS endpoint, and the
// PEM certificate authority of the endpoint's certificate.
type Server struct {
	ID  string
	URL string
	CA  string
}

// indexEntry finds the cluster an endpoint ID belongs to, and the server the
// runner started for it, which outlives the cluster record.
type indexEntry struct {
	Account string
	Region  string
	Name    string
	Server  string
}

// runner is the state of the goroutine that runs clusters' API servers.
type runner struct {
	ctx     context.Context
	runtime Runtime
	logger  *slog.Logger
	cancel  context.CancelFunc
	// busy holds the endpoint IDs whose servers are starting.
	busy map[string]bool
	wg   sync.WaitGroup
	mu   sync.Mutex
	once sync.Once
}

// UseRuntime runs each cluster's Kubernetes API server in rt, at the endpoint
// the cluster reports. Call it before serving requests; Stop ends the runner
// and closes rt.
func (s *Service) UseRuntime(rt Runtime, logger *slog.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	s.runner = &runner{ctx: ctx, cancel: cancel, runtime: rt, logger: logger, busy: map[string]bool{}}
	s.runner.wg.Go(s.run)
}

// Stop ends the runner and removes every API server.
func (s *Service) Stop() {
	r := s.runner
	if r == nil {
		return
	}

	r.once.Do(func() {
		r.cancel()
		r.wg.Wait()
		r.runtime.Close()
	})
}

func indexKey(id string) string { return indexPrefix + id }

// serve prepares a new cluster to have its API server run: CREATING, and
// indexed for the runner.
func (s *Service) serve(tx storage.WriteTx, scope request.Scope, c *cluster) error {
	if s.runner == nil {
		return nil
	}

	c.Status = creating
	data, err := json.Marshal(indexEntry{Account: scope.AccountID, Region: scope.Region, Name: c.Name})
	if err != nil {
		return err
	}

	tx.Put(indexKey(c.hostID()), data)
	return nil
}

func (s *Service) run() {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.runner.ctx.Done():
			return
		case <-ticker.C:
			s.visit()
		}
	}
}

// visit advances every cluster the runner owns that is not already starting,
// so one slow start holds up only its own cluster.
func (s *Service) visit() {
	r := s.runner
	var ids []string
	if err := s.store.View(r.ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			ids = append(ids, k[len(indexPrefix):])
		}

		return nil
	}); err != nil {
		return
	}

	for _, id := range ids {
		r.mu.Lock()
		if r.busy[id] {
			r.mu.Unlock()
			continue
		}

		r.busy[id] = true
		r.mu.Unlock()
		r.wg.Go(func() {
			defer func() {
				r.mu.Lock()
				delete(r.busy, id)
				r.mu.Unlock()
			}()

			if err := s.advance(id); err != nil && r.ctx.Err() == nil {
				r.logger.Error("eks cluster", "endpoint", id, "error", err)
			}
		})
	}
}

// lookup reads the index entry of an endpoint ID and the cluster it names, if
// it still exists with that ID.
func lookup(tx storage.ReadTx, id string) (*indexEntry, *cluster, error) {
	e, found, err := storage.Read[indexEntry](tx, indexKey(id))
	if err != nil || !found {
		return nil, nil, err
	}

	scope := request.Scope{AccountID: e.Account, Region: e.Region}
	c, found, err := storage.Read[cluster](tx, clusterKey(scope, e.Name))
	if err != nil || !found || c.hostID() != id {
		return e, nil, err
	}

	return e, c, nil
}

// advance starts the API server of a cluster that has none (or lost it, as
// every cluster does when TinyStack restarts), and removes the server of a
// deleted cluster.
func (s *Service) advance(id string) error {
	r := s.runner
	var e *indexEntry
	var c *cluster
	if err := s.store.View(r.ctx, func(tx storage.ReadTx) error {
		var err error
		e, c, err = lookup(tx, id)
		return err
	}); err != nil || e == nil {
		return err
	}

	switch {
	case c == nil:
		if e.Server != "" {
			r.runtime.Remove(r.ctx, e.Server)
		}

		return s.store.Update(r.ctx, func(tx storage.WriteTx) error {
			tx.Delete(indexKey(id))
			return nil
		})
	case c.Status == failed:
		return nil
	case c.Server != nil:
		if running, err := r.runtime.Running(r.ctx, c.Server.ID); err != nil || running {
			return err
		}
	}

	server, startErr := r.runtime.Start(r.ctx, Cluster{Account: e.Account, Name: c.Name, Version: c.Version})
	kept := false
	err := s.store.Update(r.ctx, func(tx storage.WriteTx) error {
		cur, current, err := lookup(tx, id)
		if err != nil || current == nil {
			return err
		}

		scope := request.Scope{AccountID: cur.Account, Region: cur.Region}
		if startErr != nil {
			current.Status = failed
			r.logger.Error("start eks cluster", "cluster", current.Name, "error", startErr)
			return storage.Write(tx, clusterKey(scope, current.Name), current)
		}

		current.Status, current.Server, current.Endpoint = active, &server, server.URL
		current.Authority = base64.StdEncoding.EncodeToString([]byte(server.CA))
		cur.Server, kept = server.ID, true
		if writeErr := storage.Write(tx, indexKey(id), cur); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, clusterKey(scope, current.Name), current)
	})
	if startErr == nil && !kept {
		r.runtime.Remove(context.WithoutCancel(r.ctx), server.ID)
	}

	return err
}
