package rds

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	engineVersionFilter = "engine-version"
	engineModeFilter    = "engine-mode"
	familyFilter        = "db-parameter-group-family"
	provisioned         = "provisioned"
	metadataStorageType = "gp3"
	maxInstanceStorage  = int64(65536)
)

func validateFilters(filters []api.Filter, names ...string) error {
	for _, filter := range filters {
		if !slices.Contains(names, common.Value(filter.Name)) || len(filter.Values) == 0 {
			return invalid("Unsupported filter or empty filter Values")
		}

		for _, value := range filter.Values {
			if value == "" || strings.ContainsAny(value, "*?") {
				return invalid("Empty filter values and wildcards are unsupported")
			}
		}
	}

	return nil
}

func matchesFilters(filters []api.Filter, values map[string]string) bool {
	for _, filter := range filters {
		if !slices.Contains(filter.Values, values[common.Value(filter.Name)]) {
			return false
		}
	}

	return true
}

func pageDiscovery[T any](
	items []T,
	key func(T) string,
	marker *string,
	maximum *int64,
) (page []T, next *string, err error) {
	limit, err := recordLimit(maximum)
	if err != nil {
		return nil, nil, err
	}

	return common.Page(items, key, marker, limit, invalid("Invalid Marker"))
}

type engineVersion struct {
	engine, version, major, family      string
	isDefault, familyDefault, isCluster bool
}

func engineVersions() []engineVersion {
	var out []engineVersion
	for _, item := range standaloneVersions {
		_, defaultVersion, _, _ := Engine(item.engine, "")
		for _, version := range item.versions {
			out = append(out, engineVersion{
				engine: item.engine, version: version, major: version, family: item.engine + version,
				isDefault: version == defaultVersion, familyDefault: true,
			})
		}
	}

	for _, engine := range slices.Sorted(maps.Keys(clusterEngines)) {
		definition := clusterEngines[engine]
		families := make(map[string]bool)
		for i, version := range definition.versions {
			family := clusterFamily(engine, version)
			out = append(out, engineVersion{
				engine: engine, version: version, major: strings.TrimPrefix(family, engine), family: family,
				isDefault: i == 0, familyDefault: !families[family], isCluster: true,
			})
			families[family] = true
		}
	}

	slices.SortFunc(out, func(a, b engineVersion) int {
		return strings.Compare(a.engine+"/"+a.version, b.engine+"/"+b.version)
	})

	return out
}

func matchesValue(filter *string, value string) bool { return filter == nil || *filter == value }

func (s *Service) DescribeDBEngineVersions(
	_ context.Context, in *api.DescribeDBEngineVersionsMessage,
) (*api.DBEngineVersionMessage, error) {
	if err := validateFilters(
		in.Filters,
		engineFilter,
		engineVersionFilter,
		familyFilter,
		engineModeFilter,
		"status",
	); err != nil {
		return nil, err
	}

	var items []api.DBEngineVersion
	for _, item := range engineVersions() {
		if !matchesValue(in.Engine, item.engine) || !matchesValue(in.DBParameterGroupFamily, item.family) ||
			!matchesEngineVersion(in, item) || !matchesFilters(in.Filters, map[string]string{
			engineFilter: item.engine, engineVersionFilter: item.version, familyFilter: item.family,
			engineModeFilter: provisioned, "status": available,
		}) {
			continue
		}

		items = append(items, api.DBEngineVersion{
			Engine:             new(item.engine),
			EngineVersion:      new(item.version),
			MajorEngineVersion: new(item.major),
			DBParameterGroupFamily: new(
				item.family,
			),
			Status:                     new(available),
			SupportedEngineModes:       []string{provisioned},
			DBEngineDescription:        new(item.engine),
			DBEngineVersionDescription: new(item.engine + " " + item.version),
			SupportsReadReplica:        new(!item.isCluster && s.runtime == nil),
		})
	}

	page, marker, err := pageDiscovery(items, func(v api.DBEngineVersion) string {
		return *v.Engine + "/" + *v.EngineVersion
	}, in.Marker, in.MaxRecords)
	return &api.DBEngineVersionMessage{DBEngineVersions: page, Marker: marker}, err
}

func matchesEngineVersion(in *api.DescribeDBEngineVersionsMessage, item engineVersion) bool {
	if !common.Value(in.DefaultOnly) {
		return matchesValue(in.EngineVersion, item.version)
	}

	if in.EngineVersion != nil {
		return item.familyDefault && (*in.EngineVersion == item.major || *in.EngineVersion == item.version)
	}

	for _, filter := range in.Filters {
		if *filter.Name == familyFilter || *filter.Name == engineVersionFilter {
			return item.familyDefault
		}
	}

	if in.DBParameterGroupFamily != nil {
		return item.familyDefault
	}

	return item.isDefault
}

func orderableClasses(item engineVersion, requested *string) []string {
	serverless := aurora(item.engine)
	if requested != nil {
		if !strings.HasPrefix(*requested, "db.") || len(*requested) < 4 ||
			(*requested == "db.serverless" && !serverless) {
			return nil
		}

		return []string{*requested}
	}

	if item.isCluster {
		if serverless {
			return []string{"db.r6g.large", "db.r7g.large", "db.serverless", "db.t4g.medium"}
		}

		return []string{"db.r5.large", "db.r6g.large"}
	}

	return []string{"db.m5.large", "db.t3.micro", "db.t3.small", "db.t4g.micro", "db.t4g.small"}
}

func (s *Service) DescribeOrderableDBInstanceOptions(
	ctx context.Context, in *api.DescribeOrderableDBInstanceOptionsMessage,
) (*api.OrderableDBInstanceOptionsMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.Engine) == "" {
		return nil, invalid("Engine is required")
	}

	const license = "general-public-license"
	var items []api.OrderableDBInstanceOption
	for _, item := range engineVersions() {
		if item.engine != *in.Engine || !matchesValue(in.EngineVersion, item.version) ||
			!matchesValue(in.LicenseModel, license) || (in.Vpc != nil && !*in.Vpc) {
			continue
		}

		for _, class := range orderableClasses(item, in.DBInstanceClass) {
			option := api.OrderableDBInstanceOption{
				Engine:               new(item.engine),
				EngineVersion:        new(item.version),
				DBInstanceClass:      new(class),
				LicenseModel:         new(license),
				Vpc:                  new(true),
				MultiAZCapable:       new(false),
				ReadReplicaCapable:   new(!item.isCluster && s.runtime == nil),
				SupportsClusters:     new(item.isCluster),
				SupportedEngineModes: []string{provisioned},
				AvailabilityZones: []api.AvailabilityZone{
					{Name: new(scope.Region + "a")},
					{Name: new(scope.Region + "b")},
				},
			}
			if !item.isCluster {
				option.StorageType, option.MinStorageSize, option.MaxStorageSize =
					new(metadataStorageType), new(int64(20)), new(maxInstanceStorage)
			}

			items = append(items, option)
		}
	}

	page, marker, err := pageDiscovery(items, func(v api.OrderableDBInstanceOption) string {
		return *v.EngineVersion + "/" + *v.DBInstanceClass
	}, in.Marker, in.MaxRecords)
	return &api.OrderableDBInstanceOptionsMessage{OrderableDBInstanceOptions: page, Marker: marker}, err
}

func (s *Service) DescribeValidDBInstanceModifications(
	ctx context.Context, in *api.DescribeValidDBInstanceModificationsMessage,
) (*api.DescribeValidDBInstanceModificationsResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.snapshotRecord(ctx, scope, kindDB, common.Value(in.DBInstanceIdentifier))
	if err != nil {
		return nil, err
	}

	if common.Value(instance(r).DBInstanceStatus) != available {
		return nil, awserror.New("InvalidDBInstanceStateFault", "The DB instance must be available")
	}

	out := &api.ValidDBInstanceModificationsMessage{}
	if r.Instance.DBClusterIdentifier == nil {
		out.Storage = []api.ValidStorageOptions{
			{
				StorageType:                new(metadataStorageType),
				SupportsStorageAutoscaling: new(false),
				StorageSize: []api.Range{
					{From: r.Instance.AllocatedStorage, To: new(maxInstanceStorage), Step: new(int64(1))},
				},
			},
		}
	}

	return &api.DescribeValidDBInstanceModificationsResult{ValidDBInstanceModificationsMessage: out}, nil
}

func (s *Service) DescribeAccountAttributes(
	ctx context.Context, _ *api.DescribeAccountAttributesMessage,
) (*api.AccountAttributesMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.AccountAttributesMessage{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, quota := range []struct {
			kind, name string
			max        int64
		}{
			{kindDB, "DBInstances", 40},
			{kindCluster, "DBClusters", 40},
			{kindSubnet, "DBSubnetGroups", 50},
			{kindParameter, "DBParameterGroups", 50},
			{kindClusterParameter, "DBClusterParameterGroups", 50},
			{kindOption, "OptionGroups", 20},
			{kindSubscription, "EventSubscriptions", 20},
		} {
			out.AccountQuotas = append(out.AccountQuotas, api.AccountQuota{
				AccountQuotaName: new(quota.name), Max: new(quota.max),
				Used: new(int64(len(tx.Keys(scope.Prefix(serviceName, quota.kind))))),
			})
		}

		return nil
	})
	return out, err
}

func (s *Service) DescribeOptionGroupOptions(
	_ context.Context, in *api.DescribeOptionGroupOptionsMessage,
) (*api.OptionGroupOptionsMessage, error) {
	engine := common.Value(in.EngineName)
	if engine == "" {
		return nil, invalid("EngineName is required")
	}

	var items []api.OptionGroupOption
	if s.runtime == nil && (engine == EngineMySQL || engine == EngineMariaDB) {
		_, version, _, _ := Engine(engine, "")
		if matchesValue(in.MajorEngineVersion, version) {
			items = append(items, api.OptionGroupOption{
				Name:        new(auditPlugin),
				Description: new("MariaDB audit plugin"),
				EngineName:  new(engine),
				MajorEngineVersion: new(
					version,
				),
				PortRequired: new(false),
				Persistent:   new(false),
				Permanent:    new(false),
				OptionGroupOptionSettings: []api.OptionGroupOptionSetting{
					{
						SettingName: new("SERVER_AUDIT_EVENTS"), AllowedValues: new(strings.Join(auditEvents, ",")),
						ApplyType: new("DYNAMIC"), IsModifiable: new(true),
					},
					{SettingName: new("SERVER_AUDIT_EXCL_USERS"), ApplyType: new("DYNAMIC"), IsModifiable: new(true)},
					{SettingName: new("SERVER_AUDIT_INCL_USERS"), ApplyType: new("DYNAMIC"), IsModifiable: new(true)},
				},
			})
		}
	}

	page, marker, err := pageDiscovery(
		items,
		func(v api.OptionGroupOption) string { return *v.Name },
		in.Marker,
		in.MaxRecords,
	)
	return &api.OptionGroupOptionsMessage{OptionGroupOptions: page, Marker: marker}, err
}
