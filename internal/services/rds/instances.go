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
	switch engine {
	case EnginePostgres:
		if version == "" {
			version = "16"
		}

		if slices.Contains([]string{"14", "15", "16", "17"}, version) {
			return "postgres:" + version, version, 5432, nil
		}

	case EngineMySQL:
		if version == "" {
			version = mysqlFamilyVersion
		}

		if version == mysqlFamilyVersion {
			return "mysql:8.0", version, 3306, nil
		}

	case EngineMariaDB:
		if version == "" {
			version = "11"
		}

		if version == "11" {
			return "mariadb:11", version, 3306, nil
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
	if size < 20 || size > 65536 {
		return nil, invalid("AllocatedStorage must be between 20 and 65536 GiB")
	}

	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	r := &record{
		Name: id, Kind: kindDB, Tags: tags, Password: password, RootPassword: password, Since: now,
		Instance: &api.DBInstance{
			CopyTagsToSnapshot: in.CopyTagsToSnapshot, DbiResourceId: new("db-" + common.RandomID(26)),
			DBInstanceIdentifier: new(id), DBInstanceClass: new(class), Engine: new(engine),
			EngineVersion: new(version), MasterUsername: new(username), DBName: in.DBName, AllocatedStorage: new(size),
			DBInstanceStatus: new(available), DBInstanceArn: new(arn(scope, kindDB, id)), InstanceCreateTime: new(now),
			Endpoint: &api.Endpoint{Address: new("127.0.0.1"), Port: new(port)},
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

		if in.DBParameterGroupName != nil {
			group, e := readRecord(tx, scope, kindParameter, common.Value(in.DBParameterGroupName))
			if e != nil {
				return e
			}

			if common.Value(group.Parameter.DBParameterGroupFamily) != engine+version {
				return invalid("DBParameterGroupFamily does not match the engine version")
			}

			r.Instance.DBParameterGroups = []api.DBParameterGroupStatus{{
				DBParameterGroupName: new(group.Name),
				ParameterApplyStatus: new(parameterInSync),
			}}
		}

		if e := writeRecord(tx, scope, r); e != nil {
			return e
		}

		return storage.Write(tx, indexPrefix+scope.AccountID+"/"+scope.Region+"/"+id, true)
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
		if in.FinalDBSnapshotIdentifier != nil {
			return nil, invalid("Cluster member final snapshots are unsupported")
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
		); snapshotErr != nil {
			return nil, snapshotErr
		}
	}

	if s.runtime != nil {
		if removeErr := s.runtime.Remove(ctx, s.spec(ctx, scope, r), true); removeErr != nil {
			return nil, removeErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, kindDB, r.Name))
		tx.Delete(indexPrefix + scope.AccountID + "/" + scope.Region + "/" + r.Name)
		return nil
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

	if in != nil {
		if in.DBParameterGroupName != nil {
			err = s.store.View(ctx, func(tx storage.ReadTx) error {
				group, e := readRecord(tx, scope, kindParameter, *in.DBParameterGroupName)
				if e != nil {
					return e
				}

				if parameterFamily(group) != common.Value(r.Instance.Engine)+common.Value(r.Instance.EngineVersion) {
					return invalid("DBParameterGroupFamily does not match engine")
				}

				status := parameterInSync
				if s.runtime != nil {
					status = parameterPending
				}

				r.Instance.DBParameterGroups = []api.DBParameterGroupStatus{
					{DBParameterGroupName: new(group.Name), ParameterApplyStatus: new(status)},
				}

				return nil
			})
			if err != nil {
				return nil, err
			}
		}

		if in.CopyTagsToSnapshot != nil {
			r.Instance.CopyTagsToSnapshot = in.CopyTagsToSnapshot
		}

		if in.ApplyImmediately != nil && !*in.ApplyImmediately {
			return nil, invalid("Deferred modifications are unsupported")
		}

		if in.DBInstanceClass != nil {
			if !strings.HasPrefix(*in.DBInstanceClass, "db.") || len(*in.DBInstanceClass) < 4 {
				return nil, invalid("Invalid DBInstanceClass")
			}

			r.Instance.DBInstanceClass = in.DBInstanceClass
		}

		if in.MasterUserPassword != nil {
			if !validPassword(*in.MasterUserPassword) {
				return nil, invalid("Invalid MasterUserPassword")
			}

			if s.runtime != nil {
				if passwordErr := s.runtime.Password(
					ctx,
					s.spec(ctx, scope, r),
					*in.MasterUserPassword,
				); passwordErr != nil {
					return nil, passwordErr
				}
			}

			r.Password = *in.MasterUserPassword
			if common.Value(r.Instance.MasterUsername) == "root" {
				r.RootPassword = r.Password
			}
		}
	} else {
		if s.runtime != nil && action != actionStart {
			if actionErr := s.runtime.Action(ctx, s.spec(ctx, scope, r), action); actionErr != nil {
				return nil, actionErr
			}
		}

		switch action {
		case "stop":
			r.Instance.DBInstanceStatus = new(stopped)
		case actionStart, "restart":
			for i := range r.Instance.DBParameterGroups {
				status := parameterInSync
				if s.runtime != nil {
					status = "applying"
				}

				r.Instance.DBParameterGroups[i].ParameterApplyStatus = new(status)
			}

			if s.runtime != nil {
				r.Instance.DBInstanceStatus = new(creating)
			}
		}

		r.Since = time.Now().UTC()
		if action == actionStart && s.runtime == nil {
			r.Instance.DBInstanceStatus = new(available)
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error { return writeRecord(tx, scope, r) })
	if err != nil {
		return nil, err
	}

	return instance(r), nil
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
	out, err := s.change(ctx, common.Value(in.DBInstanceIdentifier), "stop", nil)
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
	out, err := s.change(ctx, common.Value(in.DBInstanceIdentifier), "restart", nil)
	return &api.RebootDBInstanceResult{DBInstance: out}, err
}
