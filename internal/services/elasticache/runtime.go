package elasticache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/valkeydocker"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	indexPrefix = "aws/elasticache/index/"
	tick        = time.Second
)

// ErrGone is returned when a container no longer exists.
var ErrGone = valkeydocker.ErrGone

// Runtime runs cache node containers.
type Runtime interface {
	Start(context.Context, *NodeSpec) (Started, error)
	Running(context.Context, string) (bool, error)
	Remove(context.Context, string)
	ListOwned(context.Context, string, string, string) (map[string]string, error)
	Close()
}

// NodeSpec defines a container to run for a cache node.
type NodeSpec = valkeydocker.NodeSpec

// Started holds a container ID and published port.
type Started = valkeydocker.Started

// UseRuntime enables container execution for cache clusters.
func (s *Service) UseRuntime(rt Runtime, logger *slog.Logger) error {
	owner, err := dockercli.RuntimeOwner(s.store, "aws/elasticache/runtime-owner")
	if err != nil {
		return err
	}

	s.owner = owner
	s.runtime = rt
	s.logger = logger
	if s.bg == nil {
		s.bg = background.New()
	}

	s.bg.Every(tick, s.visit)
	return nil
}

func indexClusterKey(scope request.Scope, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + resKindCluster + "/" + strings.ToLower(id)
}

func indexServerlessKey(scope request.Scope, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + resKindServerlessCache + "/" + strings.ToLower(id)
}

func parseIndexKey(k string) (scope request.Scope, kind, id string, ok bool) {
	rest := strings.TrimPrefix(k, indexPrefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 4 {
		return request.Scope{}, "", "", false
	}

	return request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2], parts[3], true
}

func (s *Service) visit(ctx context.Context) {
	if s.runtime == nil {
		return
	}

	s.mu.Lock()
	needOrphanClean := !s.orphansDone
	s.orphansDone = true
	s.mu.Unlock()

	if needOrphanClean {
		s.cleanOrphans(ctx)
	}

	var keys []string
	if err := s.store.View(ctx, func(tx storage.ReadTx) error {
		keys = tx.Keys(indexPrefix)
		return nil
	}); err != nil {
		return
	}

	for _, k := range keys {
		s.mu.Lock()
		if s.busy[k] {
			s.mu.Unlock()
			continue
		}

		s.busy[k] = true
		s.mu.Unlock()

		s.bg.Go(func(ctx context.Context) {
			defer func() {
				s.mu.Lock()
				delete(s.busy, k)
				s.mu.Unlock()
			}()

			if err := s.advance(ctx, k); err != nil && ctx.Err() == nil {
				if s.logger != nil {
					s.logger.Error("elasticache advance failed", "key", k, "err", err)
				}
			}
		})
	}
}

func (s *Service) cleanOrphans(ctx context.Context) {
	containers, err := s.runtime.ListOwned(ctx, "tinystack.elasticache", "tinystack.elasticache.owner", s.owner)
	if err != nil || len(containers) == 0 {
		return
	}

	for id, arn := range containers {
		scope, kind, name, parseErr := parseARN(arn)
		if parseErr != nil {
			s.runtime.Remove(ctx, id)
			continue
		}

		exists := false
		_ = s.store.View(ctx, func(tx storage.ReadTx) error {
			switch kind {
			case resKindCluster:
				_, found, _ := storage.Read[clusterRecord](tx, clusterKey(scope, name))
				exists = found
			case resKindServerlessCache:
				_, found, _ := storage.Read[serverlessCacheRecord](tx, serverlessCacheKey(scope, name))
				exists = found
			default:
				exists = false
			}

			return nil
		})

		if !exists {
			s.runtime.Remove(ctx, id)
		}
	}
}

func (s *Service) advance(ctx context.Context, k string) error {
	scope, kind, id, ok := parseIndexKey(k)
	if !ok {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(k)
			return nil
		})
	}

	switch kind {
	case resKindCluster:
		return s.advanceCluster(ctx, scope, id, k)
	case resKindServerlessCache:
		return s.advanceServerless(ctx, scope, id, k)
	default:
		return nil
	}
}

func (s *Service) advanceCluster(ctx context.Context, scope request.Scope, clusterID, idxKey string) error {
	var record *clusterRecord
	var found bool
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		record, found, readErr = storage.Read[clusterRecord](tx, clusterKey(scope, clusterID))
		return readErr
	})
	if err != nil {
		return err
	}

	if !found {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(idxKey)
			return nil
		})
	}

	switch record.Status {
	case statusDeleting:
		for _, n := range record.Nodes {
			if n.ContainerID != "" {
				s.runtime.Remove(ctx, n.ContainerID)
			}
		}

		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(clusterKey(scope, clusterID))
			tx.Delete(idxKey)
			return nil
		})

	case statusRebooting:
		for i := range record.Nodes {
			n := &record.Nodes[i]
			if n.ContainerID != "" {
				s.runtime.Remove(ctx, n.ContainerID)
				n.ContainerID = ""
			}

			n.Status = statusCreating
		}

		record.Status = statusModifying
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			return storage.Write(tx, clusterKey(scope, clusterID), *record)
		})

	case statusCreating, statusModifying:
		return s.advanceCreatingCluster(ctx, scope, clusterID, record)

	case statusAvailable:
		return s.advanceAvailableCluster(ctx, scope, clusterID, record)

	default:
		return nil
	}
}

func (s *Service) advanceCreatingCluster(
	ctx context.Context,
	scope request.Scope,
	clusterID string,
	record *clusterRecord,
) error {
	spec, err := resolveEngine(record.Engine, record.EngineVersion)
	if err != nil {
		return err
	}

	args := s.buildParamArgs(ctx, scope, record.Engine, record.ParameterGroupName)
	containerPort := int64(6379)
	if record.Engine == EngineMemcached {
		containerPort = 11211
	}

	var extraArgs []string
	network := ""
	alias := ""

	if record.ReplicationGroupID != "" {
		network = fmt.Sprintf(
			"tinystack-elasticache-%s-%s-%s",
			s.owner+"-"+scope.AccountID,
			scope.Region,
			record.ReplicationGroupID,
		)
		alias = record.ClusterID

		var repGroup *replicationGroupRecord
		_ = s.store.View(ctx, func(tx storage.ReadTx) error {
			rg, found, _ := storage.Read[replicationGroupRecord](
				tx,
				replicationGroupKey(scope, record.ReplicationGroupID),
			)
			if found {
				repGroup = rg
			}

			return nil
		})

		if repGroup != nil {
			if repGroup.AuthToken != "" {
				extraArgs = append(extraArgs, "--requirepass", repGroup.AuthToken)
			}

			if record.ClusterID != repGroup.PrimaryClusterID {
				extraArgs = append(extraArgs, "--replicaof", repGroup.PrimaryClusterID, "6379")
				if repGroup.AuthToken != "" {
					extraArgs = append(extraArgs, "--masterauth", repGroup.AuthToken)
				}
			}
		}
	}

	changed := false
	for i := range record.Nodes {
		n := &record.Nodes[i]
		if n.ContainerID == "" {
			nodeSpec := &NodeSpec{
				Labels: map[string]string{
					"tinystack.elasticache.owner": s.owner,
					"tinystack.elasticache":       clusterARN(scope, clusterID),
					"tinystack.node":              n.NodeID,
				},
				Image:   spec.Image,
				Port:    containerPort,
				Args:    append(append([]string{}, args...), extraArgs...),
				Network: network,
				Alias:   alias,
			}

			started, startErr := s.runtime.Start(ctx, nodeSpec)
			if startErr != nil {
				record.Status = statusCreateFailed
				s.recordEvent(
					ctx,
					scope,
					clusterID,
					sourceTypeCacheCluster,
					fmt.Sprintf("Cluster %s failed to start node %s: %v", clusterID, n.NodeID, startErr),
				)
				return s.store.Update(ctx, func(tx storage.WriteTx) error {
					return storage.Write(tx, clusterKey(scope, clusterID), *record)
				})
			}

			n.ContainerID = started.ID
			n.Port = started.Port
			n.Status = statusCreating
			changed = true
		}

		if n.Status != statusAvailable {
			probeErr := probeNode(ctx, record.Engine, "127.0.0.1", n.Port, record.AuthToken, 2*time.Second)
			if probeErr == nil {
				n.Status = statusAvailable
				changed = true
			} else if time.Since(record.CreateTime) > 2*time.Minute {
				record.Status = statusCreateFailed
				s.recordEvent(
					ctx,
					scope,
					clusterID,
					sourceTypeCacheCluster,
					fmt.Sprintf("Cluster %s failed health probe after 2 minutes", clusterID),
				)
				return s.store.Update(ctx, func(tx storage.WriteTx) error {
					return storage.Write(tx, clusterKey(scope, clusterID), *record)
				})
			}
		}
	}

	allAvail := true
	for i := range record.Nodes {
		if record.Nodes[i].Status != statusAvailable {
			allAvail = false
			break
		}
	}

	if allAvail {
		record.Status = statusAvailable
		if record.ParameterApplyStatus == "applying" || record.ParameterApplyStatus == "pending-reboot" {
			record.ParameterApplyStatus = applyStatusInSync
		}

		s.recordEvent(
			ctx,
			scope,
			clusterID,
			sourceTypeCacheCluster,
			fmt.Sprintf("Cluster %s is available", clusterID),
		)
		changed = true

		if record.ReplicationGroupID != "" {
			_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
				rg, found, _ := storage.Read[replicationGroupRecord](
					tx,
					replicationGroupKey(scope, record.ReplicationGroupID),
				)
				if found && rg.Status == statusCreating {
					allMembersAvail := true
					for _, mID := range rg.MemberClusterIDs {
						if mID == clusterID {
							continue
						}

						cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID))
						if !clFound || cl.Status != statusAvailable {
							allMembersAvail = false
							break
						}
					}

					if allMembersAvail {
						rg.Status = statusAvailable
						_ = storage.Write(tx, replicationGroupKey(scope, record.ReplicationGroupID), *rg)
					}
				}

				return nil
			})
		}
	}

	if changed {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			return storage.Write(tx, clusterKey(scope, clusterID), *record)
		})
	}

	return nil
}

func (s *Service) advanceAvailableCluster(
	ctx context.Context,
	scope request.Scope,
	clusterID string,
	record *clusterRecord,
) error {
	for i := range record.Nodes {
		n := &record.Nodes[i]
		if n.ContainerID != "" {
			running, runErr := s.runtime.Running(ctx, n.ContainerID)
			if errors.Is(runErr, ErrGone) || !running {
				n.Status = statusCreating
				n.ContainerID = ""
				record.Status = statusModifying
				s.recordEvent(
					ctx,
					scope,
					clusterID,
					sourceTypeCacheCluster,
					fmt.Sprintf("Node %s stopped unexpectedly, restarting", n.NodeID),
				)
				return s.store.Update(ctx, func(tx storage.WriteTx) error {
					return storage.Write(tx, clusterKey(scope, clusterID), *record)
				})
			}
		}
	}

	return nil
}

func (s *Service) buildParamArgs(
	ctx context.Context,
	scope request.Scope,
	engine, paramGroupName string,
) []string {
	if isDefaultParameterGroup(paramGroupName) {
		return nil
	}

	var params map[string]string
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		pg, found, err := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, paramGroupName))
		if err == nil && found {
			params = pg.UserValues
		}

		return nil
	})

	var args []string
	switch engine {
	case EngineRedis, EngineValkey:
		for k, v := range params {
			args = append(args, "--"+k, v)
		}
	case EngineMemcached:
		for k, v := range params {
			flags, _ := memcachedParamFlags(k, v)
			args = append(args, flags...)
		}
	}

	return args
}

func memcachedParamFlags(name, val string) ([]string, error) {
	switch name {
	case "max_item_size":
		return []string{"-I", val}, nil
	case "max_simultaneous_connections_per_node":
		return []string{"-c", val}, nil
	case "cas_disabled":
		if val == "1" || strings.EqualFold(val, "true") {
			return []string{"-C"}, nil
		}

		return nil, nil
	case "chunk_size":
		return []string{"-s", val}, nil
	default:
		return nil, invalidParameterValue("parameter %s has no Memcached flag mapping", name)
	}
}

func (s *Service) advanceServerless(ctx context.Context, scope request.Scope, name, idxKey string) error {
	var record *serverlessCacheRecord
	var found bool
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		record, found, readErr = storage.Read[serverlessCacheRecord](tx, serverlessCacheKey(scope, name))
		return readErr
	})
	if err != nil {
		return err
	}

	if !found {
		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(idxKey)
			return nil
		})
	}

	switch record.Status {
	case statusDeleting:
		if record.Node.ContainerID != "" {
			s.runtime.Remove(ctx, record.Node.ContainerID)
		}

		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(serverlessCacheKey(scope, name))
			tx.Delete(idxKey)
			return nil
		})

	case statusCreating, statusModifying:
		return s.advanceCreatingServerless(ctx, scope, name, record)

	case statusAvailable:
		return s.advanceAvailableServerless(ctx, scope, name, record)
	}

	return nil
}

func (s *Service) advanceCreatingServerless(
	ctx context.Context,
	scope request.Scope,
	name string,
	record *serverlessCacheRecord,
) error {
	spec, err := resolveEngine(record.Engine, record.MajorEngineVersion)
	if err != nil {
		return err
	}

	n := &record.Node
	if n.ContainerID == "" {
		started, sErr := s.runtime.Start(ctx, &NodeSpec{
			Labels: map[string]string{
				"tinystack.elasticache.owner": s.owner,
				"tinystack.elasticache":       serverlessCacheARN(scope, name),
				"tinystack.node":              n.NodeID,
			},
			Image: spec.Image,
			Port:  spec.DefaultPort,
		})
		if sErr != nil {
			record.Status = statusCreateFailed
			_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
				return storage.Write(tx, serverlessCacheKey(scope, name), *record)
			})
			s.recordEvent(
				ctx,
				scope,
				name,
				"serverless-cache",
				"Failed to start serverless cache container: "+sErr.Error(),
			)
			return sErr
		}

		n.ContainerID = started.ID
		n.Port = started.Port
		n.Status = statusCreating
		_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
			return storage.Write(tx, serverlessCacheKey(scope, name), *record)
		})
	}

	if probeErr := probeNode(ctx, spec.Engine, localhost, n.Port, "", 2*time.Second); probeErr != nil {
		if time.Since(n.CreateTime) > 2*time.Minute {
			record.Status = statusCreateFailed
			_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
				return storage.Write(tx, serverlessCacheKey(scope, name), *record)
			})
			s.recordEvent(ctx, scope, name, "serverless-cache", "Serverless cache failed health check after 2 minutes")
		}

		return nil
	}

	n.Status = statusAvailable
	record.Status = statusAvailable
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, serverlessCacheKey(scope, name), *record)
	})
}

func (s *Service) advanceAvailableServerless(
	ctx context.Context,
	scope request.Scope,
	name string,
	record *serverlessCacheRecord,
) error {
	n := &record.Node
	if n.ContainerID != "" {
		running, err := s.runtime.Running(ctx, n.ContainerID)
		if err != nil || !running {
			s.runtime.Remove(ctx, n.ContainerID)
			n.ContainerID = ""
			n.Status = statusCreating
			record.Status = statusModifying
			return s.store.Update(ctx, func(tx storage.WriteTx) error {
				return storage.Write(tx, serverlessCacheKey(scope, name), *record)
			})
		}
	}

	return nil
}
