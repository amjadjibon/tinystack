package rds

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	EngineAuroraPostgres = "aurora-postgresql"
	EngineAuroraMySQL    = "aurora-mysql"
	clusterIndex         = "aws/rds/cluster-index/"
	kindClusterSnapshot  = "cluster-snapshot"
)

func aurora(engine string) bool { return engine == EngineAuroraPostgres || engine == EngineAuroraMySQL }

//nolint:nilnil // An absent optional scaling configuration has no output value.
func scalingInfo(in *api.ServerlessV2ScalingConfiguration) (*api.ServerlessV2ScalingConfigurationInfo, error) {
	if in == nil {
		return nil, nil
	}

	minimum, maximum := common.Value(in.MinCapacity), common.Value(in.MaxCapacity)
	if in.MinCapacity == nil || in.MaxCapacity == nil || minimum < 0 || maximum < 0.5 || maximum > 256 ||
		minimum > maximum ||
		minimum*2 != float64(int64(minimum*2)) ||
		maximum*2 != float64(int64(maximum*2)) {
		return nil, invalid("Scaling capacities must be half-ACU increments, 0 <= MinCapacity <= MaxCapacity <= 256")
	}

	if in.SecondsUntilAutoPause != nil &&
		(minimum != 0 || *in.SecondsUntilAutoPause < 300 || *in.SecondsUntilAutoPause > 86400) {
		return nil, invalid("SecondsUntilAutoPause requires MinCapacity=0 and must be between 300 and 86400")
	}

	return &api.ServerlessV2ScalingConfigurationInfo{
		MinCapacity:           in.MinCapacity,
		MaxCapacity:           in.MaxCapacity,
		SecondsUntilAutoPause: in.SecondsUntilAutoPause,
	}, nil
}

func (s *Service) createClusterSecret(ctx context.Context, scope request.Scope, r *record) error {
	if s.caller == nil {
		return invalid("Secrets Manager is unavailable")
	}

	value, err := json.Marshal(
		map[string]string{"username": common.Value(r.Cluster.MasterUsername), "password": r.Password},
	)
	if err != nil {
		return err
	}

	out, err := s.caller.Call(ctx, "secretsmanager", "CreateSecret", map[string]any{
		"Name": "rds!cluster-" + common.RandomID(24), "SecretString": string(value),
	})
	if err != nil {
		return err
	}

	secretARN, ok := out["ARN"].(string)
	if !ok {
		return invalid("Secrets Manager returned no ARN")
	}

	r.Cluster.MasterUserSecret = &api.MasterUserSecret{SecretArn: new(secretARN), SecretStatus: new("active")}
	return s.store.Update(ctx, func(tx storage.WriteTx) error { return writeRecord(tx, scope, r) })
}

func (s *Service) clusterSpec(ctx context.Context, scope request.Scope, r *record) *Spec {
	c := r.Cluster
	engine, image, port := EngineMySQL, "mysql:8.0", int64(3306)
	if common.Value(c.Engine) == EngineAuroraPostgres {
		engine, image, port = EnginePostgres, "postgres:"+strings.Split(common.Value(c.EngineVersion), ".")[0], 5432
	}

	if common.Value(c.Engine) == EngineDocDB {
		engine, image, port = EngineDocDB, "mongo:"+common.Value(c.EngineVersion), 27017
	}

	if common.Value(c.Engine) == EngineNeptune {
		// AWS release compatibility: 1.2.1.0 uses TinkerPop 3.6.2; 1.3.2.0 uses 3.7.1.
		// https://docs.aws.amazon.com/neptune/latest/userguide/engine-releases-1.2.1.0.html
		// https://docs.aws.amazon.com/neptune/latest/userguide/engine-releases-1.3.2.0.html
		engine, image, port = EngineNeptune, "tinkerpop/gremlin-server:3.7.1", 8182
		if common.Value(c.EngineVersion) == "1.2.1.0" {
			image = "tinkerpop/gremlin-server:3.6.2"
		}
	}

	resourceARN := arn(scope, r.Kind, r.Name)
	hash := sha256.Sum256([]byte(s.owner + resourceARN))
	name := fmt.Sprintf("tinystack-rds-%x", hash[:16])
	database := common.Value(c.DatabaseName)
	if database == "" {
		database = strings.ReplaceAll(common.Value(c.DBClusterIdentifier), "-", "_")
	}

	spec := &Spec{
		Parameters: s.runtimeParameters(ctx, scope, r),
		Owner:      s.owner,
		ARN:        resourceARN,
		Name:       name,
		Volume:     name + "-data",
		Image:      image,
		Engine:     engine,
		Username: common.Value(
			c.MasterUsername,
		),
		Password:     r.Password,
		RootPassword: r.RootPassword,
		Database:     database,
		Port:         port,
	}

	if r.Kind == kindClusterSnapshot {
		spec.Name = ""
	}

	return spec
}

func writeCluster(tx storage.WriteTx, scope request.Scope, r *record) error {
	for _, member := range r.Cluster.DBClusterMembers {
		db, err := readRecord(tx, scope, kindDB, common.Value(member.DBInstanceIdentifier))
		if err != nil {
			return err
		}

		db.Instance.Endpoint = &api.Endpoint{Address: r.Cluster.Endpoint, Port: r.Cluster.Port}
		db.Instance.DBInstanceStatus = cluster(r).Status
		db.ReadyAt = r.ReadyAt
		if err := writeRecord(tx, scope, db); err != nil {
			return err
		}
	}

	return writeRecord(tx, scope, r)
}

func (s *Service) visitClusters(ctx context.Context) {
	var keys []string
	if err := s.store.View(
		ctx,
		func(tx storage.ReadTx) error { keys = tx.Keys(clusterIndex); return nil },
	); err != nil {
		return
	}

	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}

		parts := strings.Split(strings.TrimPrefix(key, clusterIndex), "/")
		if len(parts) != 3 {
			continue
		}

		scope := request.Scope{AccountID: parts[0], Region: parts[1]}
		var r *record
		err := s.store.View(ctx, func(tx storage.ReadTx) error {
			var e error
			r, e = readRecord(tx, scope, kindCluster, parts[2])
			return e
		})

		if err != nil || common.Value(r.Cluster.Status) == stopped || common.Value(r.Cluster.Status) == failed {
			continue
		}

		previousStatus := common.Value(r.Cluster.Status)
		spec := s.clusterSpec(ctx, scope, r)
		ioCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		started, err := s.runtime.Ensure(ioCtx, spec)
		cancel()
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			r.Cluster.Status = new(failed)
			_ = s.runtime.Remove(ctx, spec, !r.Initialized)
		} else {
			r.Container, r.Volume = started.ID, spec.Volume
			r.Cluster.Endpoint, r.Cluster.ReaderEndpoint, r.Cluster.Port = new(
				"127.0.0.1",
			), new(
				"127.0.0.1",
			), new(
				started.Port,
			)
			probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
			err = s.runtime.Probe(probeCtx, spec, started)
			probeCancel()
			if err == nil {
				r.Initialized, r.Cluster.Status = true, new(available)
			} else {
				r.Cluster.Status = new(creating)
				if previousStatus == available {
					r.Since = time.Now().UTC()
				}

				if time.Since(r.Since) > 2*time.Minute {
					r.Cluster.Status = new(failed)
					_ = s.runtime.Remove(ctx, spec, !r.Initialized)
				}
			}
		}

		_ = s.store.Update(ctx, func(tx storage.WriteTx) error { return writeCluster(tx, scope, r) })
	}
}

func (s *Service) loadCluster(ctx context.Context, scope request.Scope, id string) (*record, error) {
	id, err := resourceID(scope, kindCluster, id)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var e error; r, e = readRecord(tx, scope, kindCluster, id); return e },
	)
	return r, err
}

func (s *Service) modifyCluster(ctx context.Context, in *api.ModifyDBClusterMessage) (*api.DBCluster, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.loadCluster(ctx, scope, common.Value(in.DBClusterIdentifier))
	if err != nil {
		return nil, err
	}

	c := r.Cluster
	if in.DBClusterParameterGroupName != nil {
		c.DBClusterParameterGroup = in.DBClusterParameterGroupName
	}

	if (aurora(common.Value(c.Engine)) || s.runtime != nil) && in.ApplyImmediately != nil && !*in.ApplyImmediately {
		return nil, invalid("Deferred modifications are unsupported")
	}

	if in.EngineVersion != nil && *in.EngineVersion != common.Value(c.EngineVersion) {
		return nil, invalid("EngineVersion changes are unsupported")
	}

	if !aurora(common.Value(c.Engine)) && (in.EnableHttpEndpoint != nil || in.ServerlessV2ScalingConfiguration != nil) {
		return nil, invalid("HTTP endpoint and serverless scaling require Aurora")
	}

	if in.Port != nil && aurora(common.Value(c.Engine)) {
		return nil, invalid("Changing Aurora ports is unsupported")
	}

	if in.Port != nil && s.runtime != nil && !aurora(common.Value(c.Engine)) {
		_, port, _ := clusterEngine(common.Value(c.Engine), common.Value(c.EngineVersion))
		if *in.Port != port {
			return nil, invalid("Changing cluster engine ports is unsupported in Docker mode")
		}
	}

	if in.MasterUserPassword != nil && c.MasterUserSecret != nil {
		return nil, invalid("Password is managed by Secrets Manager")
	}

	if passwordErr := checkClusterPassword(common.Value(c.Engine), in.MasterUserPassword, false); passwordErr != nil {
		return nil, passwordErr
	}

	if in.BackupRetentionPeriod != nil {
		c.BackupRetentionPeriod = in.BackupRetentionPeriod
	}

	if in.Port != nil && s.runtime == nil {
		c.Port = in.Port
	}

	if in.VpcSecurityGroupIds != nil {
		c.VpcSecurityGroups = clusterGroups(in.VpcSecurityGroupIds)
	}

	if in.DeletionProtection != nil {
		c.DeletionProtection = in.DeletionProtection
	}

	if in.EnableHttpEndpoint != nil {
		c.HttpEndpointEnabled = in.EnableHttpEndpoint
	}

	if in.ServerlessV2ScalingConfiguration != nil {
		c.ServerlessV2ScalingConfiguration, err = scalingInfo(in.ServerlessV2ScalingConfiguration)
		if err != nil {
			return nil, err
		}
	}

	// The host port is assigned by Docker; validate the configured engine port instead.
	candidate := common.Clone(c)
	if aurora(common.Value(c.Engine)) || s.runtime != nil {
		_, port, _ := clusterEngine(common.Value(c.Engine), common.Value(c.EngineVersion))
		candidate.Port = new(port)
	}

	if err := checkCluster(candidate); err != nil {
		return nil, err
	}

	if err := s.store.View(
		ctx,
		func(tx storage.ReadTx) error { return checkClusterNetwork(tx, scope, c) },
	); err != nil {
		return nil, err
	}

	if in.MasterUserPassword != nil && (aurora(common.Value(c.Engine)) || s.runtime != nil) {
		if s.runtime != nil {
			if err := s.runtime.Password(ctx, s.clusterSpec(ctx, scope, r), *in.MasterUserPassword); err != nil {
				return nil, err
			}
		}

		r.Password = *in.MasterUserPassword
		if common.Value(c.MasterUsername) == "root" {
			r.RootPassword = r.Password
		}
	}

	if err := s.store.Update(ctx, func(tx storage.WriteTx) error { return writeCluster(tx, scope, r) }); err != nil {
		return nil, err
	}

	return cluster(r), nil
}

func (s *Service) changeCluster(ctx context.Context, id, action string) (*api.DBCluster, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.loadCluster(ctx, scope, id)
	if err != nil {
		return nil, err
	}

	if !aurora(common.Value(r.Cluster.Engine)) && s.runtime == nil {
		return nil, invalid("Start/stop requires Aurora")
	}

	expected := available
	if action == actionStart {
		expected = stopped
	}

	if common.Value(cluster(r).Status) != expected {
		return nil, awserror.New("InvalidDBClusterStateFault", "Cluster is not "+expected)
	}

	if s.runtime != nil && action != actionStart {
		if err := s.runtime.Action(ctx, s.clusterSpec(ctx, scope, r), action); err != nil {
			return nil, err
		}
	}

	r.ReadyAt, r.Since = time.Time{}, time.Now().UTC()
	r.Cluster.Status = new(stopped)
	if action == actionStart {
		r.Cluster.Status = new(creating)
		if s.runtime == nil {
			r.ReadyAt = time.Now().Add(snapshotDelay)
		}
	}

	if err := s.store.Update(ctx, func(tx storage.WriteTx) error { return writeCluster(tx, scope, r) }); err != nil {
		return nil, err
	}

	return cluster(r), nil
}

func (s *Service) StartDBCluster(
	ctx context.Context,
	in *api.StartDBClusterMessage,
) (*api.StartDBClusterResult, error) {
	out, err := s.changeCluster(ctx, common.Value(in.DBClusterIdentifier), actionStart)
	return &api.StartDBClusterResult{DBCluster: out}, err
}

// UseDataCloser closes ephemeral Data API sessions after lifecycle changes, outside the RDS lock.
func (s *Service) UseDataCloser(closeCluster func(string)) { s.dataClosed = closeCluster }

func (s *Service) StopDBCluster(ctx context.Context, in *api.StopDBClusterMessage) (*api.StopDBClusterResult, error) {
	out, err := s.changeCluster(ctx, common.Value(in.DBClusterIdentifier), "stop")
	if err == nil && s.dataClosed != nil {
		s.dataClosed(common.Value(out.DBClusterArn))
	}

	return &api.StopDBClusterResult{DBCluster: out}, err
}

func (s *Service) httpEndpoint(ctx context.Context, resourceARN string, enabled bool) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	id, err := resourceID(scope, kindCluster, resourceARN)
	if err != nil {
		return err
	}

	_, err = s.modifyCluster(
		ctx,
		&api.ModifyDBClusterMessage{DBClusterIdentifier: new(id), EnableHttpEndpoint: new(enabled)},
	)
	return err
}

func (s *Service) EnableHttpEndpoint(
	ctx context.Context,
	in *api.EnableHttpEndpointRequest,
) (*api.EnableHttpEndpointResponse, error) {
	err := s.httpEndpoint(ctx, common.Value(in.ResourceArn), true)
	return &api.EnableHttpEndpointResponse{ResourceArn: in.ResourceArn, HttpEndpointEnabled: new(true)}, err
}

func (s *Service) DisableHttpEndpoint(
	ctx context.Context,
	in *api.DisableHttpEndpointRequest,
) (*api.DisableHttpEndpointResponse, error) {
	err := s.httpEndpoint(ctx, common.Value(in.ResourceArn), false)
	return &api.DisableHttpEndpointResponse{ResourceArn: in.ResourceArn, HttpEndpointEnabled: new(false)}, err
}

func (s *Service) deleteCluster(ctx context.Context, in *api.DeleteDBClusterMessage) (*api.DBCluster, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.loadCluster(ctx, scope, common.Value(in.DBClusterIdentifier))
	if err != nil {
		return nil, err
	}

	if common.Value(r.Cluster.DeletionProtection) || len(r.Cluster.DBClusterMembers) > 0 {
		return nil, awserror.New(
			"InvalidDBClusterStateFault",
			"Disable deletion protection and delete member instances first",
		)
	}

	if common.Value(in.SkipFinalSnapshot) && in.FinalDBSnapshotIdentifier != nil {
		return nil, invalid("FinalDBSnapshotIdentifier conflicts with SkipFinalSnapshot")
	}

	if !common.Value(in.SkipFinalSnapshot) {
		if _, err = s.createClusterSnapshot(
			ctx,
			scope,
			r,
			common.Value(in.FinalDBSnapshotIdentifier),
			nil,
		); err != nil {
			return nil, err
		}
	}

	if s.runtime != nil {
		if removeErr := s.runtime.Remove(ctx, s.clusterSpec(ctx, scope, r), true); removeErr != nil {
			return nil, removeErr
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(scope.Key(serviceName, kindCluster, r.Name))
		tx.Delete(clusterIndex + scope.AccountID + "/" + scope.Region + "/" + r.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}

	if r.Cluster.MasterUserSecret != nil && s.caller != nil {
		if _, secretErr := s.caller.Call(ctx, "secretsmanager", "DeleteSecret", map[string]any{
			"SecretId": common.Value(r.Cluster.MasterUserSecret.SecretArn), "ForceDeleteWithoutRecovery": true,
		}); secretErr != nil {
			return nil, secretErr
		}
	}

	out := cluster(r)
	out.Status = new("deleting")
	return out, nil
}

// DataEngine returns a private connection specification only for an enabled, running Aurora cluster.
func (s *Service) DataEngine(ctx context.Context, resourceARN string) (*Spec, int64, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.HasPrefix(resourceARN, arn(scope, kindCluster, "")) {
		return nil, 0, awserror.New("NotFoundException", "Cluster not found")
	}

	r, err := s.loadCluster(ctx, scope, resourceARN)
	if err != nil {
		return nil, 0, awserror.New("NotFoundException", "Cluster not found")
	}

	if !aurora(common.Value(r.Cluster.Engine)) || !common.Value(r.Cluster.HttpEndpointEnabled) {
		return nil, 0, awserror.New("HttpEndpointNotEnabledException", "HTTP endpoint is not enabled")
	}

	if s.runtime == nil {
		return s.clusterSpec(ctx, scope, r), 0, nil
	}

	if common.Value(r.Cluster.Status) != available {
		return nil, 0, awserror.New("DatabaseUnavailableException", "Cluster has no running engine")
	}

	return s.clusterSpec(ctx, scope, r), common.Value(r.Cluster.Port), nil
}
