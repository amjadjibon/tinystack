package mq

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	brokerCreating    = "CREATION_IN_PROGRESS"
	brokerRunning     = "RUNNING"
	brokerRebooting   = "REBOOT_IN_PROGRESS"
	brokerDeleting    = "DELETION_IN_PROGRESS"
	mqLabel           = "tinystack.mq"
	containerResource = "container"
)

type brokerRuntime struct {
	bg     *background.Group
	logger *slog.Logger
	owner  string
	mu     sync.Mutex
}

// UseDocker enables local brokers and recovers persisted records against their volumes.
func (s *Service) UseDocker(logger *slog.Logger) error {
	rt := &brokerRuntime{logger: logger, bg: background.New()}
	err := s.store.Update(context.Background(), func(tx storage.WriteTx) error {
		owner, found, err := storage.Read[string](tx, "aws/mq/runtime-owner")
		if err != nil {
			return err
		}

		if found {
			rt.owner = *owner
		} else {
			rt.owner = common.RandomID(32)
		}

		for _, key := range tx.Keys("") {
			_, _, rest, ok := storage.Scoped(key, serviceName)
			if !ok || !strings.HasPrefix(rest, brokerKind+"/") {
				continue
			}

			r, _, readErr := storage.Read[record](tx, key)
			if readErr != nil {
				return readErr
			}

			if len(r.Credentials) == 0 {
				return fmt.Errorf("existing records-only MQ broker must be recreated for Docker mode")
			}

			if common.Value(r.Broker.BrokerState) != brokerDeleting {
				r.Restarted = false
				if common.Value(r.Broker.BrokerState) != brokerRebooting {
					r.Broker.BrokerState = new(brokerCreating)
				}

				r.Broker.BrokerInstances = nil
			}

			if writeErr := storage.Write(tx, key, r); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, "aws/mq/runtime-owner", rt.owner)
	})
	if err != nil {
		return err
	}

	s.runtime = rt
	// Recovery and orphan cleanup run outside storage transactions.
	rt.bg.Every(time.Second, s.visitBrokers)
	return nil
}

func brokerNames(owner, arn string) (container, volume string) {
	hash := sha256.Sum256([]byte(owner + arn))
	name := fmt.Sprintf("tinystack-mq-%x", hash[:16])
	return name, name + "-data"
}

func (s *Service) runtimeRows(ctx context.Context) (map[string]*record, error) {
	rows := map[string]*record{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, index := range tx.Keys("aws/mq/index/") {
			key := strings.TrimPrefix(index, "aws/mq/index/")
			r, found, err := storage.Read[record](tx, key)
			if err != nil {
				return err
			}

			if found {
				rows[key] = r
			}
		}

		return nil
	})
	return rows, err
}

func (s *Service) visitBrokers(ctx context.Context) {
	rt := s.runtime
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rows, err := s.runtimeRows(ctx)
	if err != nil {
		return
	}

	known := map[string]bool{}
	for _, r := range rows {
		known[*r.Broker.BrokerArn] = true
	}

	// Owner is part of the filter: never touch another store's resources.
	for _, kind := range []string{containerResource, "volume"} {
		var resources map[string]string
		if kind == containerResource {
			resources, err = dockercli.ListLabeled(ctx, mqLabel)
		} else {
			resources, err = dockercli.ListLabeledVolumes(ctx, mqLabel)
		}

		if err != nil {
			return
		}

		for name, identity := range resources {
			owner, arn, _ := strings.Cut(identity, "|")
			if owner == rt.owner && !known[arn] {
				if kind == containerResource {
					_, err = dockercli.Run(ctx, nil, "rm", "-f", name)
				} else {
					_, err = dockercli.Run(ctx, nil, "volume", "rm", name)
				}

				if err != nil {
					return
				}
			}
		}
	}

	for key, r := range rows {
		if ctx.Err() != nil {
			return
		}

		if err = s.advanceBroker(ctx, key, r); err != nil && ctx.Err() == nil && rt.logger != nil {
			rt.logger.Error("mq runtime", "broker", *r.Broker.BrokerId, "err", err)
		}
	}
}

func (s *Service) advanceBroker(ctx context.Context, key string, r *record) error {
	name, volume := brokerNames(s.runtime.owner, *r.Broker.BrokerArn)
	state := common.Value(r.Broker.BrokerState)
	if state == brokerDeleting {
		if err := dockercli.RemoveContainerVolume(ctx, name, volume); err != nil {
			return err
		}

		return s.store.Update(ctx, func(tx storage.WriteTx) error {
			tx.Delete(key)
			tx.Delete("aws/mq/index/" + key)
			return nil
		})
	}

	if state == brokerRebooting && !r.Restarted {
		if _, err := dockercli.Run(ctx, nil, "rm", "-f", name); err != nil && !dockercli.Gone(err) {
			return err
		}

		if r.Broker.PendingEngineVersion != nil {
			r.Broker.EngineVersion = r.Broker.PendingEngineVersion
			r.Broker.PendingEngineVersion = nil
		}
	}

	ports, err := s.ensureBroker(ctx, name, volume, r)
	if err != nil {
		return err
	}

	ready := probeBroker(ctx, r, ports) == nil
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		current, found, err := storage.Read[record](tx, key)
		if err != nil || !found {
			return err
		}

		// A concurrent delete or reboot must win over an earlier probe.
		if common.Value(current.Broker.BrokerState) != state {
			return nil
		}

		if state == brokerRebooting {
			current.Broker.EngineVersion = r.Broker.EngineVersion
			current.Broker.PendingEngineVersion = r.Broker.PendingEngineVersion
			current.Restarted = true
		}

		current.Broker.BrokerState = new(brokerCreating)
		if state == brokerRebooting {
			current.Broker.BrokerState = new(state)
		}

		current.Broker.BrokerInstances = nil
		if ready {
			current.Restarted = false
			current.Broker.BrokerState = new(brokerRunning)
			current.Broker.BrokerInstances = []api.BrokerInstance{brokerEndpoints(*r.Broker.EngineType, ports)}
		}

		return storage.Write(tx, key, current)
	})
}

func (s *Service) Stop() {
	if s.runtime == nil {
		return
	}

	s.runtime.bg.Stop()
	s.runtime.mu.Lock()
	defer s.runtime.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rows, _ := s.runtimeRows(ctx)
	for _, r := range rows {
		name, _ := brokerNames(s.runtime.owner, *r.Broker.BrokerArn)
		_ = dockercli.RemoveContainerVolume(ctx, name, "")
	}
}

func (s *Service) Reset(ctx context.Context) {
	if s.runtime == nil {
		return
	}

	s.runtime.mu.Lock()
	defer s.runtime.mu.Unlock()
	rows, _ := s.runtimeRows(ctx)
	for _, r := range rows {
		name, volume := brokerNames(s.runtime.owner, *r.Broker.BrokerArn)
		_ = dockercli.RemoveContainerVolume(ctx, name, volume)
	}

	// Remove records under the same lock so a poll cannot recreate resources before app reset.
	_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for key := range rows {
			tx.Delete(key)
			tx.Delete("aws/mq/index/" + key)
		}

		return nil
	})
}

func (s *Service) RebootBroker(ctx context.Context, in *api.RebootBrokerRequest) (*api.RebootBrokerResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, common.Value(in.BrokerId))
		if readErr != nil {
			return readErr
		}

		if s.runtime != nil && common.Value(r.Broker.BrokerState) != brokerRunning {
			return invalid("Only a RUNNING broker can reboot")
		}

		if s.runtime == nil {
			applyPending(r)
			return save(tx, scope, r)
		}
		r.Broker.BrokerState = new(brokerRebooting)
		r.Restarted = false
		r.Broker.BrokerInstances = nil
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.RebootBrokerResponse{}, nil
}
