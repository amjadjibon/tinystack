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

// Engine resolves only explicitly supported versions; no minor version is silently discarded.
func Engine(engine, version string) (image, resolved string, port int64, err error) {
	port = 3306
	if engine == EnginePostgres {
		port = 5432
	}

	for _, item := range standaloneVersions {
		if item.engine != engine {
			continue
		}

		if version == "" {
			version = item.versions[0]
			if engine == EnginePostgres {
				version = "16"
			}
		}

		if slices.Contains(item.versions, version) {
			return engine + ":" + version, version, port, nil
		}
	}

	return "", "", 0, invalid("Unsupported engine or EngineVersion")
}

func validPassword(password string) bool {
	if len(password) < 8 || len(password) > 41 {
		return false
	}

	for _, c := range password {
		if c < 33 || c > 126 || strings.ContainsRune("/@\"", c) {
			return false
		}
	}

	return true
}

func (s *Service) CreateDBInstance(
	ctx context.Context,
	in *api.CreateDBInstanceMessage,
) (*api.CreateDBInstanceResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.Engine) == EngineDocDB || common.Value(in.Engine) == EngineNeptune ||
		aurora(common.Value(in.Engine)) {
		return s.createClusterInstance(ctx, in)
	}

	if in.DBClusterIdentifier != nil {
		return nil, invalid("DBClusterIdentifier requires Engine docdb or neptune")
	}

	id := strings.ToLower(common.Value(in.DBInstanceIdentifier))
	if !validName(id, 63) {
		return nil, invalid("Invalid DBInstanceIdentifier")
	}

	engine := common.Value(in.Engine)
	_, version, port, err := Engine(engine, common.Value(in.EngineVersion))
	if err != nil {
		return nil, err
	}

	username, password := common.Value(in.MasterUsername), common.Value(in.MasterUserPassword)
	maxUser := 16
	if engine == EnginePostgres {
		maxUser = 63
	}

	if !sqlName.MatchString(username) || len(username) > maxUser || !validPassword(password) {
		return nil, invalid("Invalid MasterUsername or MasterUserPassword")
	}

	dbName := common.Value(in.DBName)
	if dbName != "" && (!sqlName.MatchString(dbName) || len(dbName) > 63) {
		return nil, invalid("Invalid DBName")
	}

	class := common.Value(in.DBInstanceClass)
	if !strings.HasPrefix(class, "db.") || len(class) < 4 {
		return nil, invalid("DBInstanceClass is required")
	}

	size := common.Value(common.OrElse(in.AllocatedStorage, new(int64(20))))
	if size < 20 || size > maxInstanceStorage {
		return nil, invalid("AllocatedStorage must be between 20 and 65536 GiB")
	}

	window, _, err := maintenanceWindow(in.PreferredMaintenanceWindow, time.Now())
	if err != nil {
		return nil, err
	}

	backup, err := backupWindow(in.PreferredBackupWindow)
	if err != nil {
		return nil, err
	}

	retention := common.OrElse(in.BackupRetentionPeriod, new(int64(1)))
	if retentionErr := checkInstanceRetention(*retention, false, false); retentionErr != nil {
		return nil, retentionErr
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	r := &record{
		Name: id, Kind: kindDB, Tags: tags, Password: password, RootPassword: password, Since: now,
		Instance: &api.DBInstance{
			CopyTagsToSnapshot:         in.CopyTagsToSnapshot,
			DbiResourceId:              new("db-" + common.RandomID(26)),
			BackupRetentionPeriod:      retention,
			PreferredMaintenanceWindow: new(window),
			PreferredBackupWindow:      new(backup),
			DBInstanceIdentifier:       new(id),
			DBInstanceClass:            new(class),
			Engine:                     new(engine),
			EngineVersion:              new(version),
			MasterUsername:             new(username),
			DBName:                     in.DBName,
			AllocatedStorage:           new(size),
			DBInstanceStatus:           new(available),
			DBInstanceArn:              new(arn(scope, kindDB, id)),
			InstanceCreateTime:         new(now),
			Endpoint:                   &api.Endpoint{Address: new("127.0.0.1"), Port: new(port)},
		},
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtime != nil {
		r.Instance.DBInstanceStatus = new(creating)
		r.Instance.Endpoint.Port = new(int64(0))
	}

	// Validate group references in the same transaction that creates the instance.
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := scope.Key(serviceName, kindDB, id)
		if _, found, e := storage.Read[record](tx, key); e != nil {
			return e
		} else if found {
			return resourceError(kindDB, "AlreadyExists", id)
		}

		if in.DBSubnetGroupName != nil {
			group, e := readRecord(tx, scope, kindSubnet, common.Value(in.DBSubnetGroupName))
			if e != nil {
				return e
			}

			r.Instance.DBSubnetGroup = group.Subnet
		}

		if e := setInstanceParameterGroup(tx, scope, r.Instance, in.DBParameterGroupName, parameterInSync); e != nil {
			return e
		}

		if e := s.setOptionGroup(tx, scope, r.Instance, in.OptionGroupName); e != nil {
			return e
		}

		if e := writeRecord(tx, scope, r); e != nil {
			return e
		}

		if e := storage.Write(tx, indexPrefix+scope.AccountID+"/"+scope.Region+"/"+id, true); e != nil {
			return e
		}

		return recordEvent(tx, scope, r, eventCreation, "DB instance created")
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDBInstanceResult{DBInstance: instance(r)}, nil
}

func (s *Service) DescribeDBInstances(
	ctx context.Context,
	in *api.DescribeDBInstancesMessage,
) (*api.DBInstanceMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	for _, f := range in.Filters {
		for _, value := range f.Values {
			if strings.ContainsAny(value, "*?") {
				return nil, invalid("Filter wildcards are unsupported")
			}
		}

		if !slices.Contains([]string{instanceFilter, engineFilter, subnetType}, common.Value(f.Name)) ||
			len(f.Values) == 0 {
			return nil, invalid("Unsupported filter or empty filter Values")
		}
	}

	records, err := s.list(ctx, scope, kindDB, common.Value(in.DBInstanceIdentifier))
	if err != nil {
		return nil, err
	}

	selected := make([]*record, 0, len(records))
	for _, r := range records {
		match := true
		for _, f := range in.Filters {
			value := r.Name
			switch common.Value(f.Name) {
			case engineFilter:
				value = common.Value(r.Instance.Engine)
			case subnetType:
				value = ""
				if r.Instance.DBSubnetGroup != nil {
					value = common.Value(r.Instance.DBSubnetGroup.DBSubnetGroupName)
				}
			}

			if !slices.Contains(f.Values, value) {
				match = false
			}
		}

		if match {
			selected = append(selected, r)
		}
	}

	selected, marker, err := pageRecords(selected, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.DBInstanceMessage{Marker: marker}
	for _, r := range selected {
		out.DBInstances = append(out.DBInstances, *instance(r))
	}

	return out, nil
}

func (s *Service) DeleteDBInstance(
	ctx context.Context,
	in *api.DeleteDBInstanceMessage,
) (*api.DeleteDBInstanceResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readRecord(tx, scope, kindDB, common.Value(in.DBInstanceIdentifier))
		return e
	})
	if err != nil {
		return nil, err
	}

	if r.Instance.DBClusterIdentifier != nil {
		if in.FinalDBSnapshotIdentifier != nil || in.DeleteAutomatedBackups != nil {
			return nil, invalid("Cluster members inherit backup and snapshot settings")
		}

		e := s.store.Update(ctx, func(tx storage.WriteTx) error { return deleteClusterMember(tx, scope, r) })
		if e != nil {
			return nil, e
		}

		r.Instance.DBInstanceStatus = new("deleting")
		return &api.DeleteDBInstanceResult{DBInstance: instance(r)}, nil
	}

	if common.Value(in.SkipFinalSnapshot) && in.FinalDBSnapshotIdentifier != nil {
		return nil, invalid("FinalDBSnapshotIdentifier cannot be combined with SkipFinalSnapshot")
	}

	if !common.Value(in.SkipFinalSnapshot) {
		if _, snapshotErr := s.createSnapshot(
			ctx,
			scope,
			r,
			common.Value(in.FinalDBSnapshotIdentifier),
			nil,
			true,
			time.Time{},
		); snapshotErr != nil {
			return nil, snapshotErr
		}
	}

	if s.runtime != nil {
		if removeErr := s.runtime.Remove(ctx, s.spec(ctx, scope, r), true); removeErr != nil {
			return nil, removeErr
		}
	}

	deleteBackups := common.Value(common.OrElse(in.DeleteAutomatedBackups, new(true)))
	if deleteBackups {
		if backupErr := s.deleteBackupSnapshots(ctx, scope, r); backupErr != nil {
			return nil, backupErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if !deleteBackups {
			if backupErr := retainBackupSnapshots(tx, scope, r); backupErr != nil {
				return backupErr
			}
		}

		if linkErr := removeReplicaLinks(tx, scope, r); linkErr != nil {
			return linkErr
		}

		tx.Delete(scope.Key(serviceName, kindDB, r.Name))
		tx.Delete(indexPrefix + scope.AccountID + "/" + scope.Region + "/" + r.Name)
		tx.Delete(maintenanceKey(scope, kindDB, r.Name))
		return recordEvent(tx, scope, r, eventDeletion, "DB instance deleted")
	})
	if err != nil {
		return nil, err
	}

	r.Instance.DBInstanceStatus = new("deleting")
	return &api.DeleteDBInstanceResult{DBInstance: instance(r)}, nil
}

func (s *Service) change(
	ctx context.Context,
	id, action string,
	in *api.ModifyDBInstanceMessage,
) (*api.DBInstance, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var r *record
	err = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var e error; r, e = readRecord(tx, scope, kindDB, id); return e },
	)
	if err != nil {
		return nil, err
	}

	if r.Instance.DBClusterIdentifier != nil {
		return s.changeClusterMember(ctx, scope, r, in)
	}

	status := common.Value(instance(r).DBInstanceStatus)
	expected := available
	if action == actionStart {
		expected = stopped
	}

	if status != expected {
		return nil, awserror.New("InvalidDBInstanceStateFault", "Instance is not "+expected)
	}

	r.Instance = instance(r)
	r.ReadyAt = time.Time{}

	category, message := eventChange, "DB instance modified"
	if in != nil {
		if changeErr := s.modifyInstance(ctx, scope, r, in); changeErr != nil {
			return nil, changeErr
		}
	} else {
		if actionErr := s.instanceAction(ctx, scope, r, action); actionErr != nil {
			return nil, actionErr
		}

		category, message = eventAvailable, "DB instance "+action
		if action == actionRestart {
			message = "DB instance restarted"
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return saveModification(tx, scope, r, category, message)
	})
	if err != nil {
		return nil, err
	}

	return instance(r), nil
}

func setInstanceParameterGroup(
	tx storage.ReadTx, scope request.Scope, db *api.DBInstance, name *string, status string,
) error {
	if name == nil {
		return nil
	}

	group, err := readRecord(tx, scope, kindParameter, *name)
	if err != nil {
		return err
	}

	if parameterFamily(group) != common.Value(db.Engine)+common.Value(db.EngineVersion) {
		return invalid("DBParameterGroupFamily does not match the engine version")
	}

	db.DBParameterGroups = []api.DBParameterGroupStatus{
		{DBParameterGroupName: new(group.Name), ParameterApplyStatus: new(status)},
	}
	return nil
}

func checkInstanceRetention(retention int64, replica, hasReplicas bool) error {
	switch {
	case retention < 0 || retention > 35:
		return invalid("BackupRetentionPeriod must be between 0 and 35")
	case retention > 0 && replica:
		return invalid("Promote the read replica before enabling backups")
	case retention == 0 && hasReplicas:
		return invalid("Cannot disable backups on a source instance with read replicas")
	}

	return nil
}

func (s *Service) modifyInstance(
	ctx context.Context, scope request.Scope, r *record, in *api.ModifyDBInstanceMessage,
) error {
	db := r.Instance
	p := common.OrElse(db.PendingModifiedValues, &api.PendingModifiedValues{})
	if in.DBInstanceClass != nil {
		if !strings.HasPrefix(*in.DBInstanceClass, "db.") || len(*in.DBInstanceClass) < 4 {
			return invalid("Invalid DBInstanceClass")
		}

		p.DBInstanceClass = in.DBInstanceClass
	}

	if in.AllocatedStorage != nil {
		if *in.AllocatedStorage < common.Value(db.AllocatedStorage) || *in.AllocatedStorage > maxInstanceStorage {
			return invalid("AllocatedStorage cannot decrease or exceed 65536 GiB")
		}

		p.AllocatedStorage = in.AllocatedStorage
	}

	if in.BackupRetentionPeriod != nil {
		if err := checkInstanceRetention(
			*in.BackupRetentionPeriod, db.ReadReplicaSourceDBInstanceIdentifier != nil,
			len(db.ReadReplicaDBInstanceIdentifiers) != 0,
		); err != nil {
			return err
		}

		p.BackupRetentionPeriod = in.BackupRetentionPeriod
	}

	if *p != (api.PendingModifiedValues{}) {
		db.PendingModifiedValues = p
	}

	if in.PreferredMaintenanceWindow != nil {
		window, _, err := maintenanceWindow(in.PreferredMaintenanceWindow, time.Now())
		if err != nil {
			return err
		}

		db.PreferredMaintenanceWindow = new(window)
		r.MaintenanceAt = time.Time{}
	}

	if in.PreferredBackupWindow != nil {
		window, err := backupWindow(in.PreferredBackupWindow)
		if err != nil {
			return err
		}

		db.PreferredBackupWindow = new(window)
	}

	if in.CopyTagsToSnapshot != nil {
		db.CopyTagsToSnapshot = in.CopyTagsToSnapshot
	}

	if in.DBParameterGroupName != nil || in.OptionGroupName != nil {
		if err := s.store.View(ctx, func(tx storage.ReadTx) error {
			status := parameterInSync
			if s.runtime != nil {
				status = parameterPending
			}

			if err := setInstanceParameterGroup(tx, scope, db, in.DBParameterGroupName, status); err != nil {
				return err
			}

			if in.OptionGroupName != nil {
				current := common.Value(db.OptionGroupMemberships[0].OptionGroupName)
				if err := s.setOptionGroup(tx, scope, db, in.OptionGroupName); err != nil {
					return err
				}

				if !common.Value(in.ApplyImmediately) {
					deferOptionMembership(db, current)
				}
			}

			return nil
		}); err != nil {
			return err
		}
	}

	if common.Value(in.ApplyImmediately) {
		applyInstancePending(db)
	}

	if in.MasterUserPassword != nil {
		if !validPassword(*in.MasterUserPassword) {
			return invalid("Invalid MasterUserPassword")
		}

		if s.runtime != nil {
			if err := s.runtime.Password(ctx, s.spec(ctx, scope, r), *in.MasterUserPassword); err != nil {
				return err
			}
		}

		r.Password = *in.MasterUserPassword
		if common.Value(db.MasterUsername) == "root" {
			r.RootPassword = r.Password
		}
	}

	return nil
}

func (s *Service) instanceAction(ctx context.Context, scope request.Scope, r *record, action string) error {
	if s.runtime != nil && action != actionStart {
		if err := s.runtime.Action(ctx, s.spec(ctx, scope, r), action); err != nil {
			return err
		}
	}

	switch action {
	case actionStop:
		r.Instance.DBInstanceStatus = new(stopped)
	case actionStart, actionRestart:
		for i := range r.Instance.DBParameterGroups {
			status := parameterInSync
			if s.runtime != nil {
				status = "applying"
			}

			r.Instance.DBParameterGroups[i].ParameterApplyStatus = new(status)
		}

		r.Instance.DBInstanceStatus = new(available)
		if s.runtime != nil {
			r.Instance.DBInstanceStatus = new(creating)
		}
	}

	if action == actionRestart {
		applyInstancePending(r.Instance)
	}

	r.Since = time.Now().UTC()
	return nil
}

func (s *Service) ModifyDBInstance(
	ctx context.Context,
	in *api.ModifyDBInstanceMessage,
) (*api.ModifyDBInstanceResult, error) {
	out, err := s.change(ctx, common.Value(in.DBInstanceIdentifier), "modify", in)
	return &api.ModifyDBInstanceResult{DBInstance: out}, err
}

func (s *Service) StopDBInstance(
	ctx context.Context,
	in *api.StopDBInstanceMessage,
) (*api.StopDBInstanceResult, error) {
	out, err := s.change(ctx, common.Value(in.DBInstanceIdentifier), actionStop, nil)
	return &api.StopDBInstanceResult{DBInstance: out}, err
}

func (s *Service) StartDBInstance(
	ctx context.Context,
	in *api.StartDBInstanceMessage,
) (*api.StartDBInstanceResult, error) {
	out, err := s.change(ctx, common.Value(in.DBInstanceIdentifier), actionStart, nil)
	return &api.StartDBInstanceResult{DBInstance: out}, err
}

func (s *Service) RebootDBInstance(
	ctx context.Context,
	in *api.RebootDBInstanceMessage,
) (*api.RebootDBInstanceResult, error) {
	out, err := s.change(ctx, common.Value(in.DBInstanceIdentifier), actionRestart, nil)
	return &api.RebootDBInstanceResult{DBInstance: out}, err
}
