package mq

import (
	"context"
	"maps"
	"reflect"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func applyUpdate(b *api.DescribeBrokerResponse, in *api.UpdateBrokerRequest) {
	if in.AutoMinorVersionUpgrade != nil {
		b.AutoMinorVersionUpgrade = in.AutoMinorVersionUpgrade
	}
	if in.EngineVersion != nil {
		b.EngineVersion = in.EngineVersion
	}
	if in.HostInstanceType != nil {
		b.HostInstanceType = in.HostInstanceType
	}
	if in.Logs != nil {
		// Supplied log flags merge; omitted nested flags retain their previous values.
		if in.Logs.General != nil {
			b.Logs.General = in.Logs.General
		}
		if in.Logs.Audit != nil {
			b.Logs.Audit = in.Logs.Audit
		}
	}
	if in.MaintenanceWindowStartTime != nil {
		b.MaintenanceWindowStartTime = in.MaintenanceWindowStartTime
	}
	if in.SecurityGroups != nil {
		b.SecurityGroups = in.SecurityGroups
	}
}

func updateOutput(b *api.DescribeBrokerResponse) *api.UpdateBrokerResponse {
	return common.Clone(&api.UpdateBrokerResponse{
		BrokerId:                b.BrokerId,
		AutoMinorVersionUpgrade: b.AutoMinorVersionUpgrade, EngineVersion: b.EngineVersion,
		HostInstanceType: b.HostInstanceType, Logs: &api.Logs{General: b.Logs.General, Audit: b.Logs.Audit},
		MaintenanceWindowStartTime: b.MaintenanceWindowStartTime, SecurityGroups: b.SecurityGroups,
	})
}

func (s *Service) update(ctx context.Context, in *api.UpdateBrokerRequest, owned, desired map[string]string,
	restore *api.DescribeBrokerResponse,
) (before, after *api.DescribeBrokerResponse, err error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, nil, e
	}
	id, groups := common.Clone(in.BrokerId), slices.Clone(in.SecurityGroups)
	in = common.Clone(in)
	in.BrokerId, in.SecurityGroups = id, groups
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := read(tx, scope, common.Value(in.BrokerId))
		if readErr != nil {
			return readErr
		}
		if s.runtime != nil && in.EngineVersion != nil && *in.EngineVersion != *r.Broker.EngineVersion {
			if common.Value(r.Broker.BrokerState) != brokerRunning {
				return invalid("Engine updates require a RUNNING Docker broker")
			}

			if !compatibleUpgrade(*r.Broker.EngineType, *r.Broker.EngineVersion, *in.EngineVersion) {
				return invalid("Docker volume upgrade is unsupported for these engine versions; recreate the broker")
			}
		}

		before = common.Clone(r.Broker)
		b := r.Broker
		if restore != nil {
			// Only mutable configuration and tags can be restored. No credentials are
			// needed, and identity/initial-user metadata remain the existing incarnation.
			if !reflect.DeepEqual(b.BrokerId, restore.BrokerId) || !reflect.DeepEqual(b.BrokerArn, restore.BrokerArn) {
				return invalid("Rollback snapshot belongs to a different broker")
			}
			if s.runtime != nil && !reflect.DeepEqual(b.EngineVersion, restore.EngineVersion) {
				return invalid("A completed Docker engine upgrade cannot be rolled back")
			}

			b.AutoMinorVersionUpgrade = restore.AutoMinorVersionUpgrade
			b.EngineVersion = restore.EngineVersion
			b.PendingEngineVersion = restore.PendingEngineVersion
			b.HostInstanceType = restore.HostInstanceType
			b.Logs = restore.Logs
			b.MaintenanceWindowStartTime = restore.MaintenanceWindowStartTime
			b.SecurityGroups = restore.SecurityGroups
			b.Tags = restore.Tags
		} else {
			applyUpdate(b, in)
			if *before.EngineType == activeMQ && *before.EngineVersion == "5.19" && *b.EngineVersion == "5.18" {
				return invalid("Engine version downgrades are unsupported")
			}
			if *before.EngineType == rabbitMQ && *before.HostInstanceType == large && *b.HostInstanceType == micro {
				return invalid("RabbitMQ cannot downgrade from mq.m5.large to mq.t3.micro")
			}
			if desired != nil {
				tags := common.CloneTags(b.Tags)
				for k := range owned {
					if _, ok := desired[k]; !ok {
						delete(tags, k)
					}
				}
				maps.Copy(tags, desired)
				b.Tags = tags
			}
		}
		r.Broker = b
		if configErr := validateConfig(b, s.runtime != nil); configErr != nil {
			return configErr
		}
		if networkErr := s.validateNetwork(tx, scope, b); networkErr != nil {
			return networkErr
		}
		if s.runtime != nil && in.EngineVersion != nil && *in.EngineVersion != *before.EngineVersion {
			b.PendingEngineVersion = b.EngineVersion
			b.EngineVersion = before.EngineVersion
		}

		if saveErr := save(tx, scope, r); saveErr != nil {
			return saveErr
		}
		after = common.Clone(b)
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	return before, after, nil
}

func (s *Service) UpdateBroker(ctx context.Context, in *api.UpdateBrokerRequest) (*api.UpdateBrokerResponse, error) {
	_, b, e := s.update(ctx, in, nil, nil, nil)
	if e != nil {
		return nil, e
	}

	out := updateOutput(b)
	if b.PendingEngineVersion != nil {
		out.EngineVersion = common.Clone(b.PendingEngineVersion)
	}

	return out, nil
}

// CloudFormationUpdate atomically applies configuration and the final tag set.
// The snapshot is the actual live configuration, including externally added tags.
func (s *Service) CloudFormationUpdate(
	ctx context.Context,
	in *api.UpdateBrokerRequest,
	owned, desired map[string]string,
) (
	before, after *api.DescribeBrokerResponse, err error,
) {
	return s.update(ctx, in, owned, desired, nil)
}

// CloudFormationRestore restores a captured live mutable snapshot, permitting a
// compensating metadata version restoration without claiming a runtime downgrade.
func (s *Service) CloudFormationRestore(ctx context.Context, b *api.DescribeBrokerResponse) (
	*api.DescribeBrokerResponse, error,
) {
	_, after, e := s.update(ctx, &api.UpdateBrokerRequest{BrokerId: b.BrokerId}, nil, nil, common.Clone(b))
	return after, e
}
