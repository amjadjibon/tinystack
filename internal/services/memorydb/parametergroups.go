package memorydb

import (
	"context"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/redisparams"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	parameterActiveRehashing = "activerehashing"
	kindParameterGroup       = "parametergroup"
	redisVersion7            = "7.0"
	parameterYes             = "yes"
)

var parameterFamilies = []string{"memorydb_redis6", "memorydb_redis7", "memorydb_valkey7"}

type parameterGroupRecord struct {
	tagSet
	Values                    map[string]string
	Name, Family, Description string
}

func (r *parameterGroupRecord) output(scope request.Scope) *api.ParameterGroup {
	return &api.ParameterGroup{
		Name: new(r.Name), Family: new(r.Family), Description: new(r.Description),
		ARN: new(arn(scope, kindParameterGroup, r.Name)),
	}
}

func defaultGroup(family string) string { return "default." + strings.Replace(family, "_", "-", 1) }
func parameterGroupNotFound(name string) error {
	return fault("ParameterGroupNotFoundFault", "Parameter group %s not found.", name)
}

func readParameterGroup(tx storage.ReadTx, scope request.Scope, name string) (*parameterGroupRecord, error) {
	for _, family := range parameterFamilies {
		if name == defaultGroup(family) {
			return &parameterGroupRecord{
				Name:        name,
				Family:      family,
				Description: "Default parameter group for " + family,
			}, nil
		}
	}

	r, found, err := storage.Read[parameterGroupRecord](tx, key(scope, kindParameterGroup, name))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, parameterGroupNotFound(name)
	}

	return r, nil
}

func (s *Service) CreateParameterGroup(ctx context.Context, in *api.CreateParameterGroupRequest) (
	*api.CreateParameterGroupResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, family := normalize(in.ParameterGroupName), common.Value(in.Family)
	if nameErr := checkName("ParameterGroupName", name); nameErr != nil {
		return nil, nameErr
	}

	if !slices.Contains(parameterFamilies, family) {
		return nil, invalidValue("Unsupported parameter family %s.", family)
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	r := &parameterGroupRecord{
		Name: name, Family: family, Description: common.Value(in.Description),
		Values: map[string]string{}, tagSet: tagSet{Tags: tags},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(key(scope, kindParameterGroup, name)); found {
			return fault("ParameterGroupAlreadyExistsFault", "Parameter group %s already exists.", name)
		}

		if len(tx.Keys(scope.Prefix(serviceName, kindParameterGroup))) >= 100 {
			return fault("ParameterGroupQuotaExceededFault", "Parameter group quota exceeded.")
		}

		return storage.Write(tx, key(scope, kindParameterGroup, name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateParameterGroupResponse{ParameterGroup: r.output(scope)}, nil
}

func (s *Service) DescribeParameterGroups(ctx context.Context, in *api.DescribeParameterGroupsRequest) (
	*api.DescribeParameterGroupsResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var groups []*parameterGroupRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.ParameterGroupName != nil {
			r, e := readParameterGroup(tx, scope, normalize(in.ParameterGroupName))
			groups = append(groups, r)
			return e
		}

		for _, family := range parameterFamilies {
			r, e := readParameterGroup(tx, scope, defaultGroup(family))
			if e != nil {
				return e
			}

			groups = append(groups, r)
		}

		stored, e := storage.Records[parameterGroupRecord](tx, scope.Prefix(serviceName, kindParameterGroup))
		groups = append(groups, stored...)
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		groups,
		func(r *parameterGroupRecord) string { return r.Name },
		in.NextToken,
		limit,
		invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeParameterGroupsResponse{NextToken: next, ParameterGroups: []api.ParameterGroup{}}
	for _, r := range page {
		out.ParameterGroups = append(out.ParameterGroups, *r.output(scope))
	}

	return out, nil
}

func (s *Service) DeleteParameterGroup(ctx context.Context, in *api.DeleteParameterGroupRequest) (
	*api.DeleteParameterGroupResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ParameterGroup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name := normalize(in.ParameterGroupName)
		r, e := readParameterGroup(tx, scope, name)
		if e != nil {
			return e
		}

		if strings.HasPrefix(name, "default.") {
			return fault("InvalidParameterGroupStateFault", "Default parameter groups are immutable.")
		}

		clusters, e := storage.Records[clusterRecord](tx, scope.Prefix(serviceName, kindCluster))
		if e != nil {
			return e
		}

		for _, c := range clusters {
			if c.ParameterGroupName == name {
				return fault("InvalidParameterGroupStateFault", "Parameter group is in use.")
			}
		}

		out = r.output(scope)
		tx.Delete(key(scope, kindParameterGroup, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteParameterGroupResponse{ParameterGroup: out}, nil
}

func (s *Service) changeParameters(ctx context.Context, name string, change func(*parameterGroupRecord) error) (
	*api.ParameterGroup, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.ParameterGroup
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readParameterGroup(tx, scope, name)
		if e != nil {
			return e
		}

		if strings.HasPrefix(name, "default.") {
			return fault("InvalidParameterGroupStateFault", "Default parameter groups are immutable.")
		}

		if r.Values == nil {
			r.Values = map[string]string{}
		}

		oldValues := map[string]string{}
		for k, v := range r.Values {
			oldValues[k] = v
		}

		if changeErr := change(r); changeErr != nil {
			return changeErr
		}

		clusters, e := storage.Records[clusterRecord](tx, scope.Prefix(serviceName, kindCluster))
		if e != nil {
			return e
		}

		for _, c := range clusters {
			if c.ParameterGroupName != name {
				continue
			}

			if c.EngineVersion == redisVersion7 &&
				(r.Values["search-enabled"] == parameterYes || r.Values["search-query-timeout-ms"] != "") {
				return invalidCombination("Search parameters require engine version 7.1 or later.")
			}

			if s.runtime != nil &&
				(r.Values["search-enabled"] == parameterYes || r.Values["search-query-timeout-ms"] != "") {
				return invalidValue("Search module parameters are unsupported in Docker mode.")
			}

			for _, parameter := range []string{parameterActiveRehashing, "search-enabled"} {
				if conditionalParameter(r.Values, parameter) != conditionalParameter(oldValues, parameter) {
					return fault(
						"InvalidParameterGroupStateFault",
						"Parameter %s cannot change while the group is in use.",
						parameter,
					)
				}
			}
		}

		if s.runtime != nil {
			clusters, e := storage.Records[clusterRecord](tx, scope.Prefix(serviceName, kindCluster))
			if e != nil {
				return e
			}

			for _, c := range clusters {
				if c.ParameterGroupName != name {
					continue
				}

				c.ParameterGroupStatus = parameterApplying
				c.ParameterRevision++
				if e := storage.Write(tx, key(scope, kindCluster, c.Name), c); e != nil {
					return e
				}
			}
		}

		out = r.output(scope)
		return storage.Write(tx, key(scope, kindParameterGroup, name), r)
	})
	return out, err
}

func (s *Service) UpdateParameterGroup(ctx context.Context, in *api.UpdateParameterGroupRequest) (
	*api.UpdateParameterGroupResponse, error,
) {
	if len(in.ParameterNameValues) == 0 || len(in.ParameterNameValues) > 20 {
		return nil, invalidValue("ParameterNameValues must contain between 1 and 20 parameters.")
	}

	out, err := s.changeParameters(ctx, normalize(in.ParameterGroupName), func(r *parameterGroupRecord) error {
		defs := redisparams.MemoryDB(r.Family)
		for _, p := range in.ParameterNameValues {
			name := common.Value(p.ParameterName)
			i := slices.IndexFunc(defs, func(d redisparams.Definition) bool { return d.Name == name })
			if i < 0 || p.ParameterValue == nil {
				return invalidValue("Unknown parameter or missing value: %s.", name)
			}

			if e := redisparams.Validate(&defs[i], *p.ParameterValue); e != nil {
				return invalidValue("%s", e)
			}

			r.Values[name] = *p.ParameterValue
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateParameterGroupResponse{ParameterGroup: out}, nil
}

func (s *Service) ResetParameterGroup(ctx context.Context, in *api.ResetParameterGroupRequest) (
	*api.ResetParameterGroupResponse, error,
) {
	all := common.Value(in.AllParameters)
	if all && len(in.ParameterNames) > 0 || !all && len(in.ParameterNames) == 0 {
		return nil, invalidCombination("Specify AllParameters or ParameterNames exclusively.")
	}

	out, err := s.changeParameters(ctx, normalize(in.ParameterGroupName), func(r *parameterGroupRecord) error {
		if all {
			r.Values = map[string]string{}
			return nil
		}

		defs := redisparams.MemoryDB(r.Family)
		for _, name := range in.ParameterNames {
			if !slices.ContainsFunc(defs, func(d redisparams.Definition) bool { return d.Name == name }) {
				return invalidValue("Unknown parameter %s.", name)
			}

			delete(r.Values, name)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ResetParameterGroupResponse{ParameterGroup: out}, nil
}

func (s *Service) DescribeParameters(ctx context.Context, in *api.DescribeParametersRequest) (
	*api.DescribeParametersResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var r *parameterGroupRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readParameterGroup(tx, scope, normalize(in.ParameterGroupName))
		return e
	})
	if err != nil {
		return nil, err
	}

	defs, next, err := common.Page(redisparams.MemoryDB(r.Family), func(d redisparams.Definition) string {
		return d.Name
	}, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	out := &api.DescribeParametersResponse{NextToken: next, Parameters: []api.Parameter{}}
	for _, d := range defs {
		value, ok := r.Values[d.Name]
		if !ok {
			value = d.DefaultValue
		}

		out.Parameters = append(out.Parameters, api.Parameter{
			Name: new(d.Name), Value: new(value),
			Description: new(d.Description), DataType: new(d.DataType), AllowedValues: new(d.AllowedValues),
			MinimumEngineVersion: new(d.MinEngineVer),
		})
	}

	return out, nil
}

// conditionalParameter resolves defaults for settings that cannot change on an attached group.
func conditionalParameter(values map[string]string, name string) string {
	fallback := "no"
	if name == parameterActiveRehashing {
		fallback = parameterYes
	}

	return *common.OrElse(optional(values[name]), new(fallback))
}
