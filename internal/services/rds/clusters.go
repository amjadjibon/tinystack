package rds

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	EngineDocDB     = "docdb"
	engineDocDB     = EngineDocDB
	EngineNeptune   = "neptune"
	engineNeptune   = EngineNeptune
	clusterIDFilter = "db-cluster-id"
)

var clusterUsername = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]*$`)

func clusterEngine(engine, version string) (resolved string, port int64, err error) {
	var versions []string
	switch engine {
	case EngineAuroraPostgres:
		versions, port = []string{"17.7", "16.11", "16.6", "15.10"}, 5432
	case EngineAuroraMySQL:
		versions, port = []string{"8.0.mysql_aurora.3.12.0", "8.0.mysql_aurora.3.11.1", "8.0.mysql_aurora.3.08.0"}, 3306
	case engineDocDB:
		versions, port = []string{"5.0", "4.0", "3.6"}, 27017
	case engineNeptune:
		versions, port = []string{"1.3.2.0", "1.2.1.0"}, 8182
	default:
		return "", 0, invalid("Unsupported cluster engine")
	}

	if version == "" {
		version = versions[0]
	}

	if !slices.Contains(versions, version) {
		return "", 0, invalid("Unsupported EngineVersion")
	}

	return version, port, nil
}

// Records mode validates DocumentDB credentials without retaining them.
func checkClusterPassword(engine string, password *string, required bool) error {
	if engine == engineNeptune {
		if password != nil {
			return invalid("MasterUserPassword is not supported by Neptune")
		}

		return nil
	}

	if password == nil && !required {
		return nil
	}

	value := common.Value(password)
	maximum := 100
	switch engine {
	case EngineAuroraPostgres:
		maximum = 99
	case EngineAuroraMySQL:
		maximum = 41
	}

	if len(value) < 8 || len(value) > maximum {
		return invalid(fmt.Sprintf("MasterUserPassword must contain 8 to %d characters", maximum))
	}

	for _, ch := range value {
		if ch < 32 || ch > 126 || (aurora(engine) && ch == 32) || strings.ContainsRune(`/"@`, ch) {
			return invalid("MasterUserPassword must use printable ASCII except slash, quote and at sign")
		}
	}

	return nil
}

func checkCluster(c *api.DBCluster) error {
	if !validName(common.Value(c.DBClusterIdentifier), 63) {
		return invalid("Invalid DBClusterIdentifier")
	}

	engine := common.Value(c.Engine)
	if _, _, err := clusterEngine(engine, common.Value(c.EngineVersion)); err != nil {
		return err
	}

	switch {
	case engine == engineDocDB:
		username := common.Value(c.MasterUsername)
		if !clusterUsername.MatchString(username) || len(username) > 63 || strings.EqualFold(username, "admin") {
			return invalid("Invalid MasterUsername (admin is reserved by DocumentDB)")
		}

	case aurora(engine):
		maximum := 16
		if engine == EngineAuroraPostgres {
			maximum = 63
		}

		if !sqlName.MatchString(common.Value(c.MasterUsername)) || len(common.Value(c.MasterUsername)) > maximum {
			return invalid("Invalid MasterUsername")
		}

		if name := common.Value(c.DatabaseName); name != "" && (!sqlName.MatchString(name) || len(name) > 63) {
			return invalid("Invalid DatabaseName")
		}

	case c.MasterUsername != nil:
		return invalid("MasterUsername is not supported by Neptune")
	}

	if port := common.Value(c.Port); port < 1150 || port > 65535 {
		return invalid("Port must be between 1150 and 65535")
	}

	if retention := common.Value(c.BackupRetentionPeriod); retention < 1 || retention > 35 {
		return invalid("BackupRetentionPeriod must be between 1 and 35")
	}

	if c.KmsKeyId != nil && (!common.Value(c.StorageEncrypted) || *c.KmsKeyId == "") {
		return invalid("KmsKeyId requires StorageEncrypted=true and a nonempty key identifier")
	}

	for _, group := range c.VpcSecurityGroups {
		if common.Value(group.VpcSecurityGroupId) == "" {
			return invalid("VpcSecurityGroupIds must not contain empty IDs")
		}
	}

	return nil
}

// Validate referenced subnet/security groups in the transaction that writes the candidate.
func checkClusterNetwork(tx storage.ReadTx, scope request.Scope, c *api.DBCluster) error {
	vpc := ""
	if c.DBSubnetGroup != nil {
		group, err := readRecord(tx, scope, kindSubnet, *c.DBSubnetGroup)
		if err != nil {
			return err
		}

		vpc = common.Value(group.Subnet.VpcId)
	}

	for _, group := range c.VpcSecurityGroups {
		// EC2 security-group records expose VPCID; no external I/O inside this transaction.
		g, found, err := storage.Read[struct{ VPCID string }](tx,
			scope.Key("ec2", "security-group", common.Value(group.VpcSecurityGroupId)))
		if err != nil {
			return err
		}

		if !found {
			return invalid("VpcSecurityGroupIds must reference existing EC2 security groups")
		}

		if vpc != "" && g.VPCID != vpc {
			return invalid("Security groups and subnet group must share a VPC")
		}

		vpc = g.VPCID
	}

	return nil
}

func clusterGroups(ids []string) []api.VpcSecurityGroupMembership {
	out := make([]api.VpcSecurityGroupMembership, 0, len(ids))
	for _, id := range ids {
		out = append(out, api.VpcSecurityGroupMembership{VpcSecurityGroupId: new(id), Status: new("active")})
	}

	return out
}

func cluster(r *record) *api.DBCluster {
	out := common.Clone(r.Cluster)
	out.TagList = tagList(r.Tags)
	if !r.ReadyAt.IsZero() && !time.Now().Before(r.ReadyAt) {
		out.Status = new(available)
	}

	return out
}

func (s *Service) CreateDBCluster(
	ctx context.Context,
	in *api.CreateDBClusterMessage,
) (*api.CreateDBClusterResult, error) {
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	engine := common.Value(in.Engine)
	version, port, versionErr := clusterEngine(engine, common.Value(in.EngineVersion))
	if versionErr != nil {
		return nil, versionErr
	}

	if !aurora(engine) && !(engine == engineDocDB && s.runtime != nil) && in.ManageMasterUserPassword != nil {
		return nil, invalid("ManageMasterUserPassword requires Aurora")
	}

	if common.Value(in.ManageMasterUserPassword) && (engine == engineNeptune || in.MasterUserPassword != nil) {
		return nil, invalid("ManageMasterUserPassword requires Aurora and conflicts with MasterUserPassword")
	}

	if !aurora(engine) &&
		(in.DatabaseName != nil || in.EnableHttpEndpoint != nil || in.ServerlessV2ScalingConfiguration != nil) {
		return nil, invalid("DatabaseName, HTTP endpoint and serverless scaling require Aurora")
	}

	if err := checkClusterPassword(
		engine,
		in.MasterUserPassword,
		!common.Value(in.ManageMasterUserPassword),
	); err != nil {
		return nil, err
	}

	tags, tagsErr := parseTags(in.Tags)
	if tagsErr != nil {
		return nil, tagsErr
	}

	scaling, scalingErr := scalingInfo(in.ServerlessV2ScalingConfiguration)
	if scalingErr != nil {
		return nil, scalingErr
	}

	id := strings.ToLower(common.Value(in.DBClusterIdentifier))
	suffix := common.RandomID(12)
	c := &api.DBCluster{
		DBClusterIdentifier:              new(id),
		Engine:                           new(engine),
		EngineVersion:                    new(version),
		DatabaseName:                     in.DatabaseName,
		HttpEndpointEnabled:              in.EnableHttpEndpoint,
		ServerlessV2ScalingConfiguration: scaling,
		MasterUsername:                   in.MasterUsername,
		Port:                             common.OrElse(in.Port, new(port)),
		DBSubnetGroup:                    in.DBSubnetGroupName,
		VpcSecurityGroups:                clusterGroups(in.VpcSecurityGroupIds),
		BackupRetentionPeriod:            common.OrElse(in.BackupRetentionPeriod, new(int64(1))),
		StorageEncrypted:                 common.OrElse(in.StorageEncrypted, new(false)),
		KmsKeyId:                         in.KmsKeyId,
		DeletionProtection:               common.OrElse(in.DeletionProtection, new(false)),
		Status:                           new(available),
		Endpoint: new(
			fmt.Sprintf("%s.cluster-%s.%s.%s.amazonaws.com", id, suffix, scope.Region, engine),
		),
		ReaderEndpoint: new(
			fmt.Sprintf("%s.cluster-ro-%s.%s.%s.amazonaws.com", id, suffix, scope.Region, engine),
		),
		DbClusterResourceId: new("cluster-" + common.RandomID(26, common.AlphabetUpperHex)),
		DBClusterArn:        new(arn(scope, kindCluster, id)),
		ClusterCreateTime:   new(time.Now().UTC()),
	}

	if c.DBSubnetGroup != nil {
		c.DBSubnetGroup = new(strings.ToLower(*c.DBSubnetGroup))
	}

	if err := checkCluster(c); err != nil {
		return nil, err
	}

	r := &record{
		Name:    id,
		Kind:    kindCluster,
		Tags:    tags,
		Cluster: common.Clone(c),
		Password: common.Value(
			in.MasterUserPassword,
		),
		RootPassword: common.Value(in.MasterUserPassword),
		Since:        time.Now().UTC(),
	}

	if !aurora(engine) && (s.runtime == nil || engine == engineNeptune) {
		r.Password, r.RootPassword = "", ""
	}

	if s.runtime != nil {
		if validator, ok := s.runtime.(interface {
			Validate(context.Context, *Spec) error
		}); ok {
			if err := validator.Validate(ctx, s.clusterSpec(scope, r)); err != nil {
				return nil, invalid(err.Error())
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	writeErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found, err := storage.Read[record](tx, scope.Key(serviceName, kindCluster, id)); err != nil {
			return err
		} else if found {
			return resourceError(kindCluster, "AlreadyExists", id)
		}

		if err := checkClusterNetwork(tx, scope, c); err != nil {
			return err
		}

		if aurora(engine) || s.runtime != nil {
			if common.Value(in.ManageMasterUserPassword) {
				r.Password = common.RandomID(32)
				r.RootPassword = r.Password
			}

			r.Cluster.Status = new(creating)
			if s.runtime == nil {
				r.ReadyAt = time.Now().Add(snapshotDelay)
			} else {
				r.Cluster.Endpoint, r.Cluster.ReaderEndpoint, r.Cluster.Port = nil, nil, new(int64(0))
			}

			if err := storage.Write(tx, clusterIndex+scope.AccountID+"/"+scope.Region+"/"+id, true); err != nil {
				return err
			}
		}

		return writeRecord(tx, scope, r)
	})

	if writeErr != nil {
		return nil, writeErr
	}

	if common.Value(in.ManageMasterUserPassword) {
		if err := s.createClusterSecret(ctx, scope, r); err != nil {
			_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
				tx.Delete(scope.Key(serviceName, kindCluster, id))
				tx.Delete(clusterIndex + scope.AccountID + "/" + scope.Region + "/" + id)
				return nil
			})

			return nil, err
		}
	}

	return &api.CreateDBClusterResult{DBCluster: cluster(r)}, nil
}

func (s *Service) DescribeDBClusters(
	ctx context.Context,
	in *api.DescribeDBClustersMessage,
) (*api.DBClusterMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	for _, f := range in.Filters {
		if !slices.Contains([]string{clusterIDFilter, "db-cluster-resource-id", engineFilter}, common.Value(f.Name)) ||
			len(f.Values) == 0 {
			return nil, invalid("Unsupported filter or empty filter Values")
		}

		for _, value := range f.Values {
			if strings.ContainsAny(value, "*?") {
				return nil, invalid("Filter wildcards are unsupported")
			}
		}
	}

	name := common.Value(in.DBClusterIdentifier)
	if strings.HasPrefix(name, "arn:") {
		prefix := arn(scope, kindCluster, "")
		if !strings.HasPrefix(name, prefix) {
			return nil, resourceError(kindCluster, "NotFound", name)
		}

		name = strings.TrimPrefix(name, prefix)
	}

	records, err := s.list(ctx, scope, kindCluster, name)
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
				value = common.Value(r.Cluster.Engine)
			case "db-cluster-resource-id":
				value = common.Value(r.Cluster.DbClusterResourceId)
			}

			if !slices.Contains(f.Values, value) &&
				(common.Value(f.Name) != clusterIDFilter || !slices.Contains(f.Values, common.Value(r.Cluster.DBClusterArn))) {
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

	out := &api.DBClusterMessage{Marker: marker}
	for _, r := range selected {
		out.DBClusters = append(out.DBClusters, *cluster(r))
	}

	return out, nil
}

func (s *Service) ModifyDBCluster(
	ctx context.Context,
	in *api.ModifyDBClusterMessage,
) (*api.ModifyDBClusterResult, error) {
	out, err := s.modifyCluster(ctx, in)
	return &api.ModifyDBClusterResult{DBCluster: out}, err
}

func (s *Service) DeleteDBCluster(
	ctx context.Context,
	in *api.DeleteDBClusterMessage,
) (*api.DeleteDBClusterResult, error) {
	out, err := s.deleteCluster(ctx, in)
	if err == nil && s.dataClosed != nil {
		s.dataClosed(common.Value(out.DBClusterArn))
	}

	return &api.DeleteDBClusterResult{DBCluster: out}, err
}
