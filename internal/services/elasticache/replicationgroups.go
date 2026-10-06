package elasticache

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type replicationGroupRecord struct {
	CreateTime                time.Time
	AuthTokenLastModifiedDate *time.Time
	Tags                      map[string]string
	CacheNodeType             string
	CacheSubnetGroupName      string
	ReplicationGroupID        string
	Description               string
	Status                    string
	Engine                    string
	EngineVersion             string
	PrimaryClusterID          string
	CacheParameterGroupName   string
	AuthToken                 string
	PreferredAZ               string
	MemberClusterIDs          []string
	SecurityGroupIDs          []string
	Port                      int64
	AutomaticFailover         bool
	MultiAZ                   bool
	AuthTokenEnabled          bool
	TransitEncryptionEnabled  bool
	AtRestEncryptionEnabled   bool
}

func validateReplicationGroupID(id string) error {
	if len(id) < 1 || len(id) > 50 {
		return invalidParameterValue("ReplicationGroupId must be between 1 and 50 characters")
	}

	first := id[0]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') {
		return invalidParameterValue("ReplicationGroupId must begin with a letter")
	}

	for i := range id {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}

		if c == '-' {
			if i > 0 && id[i-1] == '-' {
				return invalidParameterValue("ReplicationGroupId cannot contain two consecutive hyphens")
			}
			if i == len(id)-1 {
				return invalidParameterValue("ReplicationGroupId cannot end with a hyphen")
			}
			continue
		}

		return invalidParameterValue("ReplicationGroupId must contain only letters, numbers, and hyphens")
	}

	for i := range id {
		if id[i] >= 'A' && id[i] <= 'Z' {
			return invalidParameterValue("ReplicationGroupId cannot contain uppercase letters")
		}
	}

	return nil
}

func validateAuthToken(token string) error {
	if len(token) < 16 || len(token) > 128 {
		return invalidParameterValue("AuthToken must be between 16 and 128 characters")
	}
	for _, r := range token {
		if r < 33 || r > 126 || r == '"' || r == '/' || r == '@' {
			return invalidParameterValue("AuthToken contains invalid characters")
		}
	}
	return nil
}

func (s *Service) CreateReplicationGroup(
	ctx context.Context,
	in *api.CreateReplicationGroupMessage,
) (*api.CreateReplicationGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	groupID := strings.ToLower(common.Value(in.ReplicationGroupId))
	if vErr := validateReplicationGroupID(groupID); vErr != nil {
		return nil, vErr
	}

	if in.GlobalReplicationGroupId != nil && *in.GlobalReplicationGroupId != "" {
		return nil, awserror.New("InvalidParameterCombination", "Global datastores are not supported")
	}
	if in.DataTieringEnabled != nil && *in.DataTieringEnabled {
		return nil, awserror.New("InvalidParameterCombination", "Data tiering is not supported")
	}
	if in.NumNodeGroups != nil && *in.NumNodeGroups > 1 {
		return nil, awserror.New("InvalidParameterCombination", "Multi-node groups / cluster mode is not supported")
	}
	if in.ClusterMode != nil && strings.EqualFold(*in.ClusterMode, "enabled") {
		return nil, awserror.New("InvalidParameterCombination", "Cluster mode enabled is not supported")
	}

	engine := strings.ToLower(common.Value(in.Engine))
	if engine == "" {
		engine = EngineRedis
	}
	if engine == EngineMemcached {
		return nil, awserror.New("InvalidParameterCombination", "Memcached cannot join a replication group")
	}
	if engine != EngineRedis && engine != EngineValkey {
		return nil, invalidParameterValue("Invalid engine: %s", engine)
	}

	transitEnc := in.TransitEncryptionEnabled != nil && *in.TransitEncryptionEnabled
	authToken := common.Value(in.AuthToken)
	if authToken != "" {
		if !transitEnc {
			return nil, awserror.New(
				"InvalidParameterCombination",
				"TransitEncryptionEnabled must be true when AuthToken is specified",
			)
		}
		if vErr := validateAuthToken(authToken); vErr != nil {
			return nil, vErr
		}
	}

	autoFailover := in.AutomaticFailoverEnabled != nil && *in.AutomaticFailoverEnabled

	var totalClusters int64 = 1
	switch {
	case in.NumCacheClusters != nil && *in.NumCacheClusters > 0:
		totalClusters = *in.NumCacheClusters
	case in.ReplicasPerNodeGroup != nil && *in.ReplicasPerNodeGroup >= 0:
		totalClusters = *in.ReplicasPerNodeGroup + 1
	case autoFailover:
		totalClusters = 2
	}

	if totalClusters < 1 || totalClusters > 6 {
		return nil, invalidParameterValue("Total cache clusters must be between 1 and 6")
	}
	if autoFailover && totalClusters < 2 {
		return nil, invalidParameterValue("Automatic failover requires at least 2 cache clusters")
	}

	spec, err := resolveEngine(engine, common.Value(in.EngineVersion))
	if err != nil {
		return nil, err
	}

	nodeType := common.Value(in.CacheNodeType)
	if nodeType == "" {
		nodeType = "cache.t4g.micro"
	}

	port := int64(6379)
	if in.Port != nil && *in.Port > 0 {
		port = *in.Port
	}

	subnetGroupName := strings.ToLower(common.Value(in.CacheSubnetGroupName))
	var subnetGroup *subnetGroupRecord
	if subnetGroupName != "" {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			sg, found, sErr := storage.Read[subnetGroupRecord](tx, subnetGroupKey(scope, subnetGroupName))
			if sErr != nil {
				return sErr
			}
			if !found {
				return subnetGroupNotFound(subnetGroupName)
			}
			subnetGroup = sg
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	vpcID := defaultVpcID
	if subnetGroup != nil {
		vpcID = subnetGroup.VpcID
	}

	sgIDs := in.SecurityGroupIds
	if len(sgIDs) > 0 {
		if vErr := s.verifySecurityGroups(ctx, sgIDs, vpcID); vErr != nil {
			return nil, vErr
		}
	}

	paramGroupName := "default." + spec.Family
	if in.CacheParameterGroupName != nil && common.Value(in.CacheParameterGroupName) != "" {
		paramGroupName = strings.ToLower(common.Value(in.CacheParameterGroupName))
	}
	if !isDefaultParameterGroup(paramGroupName) {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			_, found, pErr := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, paramGroupName))
			if pErr != nil {
				return pErr
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

	key := replicationGroupKey(scope, groupID)
	now := s.now().UTC()

	var repRecord *replicationGroupRecord
	var memberClusterIDs []string
	primaryClusterID := ""

	if in.PrimaryClusterId != nil && *in.PrimaryClusterId != "" {
		primaryClusterID = strings.ToLower(*in.PrimaryClusterId)
	}

	status := statusAvailable
	if s.runtime != nil {
		status = statusCreating
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, rErr := storage.Read[replicationGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}
		if found {
			return replicationGroupAlreadyExists(groupID)
		}

		startIndex := int64(1)
		if primaryClusterID != "" {
			primaryRecord, pFound, pErr := storage.Read[clusterRecord](tx, clusterKey(scope, primaryClusterID))
			if pErr != nil {
				return pErr
			}
			if !pFound {
				return clusterNotFound(primaryClusterID)
			}
			if primaryRecord.ReplicationGroupID != "" {
				return awserror.New(
					"InvalidClusterStateFault",
					fmt.Sprintf(
						"Cluster %s already belongs to replication group %s",
						primaryClusterID,
						primaryRecord.ReplicationGroupID,
					),
				)
			}
			primaryRecord.ReplicationGroupID = groupID
			if authToken != "" {
				primaryRecord.AuthToken = authToken
				primaryRecord.TransitEncryption = transitEnc
			}
			if writeErr := storage.Write(tx, clusterKey(scope, primaryClusterID), *primaryRecord); writeErr != nil {
				return writeErr
			}
			memberClusterIDs = append(memberClusterIDs, primaryClusterID)
			startIndex = 2
		}

		for i := startIndex; i <= totalClusters; i++ {
			memberID := fmt.Sprintf("%s-%03d", groupID, i)
			memberClusterIDs = append(memberClusterIDs, memberID)
			if primaryClusterID == "" && i == 1 {
				primaryClusterID = memberID
			}

			nodeStatus := statusAvailable
			nodePort := port
			if s.runtime != nil {
				nodeStatus = statusCreating
				nodePort = 0
			}

			clRecord := clusterRecord{
				CreateTime:       now,
				Tags:             tags,
				SecurityGroupIDs: sgIDs,
				Nodes: []clusterNode{
					{
						CreateTime: now,
						NodeID:     "0001",
						Status:     nodeStatus,
						Port:       nodePort,
					},
				},
				ClusterID:            memberID,
				Engine:               spec.Engine,
				EngineVersion:        spec.EngineVersion,
				NodeType:             nodeType,
				Status:               nodeStatus,
				SubnetGroupName:      subnetGroupName,
				ParameterGroupName:   paramGroupName,
				ParameterApplyStatus: applyStatusInSync,
				ReplicationGroupID:   groupID,
				AuthToken:            authToken,
				NumNodes:             1,
				Port:                 port,
				TransitEncryption:    transitEnc,
			}

			if writeErr := storage.Write(tx, clusterKey(scope, memberID), clRecord); writeErr != nil {
				return writeErr
			}
			if s.runtime != nil {
				if writeErr := storage.Write(
					tx,
					indexClusterKey(scope, memberID),
					struct{}{},
				); writeErr != nil {
					return writeErr
				}
			}
		}

		var lastModified *time.Time
		if authToken != "" {
			lastModified = &now
		}

		repRecord = &replicationGroupRecord{
			CreateTime:                now,
			AuthTokenLastModifiedDate: lastModified,
			Tags:                      tags,
			SecurityGroupIDs:          sgIDs,
			MemberClusterIDs:          memberClusterIDs,
			ReplicationGroupID:        groupID,
			Description:               common.Value(in.ReplicationGroupDescription),
			Status:                    status,
			Engine:                    spec.Engine,
			EngineVersion:             spec.EngineVersion,
			CacheNodeType:             nodeType,
			CacheParameterGroupName:   paramGroupName,
			CacheSubnetGroupName:      subnetGroupName,
			PreferredAZ:               "",
			AuthToken:                 authToken,
			PrimaryClusterID:          primaryClusterID,
			Port:                      port,
			AutomaticFailover:         autoFailover,
			MultiAZ:                   in.MultiAZEnabled != nil && *in.MultiAZEnabled,
			AuthTokenEnabled:          authToken != "",
			TransitEncryptionEnabled:  transitEnc,
			AtRestEncryptionEnabled:   in.AtRestEncryptionEnabled != nil && *in.AtRestEncryptionEnabled,
		}

		return storage.Write(tx, key, *repRecord)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, groupID, "replication-group", fmt.Sprintf("Replication group %s created", groupID))

	var apiGroup api.ReplicationGroup
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		apiGroup = s.toAPIReplicationGroup(scope, repRecord, tx)
		return nil
	})

	return &api.CreateReplicationGroupResult{
		ReplicationGroup: &apiGroup,
	}, nil
}

func (s *Service) DescribeReplicationGroups(
	ctx context.Context,
	in *api.DescribeReplicationGroupsMessage,
) (*api.ReplicationGroupMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	targetID := strings.ToLower(common.Value(in.ReplicationGroupId))
	var groups []api.ReplicationGroup

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if targetID != "" {
			key := replicationGroupKey(scope, targetID)
			rec, found, rErr := storage.Read[replicationGroupRecord](tx, key)
			if rErr != nil {
				return rErr
			}
			if !found {
				return replicationGroupNotFound(targetID)
			}
			groups = append(groups, s.toAPIReplicationGroup(scope, rec, tx))
			return nil
		}

		records, rErr := storage.Records[replicationGroupRecord](tx, replicationGroupPrefix(scope))
		if rErr != nil {
			return rErr
		}
		for _, rec := range records {
			groups = append(groups, s.toAPIReplicationGroup(scope, rec, tx))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ReplicationGroupMessage{
		ReplicationGroups: groups,
	}, nil
}

func (s *Service) ModifyReplicationGroup(
	ctx context.Context,
	in *api.ModifyReplicationGroupMessage,
) (*api.ModifyReplicationGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	groupID := strings.ToLower(common.Value(in.ReplicationGroupId))
	if groupID == "" {
		return nil, invalidParameterValue("ReplicationGroupId is required")
	}

	key := replicationGroupKey(scope, groupID)
	var repRecord *replicationGroupRecord
	var failoverOccurred bool
	var oldPrimary, promotedPrimary string

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, rErr := storage.Read[replicationGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}
		if !found {
			return replicationGroupNotFound(groupID)
		}

		if in.ClusterMode != nil && strings.EqualFold(*in.ClusterMode, "enabled") {
			return awserror.New("InvalidParameterCombination", "Cluster mode enabled is not supported")
		}

		if in.ReplicationGroupDescription != nil {
			existing.Description = *in.ReplicationGroupDescription
		}

		if in.AutomaticFailoverEnabled != nil {
			if *in.AutomaticFailoverEnabled && len(existing.MemberClusterIDs) < 2 {
				return invalidParameterValue("Automatic failover requires at least 2 cache clusters")
			}
			existing.AutomaticFailover = *in.AutomaticFailoverEnabled
		}

		if in.MultiAZEnabled != nil {
			existing.MultiAZ = *in.MultiAZEnabled
		}

		if len(in.SecurityGroupIds) > 0 {
			existing.SecurityGroupIDs = in.SecurityGroupIds
			for _, mID := range existing.MemberClusterIDs {
				if cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID)); clFound {
					cl.SecurityGroupIDs = in.SecurityGroupIds
					_ = storage.Write(tx, clusterKey(scope, mID), *cl)
				}
			}
		}

		if in.CacheParameterGroupName != nil && *in.CacheParameterGroupName != "" {
			paramGroup := strings.ToLower(*in.CacheParameterGroupName)
			existing.CacheParameterGroupName = paramGroup
			for _, mID := range existing.MemberClusterIDs {
				if cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID)); clFound {
					cl.ParameterGroupName = paramGroup
					_ = storage.Write(tx, clusterKey(scope, mID), *cl)
				}
			}
		}

		if in.AuthTokenUpdateStrategy != nil {
			strategy := strings.ToUpper(*in.AuthTokenUpdateStrategy)
			now := s.now().UTC()
			switch strategy {
			case "SET", "ROTATE":
				token := common.Value(in.AuthToken)
				if token == "" {
					return invalidParameterValue("AuthToken is required for SET/ROTATE strategy")
				}
				if vErr := validateAuthToken(token); vErr != nil {
					return vErr
				}
				if s.runtime != nil {
					for _, mID := range existing.MemberClusterIDs {
						if cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID)); clFound {
							for _, n := range cl.Nodes {
								if n.Port > 0 {
									_ = redisConfigSet(ctx, n.Port, existing.AuthToken, "requirepass", token)
								}
							}
						}
					}
				}
				existing.AuthToken = token
				existing.AuthTokenEnabled = true
				existing.AuthTokenLastModifiedDate = &now
			case "DELETE":
				if s.runtime != nil {
					for _, mID := range existing.MemberClusterIDs {
						if cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID)); clFound {
							for _, n := range cl.Nodes {
								if n.Port > 0 {
									_ = redisConfigSet(ctx, n.Port, existing.AuthToken, "requirepass", "")
								}
							}
						}
					}
				}
				existing.AuthToken = ""
				existing.AuthTokenEnabled = false
				existing.AuthTokenLastModifiedDate = &now
			}
		}

		// Manual failover to PrimaryClusterId
		if in.PrimaryClusterId != nil && *in.PrimaryClusterId != "" {
			newPrimary := strings.ToLower(*in.PrimaryClusterId)
			if !slices.Contains(existing.MemberClusterIDs, newPrimary) {
				return invalidParameterValue("Cluster %s is not a member of replication group %s", newPrimary, groupID)
			}
			if newPrimary != existing.PrimaryClusterID {
				oldPrimary = existing.PrimaryClusterID
				if s.runtime != nil {
					s.performFailover(ctx, scope, existing, newPrimary, tx)
				}
				existing.PrimaryClusterID = newPrimary
				failoverOccurred = true
				promotedPrimary = newPrimary
			}
		}

		repRecord = existing
		return storage.Write(tx, key, *existing)
	})
	if err != nil {
		return nil, err
	}

	if failoverOccurred {
		s.recordEvent(
			ctx,
			scope,
			groupID,
			"replication-group",
			fmt.Sprintf("Failover from primary node %s to replica node %s completed", oldPrimary, promotedPrimary),
		)
	}

	var apiGroup api.ReplicationGroup
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		apiGroup = s.toAPIReplicationGroup(scope, repRecord, tx)
		return nil
	})

	return &api.ModifyReplicationGroupResult{
		ReplicationGroup: &apiGroup,
	}, nil
}

func (s *Service) DeleteReplicationGroup(
	ctx context.Context,
	in *api.DeleteReplicationGroupMessage,
) (*api.DeleteReplicationGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	groupID := strings.ToLower(common.Value(in.ReplicationGroupId))
	if groupID == "" {
		return nil, invalidParameterValue("ReplicationGroupId is required")
	}

	key := replicationGroupKey(scope, groupID)
	var repRecord *replicationGroupRecord
	retainPrimary := in.RetainPrimaryCluster != nil && *in.RetainPrimaryCluster

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, rErr := storage.Read[replicationGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}
		if !found {
			return replicationGroupNotFound(groupID)
		}

		repRecord = existing

		for _, mID := range existing.MemberClusterIDs {
			if retainPrimary && mID == existing.PrimaryClusterID {
				if cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID)); clFound {
					cl.ReplicationGroupID = ""
					_ = storage.Write(tx, clusterKey(scope, mID), *cl)
				}
				continue
			}

			if s.runtime != nil {
				if cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID)); clFound {
					for _, n := range cl.Nodes {
						if n.ContainerID != "" {
							s.runtime.Remove(ctx, n.ContainerID)
						}
					}
				}
				tx.Delete(indexClusterKey(scope, mID))
			}

			tx.Delete(clusterKey(scope, mID))
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, groupID, "replication-group", fmt.Sprintf("Replication group %s deleted", groupID))

	repRecord.Status = statusDeleting
	apiGroup := s.toAPIReplicationGroup(scope, repRecord, nil)
	return &api.DeleteReplicationGroupResult{
		ReplicationGroup: &apiGroup,
	}, nil
}

func (s *Service) IncreaseReplicaCount(
	ctx context.Context,
	in *api.IncreaseReplicaCountMessage,
) (*api.IncreaseReplicaCountResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	groupID := strings.ToLower(common.Value(in.ReplicationGroupId))
	if groupID == "" {
		return nil, invalidParameterValue("ReplicationGroupId is required")
	}

	key := replicationGroupKey(scope, groupID)
	var repRecord *replicationGroupRecord

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, rErr := storage.Read[replicationGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}
		if !found {
			return replicationGroupNotFound(groupID)
		}

		currentReplicas := int64(len(existing.MemberClusterIDs) - 1)
		targetReplicas := int64(0)
		if in.NewReplicaCount != nil {
			targetReplicas = *in.NewReplicaCount
		}

		if targetReplicas <= currentReplicas {
			return invalidParameterValue(
				"NewReplicaCount (%d) must be greater than current replica count (%d)",
				targetReplicas,
				currentReplicas,
			)
		}

		totalNodes := targetReplicas + 1
		if totalNodes > 6 {
			return invalidParameterValue("Cannot exceed 6 total nodes in a replication group")
		}

		now := s.now().UTC()
		toAdd := targetReplicas - currentReplicas

		for range toAdd {
			// Find next available member index
			nextIdx := int64(len(existing.MemberClusterIDs) + 1)
			memberID := fmt.Sprintf("%s-%03d", groupID, nextIdx)
			for slices.Contains(existing.MemberClusterIDs, memberID) {
				nextIdx++
				memberID = fmt.Sprintf("%s-%03d", groupID, nextIdx)
			}

			nodeStatus := statusAvailable
			nodePort := existing.Port
			if s.runtime != nil {
				nodeStatus = statusCreating
				nodePort = 0
			}

			clRecord := clusterRecord{
				CreateTime:       now,
				Tags:             existing.Tags,
				SecurityGroupIDs: existing.SecurityGroupIDs,
				Nodes: []clusterNode{
					{
						CreateTime: now,
						NodeID:     "0001",
						Status:     nodeStatus,
						Port:       nodePort,
					},
				},
				ClusterID:            memberID,
				Engine:               existing.Engine,
				EngineVersion:        existing.EngineVersion,
				NodeType:             existing.CacheNodeType,
				Status:               nodeStatus,
				SubnetGroupName:      existing.CacheSubnetGroupName,
				ParameterGroupName:   existing.CacheParameterGroupName,
				ParameterApplyStatus: applyStatusInSync,
				ReplicationGroupID:   groupID,
				AuthToken:            existing.AuthToken,
				NumNodes:             1,
				Port:                 existing.Port,
				TransitEncryption:    existing.TransitEncryptionEnabled,
			}

			if writeErr := storage.Write(tx, clusterKey(scope, memberID), clRecord); writeErr != nil {
				return writeErr
			}
			if s.runtime != nil {
				if writeErr := storage.Write(
					tx,
					indexClusterKey(scope, memberID),
					struct{}{},
				); writeErr != nil {
					return writeErr
				}
			}
			existing.MemberClusterIDs = append(existing.MemberClusterIDs, memberID)
		}

		repRecord = existing
		return storage.Write(tx, key, *existing)
	})
	if err != nil {
		return nil, err
	}

	var apiGroup api.ReplicationGroup
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		apiGroup = s.toAPIReplicationGroup(scope, repRecord, tx)
		return nil
	})

	return &api.IncreaseReplicaCountResult{
		ReplicationGroup: &apiGroup,
	}, nil
}

func (s *Service) DecreaseReplicaCount(
	ctx context.Context,
	in *api.DecreaseReplicaCountMessage,
) (*api.DecreaseReplicaCountResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	groupID := strings.ToLower(common.Value(in.ReplicationGroupId))
	if groupID == "" {
		return nil, invalidParameterValue("ReplicationGroupId is required")
	}

	key := replicationGroupKey(scope, groupID)
	var repRecord *replicationGroupRecord

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, rErr := storage.Read[replicationGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}
		if !found {
			return replicationGroupNotFound(groupID)
		}

		currentReplicas := int64(len(existing.MemberClusterIDs) - 1)
		var toRemove []string

		switch {
		case len(in.ReplicasToRemove) > 0:
			for _, r := range in.ReplicasToRemove {
				remID := strings.ToLower(r)
				if remID == existing.PrimaryClusterID {
					return invalidParameterValue("Cannot remove the primary cluster from replication group")
				}
				if !slices.Contains(existing.MemberClusterIDs, remID) {
					return invalidParameterValue("Cluster %s is not a member of replication group", remID)
				}
				toRemove = append(toRemove, remID)
			}
		case in.NewReplicaCount != nil:
			targetReplicas := *in.NewReplicaCount
			if targetReplicas >= currentReplicas {
				return invalidParameterValue("NewReplicaCount must be less than current replica count")
			}
			if targetReplicas < 0 {
				return invalidParameterValue("NewReplicaCount cannot be negative")
			}
			numToRemove := currentReplicas - targetReplicas
			// Pick from end of member clusters (excluding primary)
			for i := len(existing.MemberClusterIDs) - 1; i >= 0 && len(toRemove) < int(numToRemove); i-- {
				cand := existing.MemberClusterIDs[i]
				if cand != existing.PrimaryClusterID {
					toRemove = append(toRemove, cand)
				}
			}
		default:
			return invalidParameterValue("Either NewReplicaCount or ReplicasToRemove must be specified")
		}

		remainingCount := len(existing.MemberClusterIDs) - len(toRemove)
		if existing.AutomaticFailover && remainingCount < 2 {
			return awserror.New(
				"InvalidParameterCombination",
				"Cannot reduce replica count below 1 when automatic failover is enabled",
			)
		}

		for _, remID := range toRemove {
			if s.runtime != nil {
				if cl, clFound, _ := storage.Read[clusterRecord](tx, clusterKey(scope, remID)); clFound {
					for _, n := range cl.Nodes {
						if n.ContainerID != "" {
							s.runtime.Remove(ctx, n.ContainerID)
						}
					}
				}
				tx.Delete(indexClusterKey(scope, remID))
			}
			tx.Delete(clusterKey(scope, remID))

			// Remove from slice
			existing.MemberClusterIDs = slices.DeleteFunc(existing.MemberClusterIDs, func(id string) bool {
				return id == remID
			})
		}

		repRecord = existing
		return storage.Write(tx, key, *existing)
	})
	if err != nil {
		return nil, err
	}

	var apiGroup api.ReplicationGroup
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		apiGroup = s.toAPIReplicationGroup(scope, repRecord, tx)
		return nil
	})

	return &api.DecreaseReplicaCountResult{
		ReplicationGroup: &apiGroup,
	}, nil
}

func (s *Service) TestFailover(
	ctx context.Context,
	in *api.TestFailoverMessage,
) (*api.TestFailoverResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	groupID := strings.ToLower(common.Value(in.ReplicationGroupId))
	if groupID == "" {
		return nil, invalidParameterValue("ReplicationGroupId is required")
	}

	key := replicationGroupKey(scope, groupID)
	var repRecord *replicationGroupRecord
	var oldPrimary, promotedPrimary string

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, rErr := storage.Read[replicationGroupRecord](tx, key)
		if rErr != nil {
			return rErr
		}
		if !found {
			return replicationGroupNotFound(groupID)
		}

		if len(existing.MemberClusterIDs) < 2 {
			return awserror.New("InvalidParameterCombination", "Replication group has no replicas to failover to")
		}

		// Find first replica to promote
		var newPrimary string
		for _, mID := range existing.MemberClusterIDs {
			if mID != existing.PrimaryClusterID {
				newPrimary = mID
				break
			}
		}

		oldPrimary = existing.PrimaryClusterID
		promotedPrimary = newPrimary
		if s.runtime != nil {
			s.performFailover(ctx, scope, existing, newPrimary, tx)
		}

		existing.PrimaryClusterID = newPrimary
		repRecord = existing

		return storage.Write(tx, key, *existing)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(
		ctx,
		scope,
		groupID,
		"replication-group",
		fmt.Sprintf("Failover from primary node %s to replica node %s completed", oldPrimary, promotedPrimary),
	)

	var apiGroup api.ReplicationGroup
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		apiGroup = s.toAPIReplicationGroup(scope, repRecord, tx)
		return nil
	})

	return &api.TestFailoverResult{
		ReplicationGroup: &apiGroup,
	}, nil
}

func (s *Service) performFailover(
	ctx context.Context,
	scope request.Scope,
	repGroup *replicationGroupRecord,
	newPrimary string,
	tx storage.ReadTx,
) {
	// Look up new primary cluster
	var newPrimaryPort int64
	if cl, found, _ := storage.Read[clusterRecord](tx, clusterKey(scope, newPrimary)); found && len(cl.Nodes) > 0 {
		newPrimaryPort = cl.Nodes[0].Port
	}

	if newPrimaryPort > 0 {
		_ = redisReplicaOf(ctx, "127.0.0.1", newPrimaryPort, repGroup.AuthToken, "NO", 0, 2*time.Second)
	}

	// Tell old primary and other replicas to follow new primary
	for _, mID := range repGroup.MemberClusterIDs {
		if mID == newPrimary {
			continue
		}
		if cl, found, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID)); found && len(cl.Nodes) > 0 {
			port := cl.Nodes[0].Port
			if port > 0 {
				_ = redisReplicaOf(ctx, "127.0.0.1", port, repGroup.AuthToken, newPrimary, 6379, 2*time.Second)
			}
		}
	}
}

func (s *Service) toAPIReplicationGroup(
	scope request.Scope,
	r *replicationGroupRecord,
	tx storage.ReadTx,
) api.ReplicationGroup {
	autoFailover := "disabled"
	if r.AutomaticFailover {
		autoFailover = "enabled"
	}
	multiAZ := "disabled"
	if r.MultiAZ {
		multiAZ = "enabled"
	}

	groupStatus := r.Status
	arn := replicationGroupARN(scope, r.ReplicationGroupID)

	var nodeMembers []api.NodeGroupMember
	var primaryEndpoint *api.Endpoint
	var readerEndpoint *api.Endpoint

	if tx != nil {
		for _, mID := range r.MemberClusterIDs {
			cl, found, _ := storage.Read[clusterRecord](tx, clusterKey(scope, mID))
			if !found {
				continue
			}

			if cl.Status == statusCreating {
				groupStatus = statusCreating
			}

			role := "replica"
			if mID == r.PrimaryClusterID {
				role = "primary"
			}

			nodePort := cl.Port
			if len(cl.Nodes) > 0 && cl.Nodes[0].Port > 0 {
				nodePort = cl.Nodes[0].Port
			}

			host := localhost
			endpoint := &api.Endpoint{
				Address: new(host),
				Port:    new(nodePort),
			}

			if role == "primary" {
				primaryEndpoint = endpoint
			} else if readerEndpoint == nil {
				readerEndpoint = endpoint
			}

			nodeMembers = append(nodeMembers, api.NodeGroupMember{
				CacheClusterId:            new(cl.ClusterID),
				CacheNodeId:               new("0001"),
				CurrentRole:               new(role),
				PreferredAvailabilityZone: new(cl.PreferredAZ),
				ReadEndpoint:              endpoint,
			})
		}
	}

	if primaryEndpoint == nil {
		host := localhost
		port := r.Port
		primaryEndpoint = &api.Endpoint{
			Address: new(host),
			Port:    new(port),
		}
	}
	if readerEndpoint == nil {
		readerEndpoint = primaryEndpoint
	}

	nodeGroup := api.NodeGroup{
		NodeGroupId:      new("0001"),
		Status:           new(groupStatus),
		PrimaryEndpoint:  primaryEndpoint,
		ReaderEndpoint:   readerEndpoint,
		NodeGroupMembers: nodeMembers,
	}

	return api.ReplicationGroup{
		ARN:                        new(arn),
		AutomaticFailover:          new(autoFailover),
		CacheNodeType:              new(r.CacheNodeType),
		ClusterEnabled:             new(false),
		Description:                new(r.Description),
		Engine:                     new(r.Engine),
		MemberClusters:             r.MemberClusterIDs,
		MultiAZ:                    new(multiAZ),
		NodeGroups:                 []api.NodeGroup{nodeGroup},
		ReplicationGroupCreateTime: new(r.CreateTime),
		ReplicationGroupId:         new(r.ReplicationGroupID),
		Status:                     new(groupStatus),
		AuthTokenEnabled:           new(r.AuthTokenEnabled),
		AuthTokenLastModifiedDate:  r.AuthTokenLastModifiedDate,
		TransitEncryptionEnabled:   new(r.TransitEncryptionEnabled),
		AtRestEncryptionEnabled:    new(r.AtRestEncryptionEnabled),
	}
}
