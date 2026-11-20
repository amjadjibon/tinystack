package rds

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateDBInstanceReadReplica(
	ctx context.Context, in *api.CreateDBInstanceReadReplicaMessage,
) (*api.CreateDBInstanceReadReplicaResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		return nil, invalid("Read replicas are unsupported in Docker mode")
	}

	name := strings.ToLower(common.Value(in.DBInstanceIdentifier))
	sourceID := common.Value(in.SourceDBInstanceIdentifier)
	if !validName(name, 63) || sourceID == "" {
		return nil, invalid("Valid DBInstanceIdentifier and SourceDBInstanceIdentifier are required")
	}

	if strings.HasPrefix(sourceID, "arn:") && !strings.HasPrefix(sourceID, arn(scope, kindDB, "")) {
		return nil, invalid("Cross-account and cross-Region read replicas are unsupported")
	}

	sourceID, err = resourceID(scope, kindDB, sourceID)
	if err != nil {
		return nil, err
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var replica *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		source, readErr := readRecord(tx, scope, kindDB, sourceID)
		if readErr != nil {
			return readErr
		}

		db := instance(source)
		if db.DBClusterIdentifier != nil {
			return invalid("Read replicas require a standalone DB instance")
		}

		backupsDisabled := common.Value(db.BackupRetentionPeriod) == 0
		if p := db.PendingModifiedValues; p != nil && p.BackupRetentionPeriod != nil {
			backupsDisabled = backupsDisabled || *p.BackupRetentionPeriod == 0
		}

		if common.Value(db.DBInstanceStatus) != available || backupsDisabled {
			return awserror.New("InvalidDBInstanceStateFault", "Source instance must be available with backups enabled")
		}

		if _, found, lookupErr := storage.Read[record](tx, scope.Key(serviceName, kindDB, name)); lookupErr != nil {
			return lookupErr
		} else if found {
			return resourceError(kindDB, "AlreadyExists", name)
		}

		if len(db.ReadReplicaDBInstanceIdentifiers) >= 15 {
			return awserror.New("InstanceQuotaExceededFault", "A source instance can have at most 15 read replicas")
		}

		now := time.Now().UTC()
		replica = &record{
			Name: name, Kind: kindDB, Tags: tags, Instance: db, Since: now,
			Password: source.Password, RootPassword: source.RootPassword,
		}
		db.DBInstanceIdentifier, db.DBInstanceArn = new(name), new(arn(scope, kindDB, name))
		db.DbiResourceId, db.InstanceCreateTime = new("db-"+common.RandomID(26)), new(now)
		db.BackupRetentionPeriod, db.PendingModifiedValues = new(int64(0)), nil
		db.ReadReplicaSourceDBInstanceIdentifier, db.ReadReplicaDBInstanceIdentifiers = new(source.Name), nil
		if configureErr := s.configureReplica(tx, scope, db, in); configureErr != nil {
			return configureErr
		}

		source.Instance.ReadReplicaDBInstanceIdentifiers = append(
			source.Instance.ReadReplicaDBInstanceIdentifiers,
			name,
		)
		slices.Sort(source.Instance.ReadReplicaDBInstanceIdentifiers)
		if writeErr := writeRecord(tx, scope, source); writeErr != nil {
			return writeErr
		}

		if writeErr := writeRecord(tx, scope, replica); writeErr != nil {
			return writeErr
		}

		if indexErr := storage.Write(tx, indexPrefix+scope.AccountID+"/"+scope.Region+"/"+name, true); indexErr != nil {
			return indexErr
		}

		return recordEvent(tx, scope, replica, eventCreation, "Read replica created")
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDBInstanceReadReplicaResult{DBInstance: instance(replica)}, nil
}

func (s *Service) configureReplica(
	tx storage.ReadTx, scope request.Scope, db *api.DBInstance, in *api.CreateDBInstanceReadReplicaMessage,
) error {
	if in.DBInstanceClass != nil {
		if !strings.HasPrefix(*in.DBInstanceClass, "db.") || len(*in.DBInstanceClass) < 4 {
			return invalid("Invalid DBInstanceClass")
		}

		db.DBInstanceClass = in.DBInstanceClass
	}

	if in.AllocatedStorage != nil {
		if *in.AllocatedStorage < common.Value(db.AllocatedStorage) || *in.AllocatedStorage > 65536 {
			return invalid("AllocatedStorage cannot be less than the source or exceed 65536 GiB")
		}

		db.AllocatedStorage = in.AllocatedStorage
	}

	if in.DBSubnetGroupName != nil {
		subnet, err := readRecord(tx, scope, kindSubnet, *in.DBSubnetGroupName)
		if err != nil {
			return err
		}

		db.DBSubnetGroup = subnet.Subnet
	}

	if err := setInstanceParameterGroup(tx, scope, db, in.DBParameterGroupName, parameterInSync); err != nil {
		return err
	}

	db.CopyTagsToSnapshot = common.OrElse(in.CopyTagsToSnapshot, db.CopyTagsToSnapshot)
	return s.setOptionGroup(
		tx,
		scope,
		db,
		common.OrElse(in.OptionGroupName, db.OptionGroupMemberships[0].OptionGroupName),
	)
}

func detachReplica(tx storage.WriteTx, scope request.Scope, r *record) error {
	if sourceID := r.Instance.ReadReplicaSourceDBInstanceIdentifier; sourceID != nil {
		source, err := readRecord(tx, scope, kindDB, *sourceID)
		if err != nil {
			return err
		}

		source.Instance.ReadReplicaDBInstanceIdentifiers = slices.DeleteFunc(
			source.Instance.ReadReplicaDBInstanceIdentifiers, func(id string) bool { return id == r.Name },
		)
		r.Instance.ReadReplicaSourceDBInstanceIdentifier = nil
		return writeRecord(tx, scope, source)
	}

	return nil
}

func (s *Service) PromoteReadReplica(
	ctx context.Context, in *api.PromoteReadReplicaMessage,
) (*api.PromoteReadReplicaResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		return nil, invalid("Read replicas are unsupported in Docker mode")
	}

	retention := common.OrElse(in.BackupRetentionPeriod, new(int64(1)))
	if retentionErr := checkInstanceRetention(*retention, false, false); retentionErr != nil {
		return nil, retentionErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	out := &api.PromoteReadReplicaResult{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindDB, common.Value(in.DBInstanceIdentifier))
		if readErr != nil {
			return readErr
		}

		if r.Instance.ReadReplicaSourceDBInstanceIdentifier == nil ||
			common.Value(instance(r).DBInstanceStatus) != available {
			return awserror.New("InvalidDBInstanceStateFault", "Instance must be an available read replica")
		}

		if detachErr := detachReplica(tx, scope, r); detachErr != nil {
			return detachErr
		}

		r.Instance.BackupRetentionPeriod = retention
		if p := r.Instance.PendingModifiedValues; p != nil {
			p.BackupRetentionPeriod = nil
			if *p == (api.PendingModifiedValues{}) {
				r.Instance.PendingModifiedValues = nil
			}
		}

		out.DBInstance = instance(r)
		return saveModification(tx, scope, r, eventReplica, "Read replica promoted")
	})
	return out, err
}

func removeReplicaLinks(tx storage.WriteTx, scope request.Scope, r *record) error {
	if err := detachReplica(tx, scope, r); err != nil {
		return err
	}

	for _, id := range r.Instance.ReadReplicaDBInstanceIdentifiers {
		replica, err := readRecord(tx, scope, kindDB, id)
		if err != nil {
			return err
		}

		replica.Instance.ReadReplicaSourceDBInstanceIdentifier = nil
		if err := saveModification(
			tx,
			scope,
			replica,
			eventReplica,
			"Read replica promoted after source deletion",
		); err != nil {
			return err
		}
	}

	r.Instance.ReadReplicaDBInstanceIdentifiers = nil
	return nil
}
