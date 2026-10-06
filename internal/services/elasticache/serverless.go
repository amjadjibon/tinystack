package elasticache

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type serverlessCacheRecord struct {
	CreateTime             time.Time
	SnapshotRetentionLimit *int64
	CacheUsageLimits       *api.CacheUsageLimits
	Tags                   map[string]string
	Description            string
	Engine                 string
	FullEngineVersion      string
	KmsKeyID               string
	MajorEngineVersion     string
	ServerlessCacheName    string
	ARN                    string
	Status                 string
	DailySnapshotTime      string
	Node                   clusterNode
	SecurityGroupIDs       []string
	SubnetIDs              []string
}

func validateServerlessCacheName(name string) error {
	if len(name) < 1 || len(name) > 40 {
		return invalidParameterValue("ServerlessCacheName must be between 1 and 40 characters")
	}

	first := name[0]
	if (first < 'a' || first > 'z') && (first < 'A' || first > 'Z') {
		return invalidParameterValue("ServerlessCacheName must start with a letter")
	}

	for i := range name {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}

		if c == '-' {
			if i+1 < len(name) && name[i+1] == '-' {
				return invalidParameterValue("ServerlessCacheName cannot contain consecutive hyphens")
			}

			continue
		}

		return invalidParameterValue("ServerlessCacheName contains invalid character: %c", c)
	}

	if name[len(name)-1] == '-' {
		return invalidParameterValue("ServerlessCacheName cannot end with a hyphen")
	}

	return nil
}

func (s *Service) toAPIServerlessCache(r *serverlessCacheRecord) api.ServerlessCache {
	host := localhost
	port := int64(6379)
	if spec, err := resolveEngine(r.Engine, r.MajorEngineVersion); err == nil {
		port = spec.DefaultPort
	}

	if r.Node.Port > 0 {
		port = r.Node.Port
	}

	endpoint := &api.Endpoint{
		Address: new(host),
		Port:    new(port),
	}
	readerEndpoint := &api.Endpoint{
		Address: new(host),
		Port:    new(port),
	}

	var createTime *time.Time
	if !r.CreateTime.IsZero() {
		createTime = &r.CreateTime
	}

	var snapLimit *int64
	if r.SnapshotRetentionLimit != nil {
		limit := *r.SnapshotRetentionLimit
		snapLimit = &limit
	}

	var kmsKeyID *string
	if r.KmsKeyID != "" {
		kmsKeyID = new(r.KmsKeyID)
	}

	var dailySnapshotTime *string
	if r.DailySnapshotTime != "" {
		dailySnapshotTime = new(r.DailySnapshotTime)
	}

	var description *string
	if r.Description != "" {
		description = new(r.Description)
	}

	return api.ServerlessCache{
		ARN:                    new(r.ARN),
		CacheUsageLimits:       r.CacheUsageLimits,
		CreateTime:             createTime,
		DailySnapshotTime:      dailySnapshotTime,
		Description:            description,
		Endpoint:               endpoint,
		Engine:                 new(r.Engine),
		FullEngineVersion:      new(r.FullEngineVersion),
		KmsKeyId:               kmsKeyID,
		MajorEngineVersion:     new(r.MajorEngineVersion),
		ReaderEndpoint:         readerEndpoint,
		SecurityGroupIds:       r.SecurityGroupIDs,
		ServerlessCacheName:    new(r.ServerlessCacheName),
		SnapshotRetentionLimit: snapLimit,
		Status:                 new(r.Status),
		SubnetIds:              r.SubnetIDs,
	}
}

func (s *Service) CreateServerlessCache(
	ctx context.Context,
	in *api.CreateServerlessCacheRequest,
) (*api.CreateServerlessCacheResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(common.Value(in.ServerlessCacheName))
	if name == "" {
		return nil, invalidParameterValue("ServerlessCacheName is required")
	}

	if vErr := validateServerlessCacheName(name); vErr != nil {
		return nil, vErr
	}

	engine := strings.ToLower(strings.TrimSpace(common.Value(in.Engine)))
	if engine == "" {
		return nil, invalidParameterValue("Engine is required")
	}

	spec, err := resolveEngine(engine, common.Value(in.MajorEngineVersion))
	if err != nil {
		return nil, err
	}

	majorVer := common.Value(in.MajorEngineVersion)
	if majorVer == "" {
		majorVer = strings.Split(spec.EngineVersion, ".")[0]
	}

	tagMap, tagErr := tagging.Map(in.Tags, func(t api.Tag) (k, v *string) {
		return t.Key, t.Value
	}, invalidParameterValue("Tag key and value are required"), nil)
	if tagErr != nil {
		return nil, tagErr
	}

	if err = tagging.CheckSet(tagMap); err != nil {
		return nil, invalidParameterValue("%s", err.Error())
	}

	vpcID := defaultVpcID
	if len(in.SubnetIds) > 0 {
		var sErr error
		vpcID, _, sErr = s.validateSubnets(ctx, scope, in.SubnetIds)
		if sErr != nil {
			return nil, sErr
		}
	}

	if len(in.SecurityGroupIds) > 0 {
		if vErr := s.verifySecurityGroups(ctx, in.SecurityGroupIds, vpcID); vErr != nil {
			return nil, vErr
		}
	}

	key := serverlessCacheKey(scope, name)
	now := s.now()
	status := statusAvailable
	if s.runtime != nil {
		status = statusCreating
	}

	arn := serverlessCacheARN(scope, name)
	rec := serverlessCacheRecord{
		ARN:                    arn,
		CacheUsageLimits:       in.CacheUsageLimits,
		CreateTime:             now,
		DailySnapshotTime:      common.Value(in.DailySnapshotTime),
		Description:            common.Value(in.Description),
		Engine:                 spec.Engine,
		FullEngineVersion:      spec.EngineVersion,
		KmsKeyID:               common.Value(in.KmsKeyId),
		MajorEngineVersion:     majorVer,
		SecurityGroupIDs:       in.SecurityGroupIds,
		ServerlessCacheName:    name,
		SnapshotRetentionLimit: in.SnapshotRetentionLimit,
		Status:                 status,
		SubnetIDs:              in.SubnetIds,
		Tags:                   tagMap,
		Node: clusterNode{
			NodeID:     name + "-001",
			Status:     status,
			Port:       spec.DefaultPort,
			CreateTime: now,
		},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, rErr := storage.Read[serverlessCacheRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if found {
			return serverlessCacheAlreadyExists(name)
		}

		if s.runtime != nil {
			if wErr := storage.Write(tx, indexServerlessKey(scope, name), struct{}{}); wErr != nil {
				return wErr
			}
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, "serverless-cache", "Serverless cache created")
	apiRes := s.toAPIServerlessCache(&rec)

	return &api.CreateServerlessCacheResponse{
		ServerlessCache: &apiRes,
	}, nil
}

func (s *Service) DescribeServerlessCaches(
	ctx context.Context,
	in *api.DescribeServerlessCachesRequest,
) (*api.DescribeServerlessCachesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var caches []api.ServerlessCache
	name := strings.TrimSpace(common.Value(in.ServerlessCacheName))

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if name != "" {
			key := serverlessCacheKey(scope, name)
			rec, found, rErr := storage.Read[serverlessCacheRecord](tx, key)
			if rErr != nil {
				return rErr
			}

			if !found {
				return serverlessCacheNotFound(name)
			}

			caches = append(caches, s.toAPIServerlessCache(rec))
			return nil
		}

		for _, k := range tx.Keys(serverlessCachePrefix(scope)) {
			rec, found, rErr := storage.Read[serverlessCacheRecord](tx, k)
			if rErr != nil {
				return rErr
			}

			if found {
				caches = append(caches, s.toAPIServerlessCache(rec))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeServerlessCachesResponse{
		ServerlessCaches: caches,
	}, nil
}

func (s *Service) ModifyServerlessCache(
	ctx context.Context,
	in *api.ModifyServerlessCacheRequest,
) (*api.ModifyServerlessCacheResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(common.Value(in.ServerlessCacheName))
	if name == "" {
		return nil, invalidParameterValue("ServerlessCacheName is required")
	}

	key := serverlessCacheKey(scope, name)
	var rec *serverlessCacheRecord

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, rErr := storage.Read[serverlessCacheRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !found {
			return serverlessCacheNotFound(name)
		}

		if in.Description != nil {
			existing.Description = *in.Description
		}

		if in.CacheUsageLimits != nil {
			existing.CacheUsageLimits = in.CacheUsageLimits
		}

		if in.SnapshotRetentionLimit != nil {
			existing.SnapshotRetentionLimit = in.SnapshotRetentionLimit
		}

		if in.DailySnapshotTime != nil {
			existing.DailySnapshotTime = *in.DailySnapshotTime
		}

		if len(in.SecurityGroupIds) > 0 {
			existing.SecurityGroupIDs = in.SecurityGroupIds
		}

		if in.MajorEngineVersion != nil || in.Engine != nil {
			targetEngine := existing.Engine
			if in.Engine != nil {
				targetEngine = strings.ToLower(strings.TrimSpace(*in.Engine))
			}

			spec, sErr := resolveEngine(targetEngine, common.Value(in.MajorEngineVersion))
			if sErr != nil {
				return sErr
			}

			existing.Engine = spec.Engine
			existing.FullEngineVersion = spec.EngineVersion
			if in.MajorEngineVersion != nil {
				existing.MajorEngineVersion = *in.MajorEngineVersion
			}
		}

		rec = existing
		return storage.Write(tx, key, *existing)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, "serverless-cache", "Serverless cache modified")
	apiRes := s.toAPIServerlessCache(rec)

	return &api.ModifyServerlessCacheResponse{
		ServerlessCache: &apiRes,
	}, nil
}

func (s *Service) DeleteServerlessCache(
	ctx context.Context,
	in *api.DeleteServerlessCacheRequest,
) (*api.DeleteServerlessCacheResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(common.Value(in.ServerlessCacheName))
	if name == "" {
		return nil, invalidParameterValue("ServerlessCacheName is required")
	}

	key := serverlessCacheKey(scope, name)
	var rec *serverlessCacheRecord

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, rErr := storage.Read[serverlessCacheRecord](tx, key)
		if rErr != nil {
			return rErr
		}

		if !found {
			return serverlessCacheNotFound(name)
		}

		rec = existing
		if s.runtime == nil {
			tx.Delete(key)
			tx.Delete(indexServerlessKey(scope, name))
			rec.Status = statusDeleting
			return nil
		}

		existing.Status = statusDeleting
		return storage.Write(tx, key, *existing)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, "serverless-cache", "Serverless cache deleted")
	apiRes := s.toAPIServerlessCache(rec)

	return &api.DeleteServerlessCacheResponse{
		ServerlessCache: &apiRes,
	}, nil
}
