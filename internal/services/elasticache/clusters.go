package elasticache

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type clusterNode struct {
	CreateTime  time.Time
	NodeID      string
	Status      string
	ContainerID string
	Port        int64
}

type clusterRecord struct {
	CreateTime           time.Time
	ClusterID            string
	Engine               string
	EngineVersion        string
	NodeType             string
	Status               string
	SubnetGroupName      string
	ParameterGroupName   string
	ParameterApplyStatus string
	PreferredAZ          string
	Tags                 map[string]string
	SecurityGroupIDs     []string
	Nodes                []clusterNode
	NumNodes             int64
	Port                 int64
}

func validateClusterID(id string) error {
	if len(id) < 1 || len(id) > 50 {
		return invalidParameterValue("CacheClusterId must be between 1 and 50 characters")
	}

	// Must begin with a letter, contain only lowercase letters, numbers, and hyphens,
	// and not end with a hyphen or have two consecutive hyphens.
	first := id[0]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') {
		return invalidParameterValue("CacheClusterId must begin with a letter")
	}

	for i := range id {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}

		if c == '-' {
			if i > 0 && id[i-1] == '-' {
				return invalidParameterValue("CacheClusterId cannot contain two consecutive hyphens")
			}

			if i == len(id)-1 {
				return invalidParameterValue("CacheClusterId cannot end with a hyphen")
			}

			continue
		}

		return invalidParameterValue("CacheClusterId must contain only letters, numbers, and hyphens")
	}

	// Check for uppercase letters
	for i := range id {
		if id[i] >= 'A' && id[i] <= 'Z' {
			return invalidParameterValue("CacheClusterId cannot contain uppercase letters")
		}
	}

	return nil
}

func (s *Service) verifySecurityGroups(ctx context.Context, sgIDs []string, vpcID string) error {
	if len(sgIDs) == 0 {
		return nil
	}

	out, err := s.call(ctx, "ec2", "DescribeSecurityGroups", map[string]any{
		"GroupId": sgIDs,
	})
	if err != nil {
		return err
	}

	for _, g := range objects(out, "SecurityGroups") {
		if text(g, "VpcId") != vpcID && vpcID != defaultVpcID {
			return invalidParameterValue("Security group '%s' does not belong to VPC '%s'", text(g, "GroupId"), vpcID)
		}
	}

	return nil
}

func toAPICacheCluster(scope request.Scope, r *clusterRecord, showNodes bool) api.CacheCluster {
	arn := clusterARN(scope, r.ClusterID)
	numNodes := r.NumNodes

	var sgMemberships []api.SecurityGroupMembership
	for _, sgID := range r.SecurityGroupIDs {
		status := "active"
		sgMemberships = append(sgMemberships, api.SecurityGroupMembership{
			SecurityGroupId: new(sgID),
			Status:          new(status),
		})
	}

	paramGroup := &api.CacheParameterGroupStatus{
		CacheParameterGroupName: new(r.ParameterGroupName),
		ParameterApplyStatus:    new(r.ParameterApplyStatus),
	}

	var nodes []api.CacheNode
	if showNodes {
		for _, n := range r.Nodes {
			host := "127.0.0.1"
			port := n.Port
			nodes = append(nodes, api.CacheNode{
				CacheNodeId:         new(n.NodeID),
				CacheNodeStatus:     new(n.Status),
				CacheNodeCreateTime: new(n.CreateTime),
				Endpoint: &api.Endpoint{
					Address: new(host),
					Port:    new(port),
				},
				ParameterGroupStatus: new(r.ParameterApplyStatus),
			})
		}
	}

	endpointHost := clusterEndpoint(scope, r.ClusterID)
	cluster := api.CacheCluster{
		CacheClusterId:            new(r.ClusterID),
		Engine:                    new(r.Engine),
		EngineVersion:             new(r.EngineVersion),
		CacheNodeType:             new(r.NodeType),
		CacheClusterStatus:        new(r.Status),
		NumCacheNodes:             new(numNodes),
		CacheClusterCreateTime:    new(r.CreateTime),
		CacheSubnetGroupName:      new(r.SubnetGroupName),
		CacheParameterGroup:       paramGroup,
		SecurityGroups:            sgMemberships,
		CacheNodes:                nodes,
		PreferredAvailabilityZone: new(r.PreferredAZ),
		ARN:                       new(arn),
	}

	if r.Engine == EngineMemcached {
		cfgPort := r.Port
		if len(r.Nodes) > 0 && r.Nodes[0].Port > 0 {
			cfgPort = r.Nodes[0].Port
		}
		cluster.ConfigurationEndpoint = &api.Endpoint{
			Address: new(endpointHost),
			Port:    new(cfgPort),
		}
	}

	return cluster
}

func (s *Service) CreateCacheCluster(
	ctx context.Context,
	in *api.CreateCacheClusterMessage,
) (*api.CreateCacheClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	rawID := common.Value(in.CacheClusterId)
	if vErr := validateClusterID(rawID); vErr != nil {
		return nil, vErr
	}

	clusterID := strings.ToLower(rawID)

	engineName := common.Value(in.Engine)
	engineVer := common.Value(in.EngineVersion)
	spec, err := resolveEngine(engineName, engineVer)
	if err != nil {
		return nil, err
	}

	numNodes := int64(1)
	if in.NumCacheNodes != nil {
		numNodes = *in.NumCacheNodes
	}

	switch spec.Engine {
	case EngineRedis, EngineValkey:
		if numNodes != 1 {
			return nil, invalidParameterValue("NumCacheNodes must be 1 for %s", spec.Description)
		}
	case EngineMemcached:
		if numNodes < 1 || numNodes > 40 {
			return nil, invalidParameterValue("NumCacheNodes must be between 1 and 40 for Memcached")
		}
	}

	nodeType := "cache.t4g.micro"
	if in.CacheNodeType != nil {
		nodeType = common.Value(in.CacheNodeType)
	}

	port := spec.DefaultPort
	if in.Port != nil && *in.Port > 0 {
		port = *in.Port
	}

	subnetGroupName := "default"
	if in.CacheSubnetGroupName != nil && common.Value(in.CacheSubnetGroupName) != "" {
		subnetGroupName = common.Value(in.CacheSubnetGroupName)
	}

	// Verify subnet group exists
	var vpcID string
	if strings.EqualFold(subnetGroupName, "default") {
		defSG := s.getDefaultSubnetGroup(ctx, scope)
		if defSG != nil && defSG.VpcId != nil {
			vpcID = *defSG.VpcId
		}
	} else {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			sg, found, readErr := storage.Read[subnetGroupRecord](tx, subnetGroupKey(scope, subnetGroupName))
			if readErr != nil {
				return readErr
			}

			if !found {
				return subnetGroupNotFound(subnetGroupName)
			}

			vpcID = sg.VpcID
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Verify security groups
	if sgErr := s.verifySecurityGroups(ctx, in.SecurityGroupIds, vpcID); sgErr != nil {
		return nil, sgErr
	}

	// Parameter group
	paramGroupName := "default." + spec.Family
	if in.CacheParameterGroupName != nil && common.Value(in.CacheParameterGroupName) != "" {
		paramGroupName = common.Value(in.CacheParameterGroupName)
	}

	if !isDefaultParameterGroup(paramGroupName) {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			_, found, readErr := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, paramGroupName))
			if readErr != nil {
				return readErr
			}

			if !found {
				return parameterGroupNotFound(paramGroupName)
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	tags, tagErr := tagging.Map(in.Tags, func(t api.Tag) (k, v *string) {
		return t.Key, t.Value
	}, invalidParameterValue("Tag key and value are required"), nil)
	if tagErr != nil {
		return nil, tagErr
	}

	if err = tagging.CheckSet(tags); err != nil {
		return nil, invalidParameterValue("%s", err.Error())
	}

	prefAZ := scope.Region + "a"
	if in.PreferredAvailabilityZone != nil && common.Value(in.PreferredAvailabilityZone) != "" {
		prefAZ = common.Value(in.PreferredAvailabilityZone)
	}

	if spec.Engine == EngineMemcached && !isDefaultParameterGroup(paramGroupName) {
		var memParams map[string]string
		_ = s.store.View(ctx, func(tx storage.ReadTx) error {
			pg, found, rErr := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, paramGroupName))
			if rErr == nil && found {
				memParams = pg.UserValues
			}
			return nil
		})
		for k, v := range memParams {
			if _, fErr := memcachedParamFlags(k, v); fErr != nil {
				return nil, fErr
			}
		}
	}

	clusterStatus := statusAvailable
	nodeStatus := statusAvailable
	if s.runtime != nil {
		clusterStatus = statusCreating
		nodeStatus = statusCreating
	}

	now := time.Now().UTC()
	nodes := make([]clusterNode, 0, numNodes)
	for i := int64(1); i <= numNodes; i++ {
		nodeID := fmt.Sprintf("%04d", i)
		nodePort := port
		if spec.Engine == EngineMemcached {
			nodePort = port + i - 1
		}
		if s.runtime != nil {
			nodePort = 0
		}

		nodes = append(nodes, clusterNode{
			CreateTime: now,
			NodeID:     nodeID,
			Status:     nodeStatus,
			Port:       nodePort,
		})
	}

	record := clusterRecord{
		CreateTime:           now,
		Tags:                 tags,
		SecurityGroupIDs:     in.SecurityGroupIds,
		Nodes:                nodes,
		ClusterID:            clusterID,
		Engine:               spec.Engine,
		EngineVersion:        spec.EngineVersion,
		NodeType:             nodeType,
		Status:               clusterStatus,
		SubnetGroupName:      subnetGroupName,
		ParameterGroupName:   paramGroupName,
		ParameterApplyStatus: applyStatusInSync,
		PreferredAZ:          prefAZ,
		NumNodes:             numNodes,
		Port:                 port,
	}

	key := clusterKey(scope, clusterID)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found, readErr := storage.Read[clusterRecord](tx, key); readErr != nil {
			return readErr
		} else if found {
			return clusterAlreadyExists(clusterID)
		}

		if s.runtime != nil {
			if writeErr := storage.Write(tx, indexKey(scope, resKindCluster, clusterID), struct{}{}); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, key, record)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, clusterID, sourceTypeCacheCluster, "Cluster "+clusterID+" created")

	cluster := toAPICacheCluster(scope, &record, true)
	return &api.CreateCacheClusterResult{
		CacheCluster: &cluster,
	}, nil
}

func (s *Service) DescribeCacheClusters(
	ctx context.Context,
	in *api.DescribeCacheClustersMessage,
) (*api.CacheClusterMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	showNodes := in.ShowCacheNodeInfo != nil && *in.ShowCacheNodeInfo
	targetID := common.Value(in.CacheClusterId)

	if targetID != "" {
		targetID = strings.ToLower(targetID)
		var cluster api.CacheCluster
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			r, found, readErr := storage.Read[clusterRecord](tx, clusterKey(scope, targetID))
			if readErr != nil {
				return readErr
			}

			if !found {
				return clusterNotFound(targetID)
			}

			cluster = toAPICacheCluster(scope, r, showNodes)
			return nil
		})
		if err != nil {
			return nil, err
		}

		return &api.CacheClusterMessage{
			CacheClusters: []api.CacheCluster{cluster},
		}, nil
	}

	prefix := scope.Prefix(serviceName, "cluster")
	var clusters []api.CacheCluster

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[clusterRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, r := range records {
			clusters = append(clusters, toAPICacheCluster(scope, r, showNodes))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CacheClusterMessage{
		CacheClusters: clusters,
	}, nil
}

func (s *Service) ModifyCacheCluster(
	ctx context.Context,
	in *api.ModifyCacheClusterMessage,
) (*api.ModifyCacheClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	targetID := strings.ToLower(common.Value(in.CacheClusterId))
	if targetID == "" {
		return nil, invalidParameterValue("CacheClusterId is required.")
	}

	key := clusterKey(scope, targetID)
	var existing *clusterRecord

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[clusterRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return clusterNotFound(targetID)
		}

		existing = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Engine version modification: upgrade only
	if in.EngineVersion != nil && common.Value(in.EngineVersion) != "" {
		targetVer := common.Value(in.EngineVersion)
		newSpec, resErr := resolveEngine(existing.Engine, targetVer)
		if resErr != nil {
			return nil, resErr
		}

		if compareVersions(newSpec.EngineVersion, existing.EngineVersion) < 0 {
			return nil, invalidParameterValue(
				"Cannot downgrade engine version from %s to %s",
				existing.EngineVersion,
				newSpec.EngineVersion,
			)
		}

		existing.EngineVersion = newSpec.EngineVersion
	}

	// Memcached node count modification
	if in.NumCacheNodes != nil {
		newNum := *in.NumCacheNodes
		if existing.Engine != EngineMemcached {
			return nil, invalidParameterCombination("NumCacheNodes cannot be modified for %s", existing.Engine)
		}

		if newNum < 1 || newNum > 40 {
			return nil, invalidParameterValue("NumCacheNodes must be between 1 and 40 for Memcached")
		}

		if newNum > existing.NumNodes {
			now := time.Now().UTC()
			for i := existing.NumNodes + 1; i <= newNum; i++ {
				existing.Nodes = append(existing.Nodes, clusterNode{
					NodeID:     fmt.Sprintf("%04d", i),
					Port:       existing.Port + i - 1,
					Status:     statusAvailable,
					CreateTime: now,
				})
			}
		} else if newNum < existing.NumNodes {
			existing.Nodes = existing.Nodes[:newNum]
		}

		existing.NumNodes = newNum
	}

	// Security groups modification
	if len(in.SecurityGroupIds) > 0 {
		var vpcID string
		if strings.EqualFold(existing.SubnetGroupName, "default") {
			defSG := s.getDefaultSubnetGroup(ctx, scope)
			if defSG != nil && defSG.VpcId != nil {
				vpcID = *defSG.VpcId
			}
		} else {
			err = s.store.View(ctx, func(tx storage.ReadTx) error {
				sg, found, sgErr := storage.Read[subnetGroupRecord](tx, subnetGroupKey(scope, existing.SubnetGroupName))
				if sgErr != nil {
					return sgErr
				}

				if found {
					vpcID = sg.VpcID
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}

		if sgErr := s.verifySecurityGroups(ctx, in.SecurityGroupIds, vpcID); sgErr != nil {
			return nil, sgErr
		}

		existing.SecurityGroupIDs = in.SecurityGroupIds
	}

	// Parameter group modification
	if in.CacheParameterGroupName != nil && common.Value(in.CacheParameterGroupName) != "" {
		newPg := common.Value(in.CacheParameterGroupName)
		if !isDefaultParameterGroup(newPg) {
			err = s.store.View(ctx, func(tx storage.ReadTx) error {
				_, found, readErr := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, newPg))
				if readErr != nil {
					return readErr
				}

				if !found {
					return parameterGroupNotFound(newPg)
				}

				return nil
			})
			if err != nil {
				return nil, err
			}
		}

		existing.ParameterGroupName = newPg
		existing.ParameterApplyStatus = "in-sync"
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key, existing)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, targetID, sourceTypeCacheCluster, "Cluster "+targetID+" modified")

	cluster := toAPICacheCluster(scope, existing, true)
	return &api.ModifyCacheClusterResult{
		CacheCluster: &cluster,
	}, nil
}

func (s *Service) DeleteCacheCluster(
	ctx context.Context,
	in *api.DeleteCacheClusterMessage,
) (*api.DeleteCacheClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	targetID := strings.ToLower(common.Value(in.CacheClusterId))
	if targetID == "" {
		return nil, invalidParameterValue("CacheClusterId is required.")
	}

	key := clusterKey(scope, targetID)
	var existing *clusterRecord

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[clusterRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return clusterNotFound(targetID)
		}

		existing = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		existing.Status = statusDeleting
		for i := range existing.Nodes {
			existing.Nodes[i].Status = statusDeleting
		}
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			if writeErr := storage.Write(tx, indexKey(scope, resKindCluster, targetID), struct{}{}); writeErr != nil {
				return writeErr
			}
			return storage.Write(tx, key, *existing)
		})
		if err != nil {
			return nil, err
		}

		cluster := toAPICacheCluster(scope, existing, false)
		return &api.DeleteCacheClusterResult{
			CacheCluster: &cluster,
		}, nil
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, targetID, sourceTypeCacheCluster, "Cluster "+targetID+" deleted")

	existing.Status = "deleting"
	cluster := toAPICacheCluster(scope, existing, false)
	return &api.DeleteCacheClusterResult{
		CacheCluster: &cluster,
	}, nil
}

func (s *Service) RebootCacheCluster(
	ctx context.Context,
	in *api.RebootCacheClusterMessage,
) (*api.RebootCacheClusterResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	targetID := strings.ToLower(common.Value(in.CacheClusterId))
	if targetID == "" {
		return nil, invalidParameterValue("CacheClusterId is required.")
	}

	key := clusterKey(scope, targetID)
	var existing *clusterRecord

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[clusterRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return clusterNotFound(targetID)
		}

		existing = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		existing.Status = statusRebooting
		for i := range existing.Nodes {
			n := &existing.Nodes[i]
			if len(in.CacheNodeIdsToReboot) == 0 || slices.Contains(in.CacheNodeIdsToReboot, n.NodeID) {
				n.Status = statusRebooting
			}
		}
		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			if writeErr := storage.Write(tx, indexKey(scope, resKindCluster, targetID), struct{}{}); writeErr != nil {
				return writeErr
			}
			return storage.Write(tx, key, *existing)
		})
		if err != nil {
			return nil, err
		}
	}

	s.recordEvent(ctx, scope, targetID, sourceTypeCacheCluster, "Cluster "+targetID+" rebooted")

	cluster := toAPICacheCluster(scope, existing, true)
	return &api.RebootCacheClusterResult{
		CacheCluster: &cluster,
	}, nil
}

func compareVersions(v1, v2 string) int {
	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")
	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := range maxLen {
		n1 := 0
		if i < len(parts1) {
			n1, _ = strconv.Atoi(parts1[i])
		}

		n2 := 0
		if i < len(parts2) {
			n2, _ = strconv.Atoi(parts2[i])
		}

		if n1 < n2 {
			return -1
		}

		if n1 > n2 {
			return 1
		}
	}

	return 0
}

func clusterEndpoint(scope request.Scope, clusterID string) string {
	return fmt.Sprintf("%s.0001.%s.cache.amazonaws.com", clusterID, scope.Region)
}
