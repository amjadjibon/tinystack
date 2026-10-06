package elasticache

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	EngineRedis     = "redis"
	EngineValkey    = "valkey"
	EngineMemcached = "memcached"

	descRedis     = "Redis"
	descValkey    = "Valkey"
	binRedis      = "redis-server"
	binValkey     = "valkey-server"
	familyRedis7  = "redis7"
	familyValkey8 = "valkey8"
)

type engineSpec struct {
	Engine             string
	EngineVersion      string
	Family             string
	Description        string
	VersionDescription string
	Image              string
	Binary             string
	DefaultPort        int64
	IsDefault          bool
}

var supportedEngines = []engineSpec{
	{
		Engine:             EngineRedis,
		EngineVersion:      "6.2",
		Family:             "redis6.x",
		Description:        descRedis,
		VersionDescription: "Redis version 6.2",
		Image:              "redis:6.2-alpine",
		Binary:             binRedis,
		DefaultPort:        6379,
		IsDefault:          false,
	},
	{
		Engine:             EngineRedis,
		EngineVersion:      "7.0",
		Family:             familyRedis7,
		Description:        descRedis,
		VersionDescription: "Redis version 7.0",
		Image:              "redis:7.0-alpine",
		Binary:             binRedis,
		DefaultPort:        6379,
		IsDefault:          false,
	},
	{
		Engine:             EngineRedis,
		EngineVersion:      "7.1",
		Family:             familyRedis7,
		Description:        descRedis,
		VersionDescription: "Redis version 7.1",
		Image:              "redis:7.1-alpine",
		Binary:             binRedis,
		DefaultPort:        6379,
		IsDefault:          true,
	},
	{
		Engine:             EngineValkey,
		EngineVersion:      "7.2",
		Family:             "valkey7",
		Description:        descValkey,
		VersionDescription: "Valkey version 7.2",
		Image:              "valkey/valkey:7.2-alpine",
		Binary:             binValkey,
		DefaultPort:        6379,
		IsDefault:          false,
	},
	{
		Engine:             EngineValkey,
		EngineVersion:      "8.0",
		Family:             familyValkey8,
		Description:        descValkey,
		VersionDescription: "Valkey version 8.0",
		Image:              "valkey/valkey:8.0-alpine",
		Binary:             binValkey,
		DefaultPort:        6379,
		IsDefault:          false,
	},
	{
		Engine:             EngineValkey,
		EngineVersion:      "8.1",
		Family:             familyValkey8,
		Description:        descValkey,
		VersionDescription: "Valkey version 8.1",
		Image:              "valkey/valkey:8.1-alpine",
		Binary:             binValkey,
		DefaultPort:        6379,
		IsDefault:          true,
	},
	{
		Engine:             EngineMemcached,
		EngineVersion:      "1.6.22",
		Family:             "memcached1.6",
		Description:        "Memcached",
		VersionDescription: "Memcached version 1.6.22",
		Image:              "memcached:1.6.22-alpine",
		Binary:             "memcached",
		DefaultPort:        11211,
		IsDefault:          true,
	},
}

func resolveEngine(engine, version string) (engineSpec, error) {
	eng := strings.ToLower(strings.TrimSpace(engine))
	if eng == "" {
		return engineSpec{}, invalidParameterValue("Engine is required")
	}

	ver := strings.TrimSpace(version)
	if ver == "" {
		for i := range supportedEngines {
			e := &supportedEngines[i]
			if e.Engine == eng && e.IsDefault {
				return *e, nil
			}
		}

		return engineSpec{}, invalidParameterValue("Cannot find engine %s", eng)
	}

	// Support major version shortcuts like "7" -> "7.1", "8" -> "8.1", "6" -> "6.2"
	for i := range supportedEngines {
		e := &supportedEngines[i]
		if e.Engine == eng && (e.EngineVersion == ver || strings.HasPrefix(e.EngineVersion, ver+".")) {
			return *e, nil
		}
	}

	return engineSpec{}, invalidParameterValue("Cannot find version %s for engine %s", ver, eng)
}

func (s *Service) DescribeCacheEngineVersions(
	_ context.Context,
	in *api.DescribeCacheEngineVersionsMessage,
) (*api.CacheEngineVersionMessage, error) {
	var targetEngine, targetVersion, targetFamily string
	if in.Engine != nil {
		targetEngine = strings.ToLower(common.Value(in.Engine))
	}

	if in.EngineVersion != nil {
		targetVersion = common.Value(in.EngineVersion)
	}

	if in.CacheParameterGroupFamily != nil {
		targetFamily = common.Value(in.CacheParameterGroupFamily)
	}

	defaultOnly := in.DefaultOnly != nil && *in.DefaultOnly

	var matched []api.CacheEngineVersion
	for i := range supportedEngines {
		spec := &supportedEngines[i]
		if targetEngine != "" && spec.Engine != targetEngine {
			continue
		}

		if targetVersion != "" && spec.EngineVersion != targetVersion {
			continue
		}

		if targetFamily != "" && spec.Family != targetFamily {
			continue
		}

		if defaultOnly && !spec.IsDefault {
			continue
		}

		matched = append(matched, api.CacheEngineVersion{
			CacheEngineDescription:        new(spec.Description),
			CacheEngineVersionDescription: new(spec.VersionDescription),
			CacheParameterGroupFamily:     new(spec.Family),
			Engine:                        new(spec.Engine),
			EngineVersion:                 new(spec.EngineVersion),
		})
	}

	return &api.CacheEngineVersionMessage{
		CacheEngineVersions: matched,
	}, nil
}
