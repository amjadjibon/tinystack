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

const (
	typeString  = "string"
	typeInteger = "integer"
	minVerRedis = "6.0.0"
)

type paramDefinition struct {
	Name          string
	Description   string
	DataType      string
	AllowedValues string
	DefaultValue  string
	MinEngineVer  string
	IsModifiable  bool
	Dynamic       bool
}

var redisParams = []paramDefinition{
	{
		Name:        "maxmemory-policy",
		Description: "How Redis will select what to remove when maxmemory is reached",
		DataType:    typeString,
		AllowedValues: "volatile-lru,allkeys-lru,volatile-lfu,allkeys-lfu,volatile-random," +
			"allkeys-random,volatile-ttl,noeviction",
		DefaultValue: "volatile-lru",
		IsModifiable: true,
		Dynamic:      true,
		MinEngineVer: minVerRedis,
	},
	{
		Name:          "timeout",
		Description:   "Close connection after client is idle for N seconds (0 to disable)",
		DataType:      typeInteger,
		AllowedValues: "0-31536000",
		DefaultValue:  "0",
		IsModifiable:  true,
		Dynamic:       true,
		MinEngineVer:  minVerRedis,
	},
	{
		Name:          "notify-keyspace-events",
		Description:   "Events to notify via Pub/Sub",
		DataType:      typeString,
		AllowedValues: "K,E,g,$,l,s,h,z,x,e,A,t,m",
		DefaultValue:  "",
		IsModifiable:  true,
		Dynamic:       true,
		MinEngineVer:  minVerRedis,
	},
	{
		Name:          "databases",
		Description:   "Set the number of databases",
		DataType:      typeInteger,
		AllowedValues: "1-1000000",
		DefaultValue:  "16",
		IsModifiable:  true,
		Dynamic:       false,
		MinEngineVer:  minVerRedis,
	},
	{
		Name:          "appendonly",
		Description:   "Enable append only mode",
		DataType:      typeString,
		AllowedValues: "yes,no",
		DefaultValue:  "no",
		IsModifiable:  true,
		Dynamic:       true,
		MinEngineVer:  minVerRedis,
	},
}

var memcachedParams = []paramDefinition{
	{
		Name:          "max_item_size",
		Description:   "Maximum item size in bytes",
		DataType:      typeInteger,
		AllowedValues: "1048576-134217728",
		DefaultValue:  "1048576",
		IsModifiable:  true,
		Dynamic:       false,
		MinEngineVer:  "1.4.0",
	},
	{
		Name:          "max_simultaneous_connections_per_node",
		Description:   "Maximum simultaneous connections",
		DataType:      typeInteger,
		AllowedValues: "1024-65535",
		DefaultValue:  "1024",
		IsModifiable:  true,
		Dynamic:       false,
		MinEngineVer:  "1.4.0",
	},
}

var defaultFamilies = []string{
	"redis6.x",
	familyRedis7,
	"valkey7",
	familyValkey8,
	"memcached1.6",
}

type parameterGroupRecord struct {
	UserValues  map[string]string
	Tags        map[string]string
	Name        string
	Family      string
	Description string
}

func isDefaultParameterGroup(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "default.")
}

func familyForDefaultGroup(name string) (string, bool) {
	lower := strings.ToLower(name)
	for _, fam := range defaultFamilies {
		if lower == "default."+fam {
			return fam, true
		}
	}

	return "", false
}

func getFamilyParams(family string) []paramDefinition {
	lower := strings.ToLower(family)
	if strings.HasPrefix(lower, "memcached") {
		return memcachedParams
	}

	return redisParams
}

func toAPIParameterGroup(scope request.Scope, name, family, description string, isGlobal bool) api.CacheParameterGroup {
	arn := parameterGroupARN(scope, name)
	return api.CacheParameterGroup{
		CacheParameterGroupName:   new(name),
		CacheParameterGroupFamily: new(family),
		Description:               new(description),
		IsGlobal:                  new(isGlobal),
		ARN:                       new(arn),
	}
}

func listDefaultParameterGroups(scope request.Scope) []api.CacheParameterGroup {
	out := make([]api.CacheParameterGroup, 0, len(defaultFamilies))
	for _, fam := range defaultFamilies {
		name := "default." + fam
		desc := "Default parameter group for " + fam
		out = append(out, toAPIParameterGroup(scope, name, fam, desc, true))
	}

	return out
}

func (s *Service) CreateCacheParameterGroup(
	ctx context.Context,
	in *api.CreateCacheParameterGroupMessage,
) (*api.CreateCacheParameterGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheParameterGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheParameterGroupName is required.")
	}

	if isDefaultParameterGroup(name) {
		return nil, invalidParameterGroupState("A parameter group cannot begin with 'default.'.")
	}

	family := common.Value(in.CacheParameterGroupFamily)
	if family == "" {
		return nil, invalidParameterValue("CacheParameterGroupFamily is required.")
	}

	var validFamily bool
	for _, fam := range defaultFamilies {
		if strings.EqualFold(fam, family) {
			validFamily = true
			family = fam
			break
		}
	}

	if !validFamily {
		return nil, invalidParameterValue("Invalid parameter group family %s.", family)
	}

	desc := common.Value(in.Description)
	if desc == "" {
		return nil, invalidParameterValue("Description is required.")
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

	key := parameterGroupKey(scope, name)
	record := parameterGroupRecord{
		Name:        name,
		Family:      family,
		Description: desc,
		UserValues:  make(map[string]string),
		Tags:        tags,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found, readErr := storage.Read[parameterGroupRecord](tx, key); readErr != nil {
			return readErr
		} else if found {
			return parameterGroupAlreadyExists(name)
		}

		return storage.Write(tx, key, record)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, sourceTypeCacheParameterGroup, "Cache parameter group "+name+" created")

	group := toAPIParameterGroup(scope, record.Name, record.Family, record.Description, false)
	return &api.CreateCacheParameterGroupResult{
		CacheParameterGroup: &group,
	}, nil
}

func (s *Service) DescribeCacheParameterGroups(
	ctx context.Context,
	in *api.DescribeCacheParameterGroupsMessage,
) (*api.CacheParameterGroupsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	targetName := common.Value(in.CacheParameterGroupName)
	if targetName != "" {
		if isDefaultParameterGroup(targetName) {
			fam, ok := familyForDefaultGroup(targetName)
			if !ok {
				return nil, parameterGroupNotFound(targetName)
			}

			group := toAPIParameterGroup(scope, "default."+fam, fam, "Default parameter group for "+fam, true)
			return &api.CacheParameterGroupsMessage{
				CacheParameterGroups: []api.CacheParameterGroup{group},
			}, nil
		}

		var group api.CacheParameterGroup
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			r, found, readErr := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, targetName))
			if readErr != nil {
				return readErr
			}

			if !found {
				return parameterGroupNotFound(targetName)
			}

			group = toAPIParameterGroup(scope, r.Name, r.Family, r.Description, false)
			return nil
		})
		if err != nil {
			return nil, err
		}

		return &api.CacheParameterGroupsMessage{
			CacheParameterGroups: []api.CacheParameterGroup{group},
		}, nil
	}

	groups := listDefaultParameterGroups(scope)
	prefix := scope.Prefix(serviceName, "parameter-group")

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[parameterGroupRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, r := range records {
			groups = append(groups, toAPIParameterGroup(scope, r.Name, r.Family, r.Description, false))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CacheParameterGroupsMessage{
		CacheParameterGroups: groups,
	}, nil
}

func (s *Service) DescribeCacheParameters(
	ctx context.Context,
	in *api.DescribeCacheParametersMessage,
) (*api.CacheParameterGroupDetails, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheParameterGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheParameterGroupName is required.")
	}

	var (
		family     string
		userValues map[string]string
	)

	if isDefaultParameterGroup(name) {
		fam, ok := familyForDefaultGroup(name)
		if !ok {
			return nil, parameterGroupNotFound(name)
		}

		family = fam
		userValues = map[string]string{}
	} else {
		err = s.store.View(ctx, func(tx storage.ReadTx) error {
			r, found, readErr := storage.Read[parameterGroupRecord](tx, parameterGroupKey(scope, name))
			if readErr != nil {
				return readErr
			}

			if !found {
				return parameterGroupNotFound(name)
			}

			family = r.Family
			userValues = r.UserValues
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sourceFilter := strings.ToLower(common.Value(in.Source))
	defs := getFamilyParams(family)

	var params []api.Parameter
	for _, def := range defs {
		val := def.DefaultValue
		source := "system"
		if uVal, ok := userValues[def.Name]; ok {
			val = uVal
			source = "user"
		}

		if sourceFilter != "" && source != sourceFilter {
			continue
		}

		changeType := "immediate"
		if !def.Dynamic {
			changeType = "requires-reboot"
		}

		params = append(params, api.Parameter{
			ParameterName:        new(def.Name),
			ParameterValue:       new(val),
			Description:          new(def.Description),
			Source:               new(source),
			DataType:             new(def.DataType),
			AllowedValues:        new(def.AllowedValues),
			IsModifiable:         new(def.IsModifiable),
			MinimumEngineVersion: new(def.MinEngineVer),
			ChangeType:           new(changeType),
		})
	}

	return &api.CacheParameterGroupDetails{
		Parameters: params,
	}, nil
}

func (s *Service) DescribeEngineDefaultParameters(
	_ context.Context,
	in *api.DescribeEngineDefaultParametersMessage,
) (*api.DescribeEngineDefaultParametersResult, error) {
	family := common.Value(in.CacheParameterGroupFamily)
	if family == "" {
		return nil, invalidParameterValue("CacheParameterGroupFamily is required.")
	}

	var validFamily bool
	for _, fam := range defaultFamilies {
		if strings.EqualFold(fam, family) {
			validFamily = true
			family = fam
			break
		}
	}

	if !validFamily {
		return nil, invalidParameterValue("Invalid parameter group family %s.", family)
	}

	defs := getFamilyParams(family)
	var params []api.Parameter
	for _, def := range defs {
		changeType := "immediate"
		if !def.Dynamic {
			changeType = "requires-reboot"
		}

		source := "system"
		params = append(params, api.Parameter{
			ParameterName:        new(def.Name),
			ParameterValue:       new(def.DefaultValue),
			Description:          new(def.Description),
			Source:               new(source),
			DataType:             new(def.DataType),
			AllowedValues:        new(def.AllowedValues),
			IsModifiable:         new(def.IsModifiable),
			MinimumEngineVersion: new(def.MinEngineVer),
			ChangeType:           new(changeType),
		})
	}

	return &api.DescribeEngineDefaultParametersResult{
		EngineDefaults: &api.EngineDefaults{
			CacheParameterGroupFamily: new(family),
			Parameters:                params,
		},
	}, nil
}

func (s *Service) ModifyCacheParameterGroup(
	ctx context.Context,
	in *api.ModifyCacheParameterGroupMessage,
) (*api.CacheParameterGroupNameMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheParameterGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheParameterGroupName is required.")
	}

	if isDefaultParameterGroup(name) {
		return nil, invalidParameterGroupState("Default parameter groups cannot be modified.")
	}

	key := parameterGroupKey(scope, name)
	var existing *parameterGroupRecord

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[parameterGroupRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return parameterGroupNotFound(name)
		}

		existing = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	defs := getFamilyParams(existing.Family)
	defMap := make(map[string]paramDefinition, len(defs))
	for _, d := range defs {
		defMap[d.Name] = d
	}

	if existing.UserValues == nil {
		existing.UserValues = make(map[string]string)
	}

	for _, pnv := range in.ParameterNameValues {
		pName := common.Value(pnv.ParameterName)
		pVal := common.Value(pnv.ParameterValue)

		d, ok := defMap[pName]
		if !ok {
			return nil, invalidParameterValue("Parameter %s is not valid for family %s.", pName, existing.Family)
		}

		if !d.IsModifiable {
			return nil, invalidParameterValue("Parameter %s is not modifiable.", pName)
		}

		if strings.HasPrefix(existing.Family, "memcached") {
			if _, fErr := memcachedParamFlags(pName, pVal); fErr != nil {
				return nil, fErr
			}
		}

		existing.UserValues[pName] = pVal
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key, existing)
	})
	if err != nil {
		return nil, err
	}

	if s.runtime != nil {
		s.applyParameterGroupChanges(ctx, scope, name, in.ParameterNameValues, defMap)
	}

	s.recordEvent(ctx, scope, name, sourceTypeCacheParameterGroup, "Cache parameter group "+name+" modified")

	return &api.CacheParameterGroupNameMessage{
		CacheParameterGroupName: new(name),
	}, nil
}

func (s *Service) ResetCacheParameterGroup(
	ctx context.Context,
	in *api.ResetCacheParameterGroupMessage,
) (*api.CacheParameterGroupNameMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheParameterGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheParameterGroupName is required.")
	}

	if isDefaultParameterGroup(name) {
		return nil, invalidParameterGroupState("Default parameter groups cannot be modified.")
	}

	key := parameterGroupKey(scope, name)
	var existing *parameterGroupRecord

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, readErr := storage.Read[parameterGroupRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return parameterGroupNotFound(name)
		}

		existing = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	if in.ResetAllParameters != nil && *in.ResetAllParameters {
		existing.UserValues = make(map[string]string)
	} else {
		for _, pnv := range in.ParameterNameValues {
			delete(existing.UserValues, common.Value(pnv.ParameterName))
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key, existing)
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, sourceTypeCacheParameterGroup, "Cache parameter group "+name+" reset")

	return &api.CacheParameterGroupNameMessage{
		CacheParameterGroupName: new(name),
	}, nil
}

func (s *Service) DeleteCacheParameterGroup(
	ctx context.Context,
	in *api.DeleteCacheParameterGroupMessage,
) (*api.DeleteCacheParameterGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.CacheParameterGroupName)
	if name == "" {
		return nil, invalidParameterValue("CacheParameterGroupName is required.")
	}

	if isDefaultParameterGroup(name) {
		return nil, invalidParameterGroupState("Default parameter groups cannot be deleted.")
	}

	key := parameterGroupKey(scope, name)

	// Check if any cluster in scope uses this parameter group
	clusterPrefix := scope.Prefix(serviceName, "cluster")
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[parameterGroupRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return parameterGroupNotFound(name)
		}

		clusters, cErr := storage.Records[clusterRecord](tx, clusterPrefix)
		if cErr != nil {
			return cErr
		}

		for _, c := range clusters {
			if strings.EqualFold(c.ParameterGroupName, name) {
				return invalidParameterGroupState("Parameter group %s is being used by existing cache clusters.", name)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.recordEvent(ctx, scope, name, sourceTypeCacheParameterGroup, "Cache parameter group "+name+" deleted")

	return &api.DeleteCacheParameterGroupOutput{}, nil
}

func (s *Service) applyParameterGroupChanges(
	ctx context.Context,
	scope request.Scope,
	groupName string,
	pnvs []api.ParameterNameValue,
	defs map[string]paramDefinition,
) {
	var clusters []clusterRecord
	_ = s.store.View(ctx, func(tx storage.ReadTx) error {
		all, _ := storage.Records[clusterRecord](tx, clusterPrefix(scope))
		for _, cl := range all {
			if strings.EqualFold(cl.ParameterGroupName, groupName) {
				clusters = append(clusters, *cl)
			}
		}
		return nil
	})

	for i := range clusters {
		cl := &clusters[i]
		hasStatic := false
		if cl.Engine == EngineRedis || cl.Engine == EngineValkey {
			for _, pnv := range pnvs {
				pName := common.Value(pnv.ParameterName)
				pVal := common.Value(pnv.ParameterValue)
				d := defs[pName]
				if d.Dynamic && cl.Status == statusAvailable {
					for _, node := range cl.Nodes {
						if node.Port > 0 {
							_ = redisConfigSet(ctx, "127.0.0.1", node.Port, pName, pVal, 2*time.Second)
						}
					}
				} else {
					hasStatic = true
				}
			}
		} else {
			hasStatic = true
		}

		if hasStatic {
			cl.ParameterApplyStatus = "pending-reboot"
			_ = s.store.Update(ctx, func(tx storage.WriteTx) error {
				return storage.Write(tx, clusterKey(scope, cl.ClusterID), cl)
			})
		}
	}
}
