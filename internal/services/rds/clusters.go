package rds

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	engineDocDB     = "docdb"
	engineNeptune   = "neptune"
	clusterIDFilter = "db-cluster-id"
)

var clusterUsername = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]*$`)

func clusterEngine(engine, version string) (resolved string, port int64, err error) {
	var versions []string
	switch engine {
	case engineDocDB:
		versions, port = []string{"5.0", "4.0", "3.6"}, 27017
	case engineNeptune:
		versions, port = []string{"1.3.2.0", "1.2.1.0"}, 8182
	default:
		return "", 0, invalid("DB clusters support only docdb and neptune; Aurora is unsupported")
	}

	if version == "" {
		version = versions[0]
	}

	if !slices.Contains(versions, version) {
		return "", 0, invalid("Unsupported EngineVersion")
	}

	return version, port, nil
}

// DocumentDB credentials are validated and discarded: there is no database runtime to use them.
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
	if len(value) < 8 || len(value) > 100 {
		return invalid("MasterUserPassword must contain 8 to 100 characters")
	}

	for _, ch := range value {
		if ch < 32 || ch > 126 || strings.ContainsRune(`/"@`, ch) {
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

	if engine == engineDocDB {
		username := common.Value(c.MasterUsername)
		if !clusterUsername.MatchString(username) || len(username) > 63 || strings.EqualFold(username, "admin") {
			return invalid("Invalid MasterUsername (admin is reserved by DocumentDB)")
		}
	} else if c.MasterUsername != nil {
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

	if err := checkClusterPassword(engine, in.MasterUserPassword, true); err != nil {
		return nil, err
	}

	tags, tagsErr := parseTags(in.Tags)
	if tagsErr != nil {
		return nil, tagsErr
	}

	id := strings.ToLower(common.Value(in.DBClusterIdentifier))
	suffix := common.RandomID(12)
	c := &api.DBCluster{
		DBClusterIdentifier: new(id), Engine: new(engine), EngineVersion: new(version),
		MasterUsername: in.MasterUsername, Port: common.OrElse(in.Port, new(port)), DBSubnetGroup: in.DBSubnetGroupName,
		VpcSecurityGroups:     clusterGroups(in.VpcSecurityGroupIds),
		BackupRetentionPeriod: common.OrElse(in.BackupRetentionPeriod, new(int64(1))),
		StorageEncrypted:      common.OrElse(in.StorageEncrypted, new(false)), KmsKeyId: in.KmsKeyId,
		DeletionProtection: common.OrElse(in.DeletionProtection, new(false)), Status: new(available),
		Endpoint:            new(fmt.Sprintf("%s.cluster-%s.%s.%s.amazonaws.com", id, suffix, scope.Region, engine)),
		ReaderEndpoint:      new(fmt.Sprintf("%s.cluster-ro-%s.%s.%s.amazonaws.com", id, suffix, scope.Region, engine)),
		DbClusterResourceId: new("cluster-" + common.RandomID(26, common.AlphabetUpperHex)),
		DBClusterArn:        new(arn(scope, kindCluster, id)), ClusterCreateTime: new(time.Now().UTC()),
	}

	if c.DBSubnetGroup != nil {
		c.DBSubnetGroup = new(strings.ToLower(*c.DBSubnetGroup))
	}

	if err := checkCluster(c); err != nil {
		return nil, err
	}

	r := &record{Name: id, Kind: kindCluster, Tags: tags, Cluster: common.Clone(c)}
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

		return writeRecord(tx, scope, r)
	})
	if writeErr != nil {
		return nil, writeErr
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
	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	var result *api.DBCluster
	s.mu.Lock()
	defer s.mu.Unlock()
	writeErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readRecord(tx, scope, kindCluster, common.Value(in.DBClusterIdentifier))
		if err != nil {
			return err
		}

		c := r.Cluster
		if in.EngineVersion != nil && *in.EngineVersion != common.Value(c.EngineVersion) {
			return invalid("EngineVersion changes are unsupported")
		}

		if err := checkClusterPassword(common.Value(c.Engine), in.MasterUserPassword, false); err != nil {
			return err
		}

		if in.BackupRetentionPeriod != nil {
			c.BackupRetentionPeriod = in.BackupRetentionPeriod
		}

		if in.Port != nil {
			c.Port = in.Port
		}

		if in.VpcSecurityGroupIds != nil {
			c.VpcSecurityGroups = clusterGroups(in.VpcSecurityGroupIds)
		}

		if in.DeletionProtection != nil {
			c.DeletionProtection = in.DeletionProtection
		}

		if err := checkCluster(c); err != nil {
			return err
		}

		if err := checkClusterNetwork(tx, scope, c); err != nil {
			return err
		}

		// Keep member endpoints consistent with the cluster's port in the same transaction.
		for _, member := range c.DBClusterMembers {
			db, err := readRecord(tx, scope, kindDB, common.Value(member.DBInstanceIdentifier))
			if err != nil {
				return err
			}

			db.Instance.Endpoint.Port = c.Port
			if err := writeRecord(tx, scope, db); err != nil {
				return err
			}
		}

		if err := writeRecord(tx, scope, r); err != nil {
			return err
		}

		result = cluster(r)
		return nil
	})
	if writeErr != nil {
		return nil, writeErr
	}

	return &api.ModifyDBClusterResult{DBCluster: result}, nil
}

func (s *Service) DeleteDBCluster(
	ctx context.Context,
	in *api.DeleteDBClusterMessage,
) (*api.DeleteDBClusterResult, error) {
	if !common.Value(in.SkipFinalSnapshot) {
		return nil, invalid("Snapshots are unsupported; set SkipFinalSnapshot=true")
	}

	if in.FinalDBSnapshotIdentifier != nil {
		return nil, invalid("FinalDBSnapshotIdentifier conflicts with SkipFinalSnapshot")
	}

	scope, scopeErr := request.RegionalScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	var result *api.DBCluster
	s.mu.Lock()
	defer s.mu.Unlock()
	writeErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readRecord(tx, scope, kindCluster, common.Value(in.DBClusterIdentifier))
		if err != nil {
			return err
		}

		if common.Value(r.Cluster.DeletionProtection) || len(r.Cluster.DBClusterMembers) > 0 {
			return awserror.New(
				"InvalidDBClusterStateFault",
				"Disable deletion protection and delete member instances first",
			)
		}

		tx.Delete(scope.Key(serviceName, kindCluster, r.Name))
		result = cluster(r)
		result.Status = new("deleting")
		return nil
	})
	if writeErr != nil {
		return nil, writeErr
	}

	return &api.DeleteDBClusterResult{DBCluster: result}, nil
}
