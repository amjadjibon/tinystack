package rds

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindOption          = "og"
	defaultOptionPrefix = "default:"
	auditPlugin         = "MARIADB_AUDIT_PLUGIN"
	optionGroupInUse    = "InvalidOptionGroupStateFault"
	optionPendingApply  = "pending-apply"
	optionPendingRemove = "pending-removal"
)

var optionEngines = []struct {
	engine   string
	versions []string
}{
	{EngineMariaDB, []string{"11"}},
	{EngineMySQL, []string{mysqlFamilyVersion}},
	{EnginePostgres, []string{"14", "15", "16", "17"}},
}

func optionGroupName(engine, version string) string {
	return defaultOptionPrefix + engine + "-" + strings.ReplaceAll(version, ".", "-")
}

func makeOptionGroup(scope request.Scope, name, engine, version, description string) *record {
	return &record{
		Name: name, Kind: kindOption, OptionGroup: &api.OptionGroup{
			OptionGroupName: new(name), OptionGroupArn: new(arn(scope, kindOption, name)),
			EngineName: new(engine), MajorEngineVersion: new(version), OptionGroupDescription: new(description),
			AllowsVpcAndNonVpcInstanceMemberships: new(true),
		},
	}
}

func defaultOptionGroups(scope request.Scope) []*record {
	var out []*record
	for _, item := range optionEngines {
		for _, version := range item.versions {
			name := optionGroupName(item.engine, version)
			out = append(out, makeOptionGroup(scope, name, item.engine, version, "Default option group"))
		}
	}

	return out
}

func (s *Service) setOptionGroup(
	tx storage.ReadTx, scope request.Scope, db *api.DBInstance, name *string,
) error {
	engine, version := common.Value(db.Engine), common.Value(db.EngineVersion)
	group, err := readRecord(
		tx,
		scope,
		kindOption,
		common.Value(common.OrElse(name, new(optionGroupName(engine, version)))),
	)
	if err != nil {
		return err
	}

	if common.Value(group.OptionGroup.EngineName) != engine ||
		common.Value(group.OptionGroup.MajorEngineVersion) != version {
		return invalid("Option group engine and major version must match the DB instance")
	}

	if s.runtime != nil && len(group.OptionGroup.Options) != 0 {
		return invalid("Option groups with configured options are unsupported in Docker mode")
	}

	db.OptionGroupMemberships = optionMembership(group.Name)
	return nil
}

func optionMembership(name string) []api.OptionGroupMembership {
	return []api.OptionGroupMembership{{OptionGroupName: new(name), Status: new(parameterInSync)}}
}

// deferOptionMembership retains the current group until the new membership takes effect.
func deferOptionMembership(db *api.DBInstance, current string) {
	target := common.Value(db.OptionGroupMemberships[0].OptionGroupName)
	if current == target {
		return
	}

	db.OptionGroupMemberships = []api.OptionGroupMembership{
		{OptionGroupName: new(current), Status: new(optionPendingRemove)},
		{OptionGroupName: new(target), Status: new(optionPendingApply)},
	}
}

func pendingOptionGroup(db *api.DBInstance) *string {
	for _, membership := range db.OptionGroupMemberships {
		if common.Value(membership.Status) == optionPendingApply {
			return membership.OptionGroupName
		}
	}

	return nil
}

func (s *Service) CreateOptionGroup(
	ctx context.Context, in *api.CreateOptionGroupMessage,
) (*api.CreateOptionGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name := strings.ToLower(common.Value(in.OptionGroupName))
	engine, version := common.Value(in.EngineName), common.Value(in.MajorEngineVersion)
	description := common.Value(in.OptionGroupDescription)
	if !validName(name, 255) || strings.TrimSpace(description) == "" || len(description) > 255 {
		return nil, invalid("Valid OptionGroupName and OptionGroupDescription are required")
	}

	if _, _, _, engineErr := Engine(engine, version); version == "" || engineErr != nil {
		return nil, invalid("Unsupported EngineName or MajorEngineVersion")
	}

	r := makeOptionGroup(scope, name, engine, version, description)
	r.Tags, err = parseTags(in.Tags)
	if err != nil {
		return nil, err
	}

	if err := s.insert(ctx, scope, r); err != nil {
		return nil, err
	}

	return &api.CreateOptionGroupResult{OptionGroup: r.OptionGroup}, nil
}

func (s *Service) DescribeOptionGroups(
	ctx context.Context, in *api.DescribeOptionGroupsMessage,
) (*api.OptionGroups, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.OptionGroupName != nil && *in.OptionGroupName == "" {
		return nil, invalid("OptionGroupName must be nonempty")
	}

	groups, err := s.list(ctx, scope, kindOption, common.Value(in.OptionGroupName))
	if err != nil {
		return nil, err
	}

	if in.OptionGroupName == nil {
		index, _ := slices.BinarySearchFunc(groups, defaultOptionPrefix, func(r *record, prefix string) int {
			return strings.Compare(r.Name, prefix)
		})
		groups = slices.Insert(groups, index, defaultOptionGroups(scope)...)
	}

	selected := slices.DeleteFunc(groups, func(r *record) bool {
		return (in.EngineName != nil && *in.EngineName != common.Value(r.OptionGroup.EngineName)) ||
			(in.MajorEngineVersion != nil && *in.MajorEngineVersion != common.Value(r.OptionGroup.MajorEngineVersion))
	})

	page, marker, err := pageRecords(selected, in.Marker, in.MaxRecords)
	if err != nil {
		return nil, err
	}

	out := &api.OptionGroups{Marker: marker}
	for _, r := range page {
		out.OptionGroupsList = append(out.OptionGroupsList, *r.OptionGroup)
	}

	return out, nil
}

func optionGroupAttached(tx storage.ReadTx, scope request.Scope, name string) (bool, error) {
	instances, err := storage.Records[record](tx, scope.Prefix(serviceName, kindDB))
	if err != nil {
		return false, err
	}

	return slices.ContainsFunc(instances, func(r *record) bool {
		return slices.ContainsFunc(r.Instance.OptionGroupMemberships, func(group api.OptionGroupMembership) bool {
			return common.Value(group.OptionGroupName) == name
		})
	}), nil
}

func (s *Service) DeleteOptionGroup(
	ctx context.Context, in *api.DeleteOptionGroupMessage,
) (*api.DeleteOptionGroupOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindOption, common.Value(in.OptionGroupName))
		if readErr != nil {
			return readErr
		}

		attached, attachErr := optionGroupAttached(tx, scope, r.Name)
		if attachErr != nil {
			return attachErr
		}

		if attached || strings.HasPrefix(r.Name, defaultOptionPrefix) {
			return awserror.New(optionGroupInUse, "Cannot delete an attached or default option group")
		}

		tx.Delete(scope.Key(serviceName, kindOption, r.Name))
		return nil
	})
	return &api.DeleteOptionGroupOutput{}, err
}

func (s *Service) ModifyOptionGroup(
	ctx context.Context, in *api.ModifyOptionGroupMessage,
) (*api.ModifyOptionGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	out := &api.ModifyOptionGroupResult{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, readErr := readRecord(tx, scope, kindOption, common.Value(in.OptionGroupName))
		if readErr != nil {
			return readErr
		}

		if strings.HasPrefix(r.Name, defaultOptionPrefix) {
			return awserror.New(optionGroupInUse, "Default option groups are immutable")
		}

		if updateErr := updateOptions(r.OptionGroup, in); updateErr != nil {
			return updateErr
		}

		attached, attachErr := optionGroupAttached(tx, scope, r.Name)
		if attachErr != nil {
			return attachErr
		}

		if attached && len(r.OptionGroup.Options) > 0 && s.runtime != nil {
			return invalid("Option groups with configured options are unsupported in Docker mode")
		}

		if attached && !common.Value(in.ApplyImmediately) && len(in.OptionsToInclude)+len(in.OptionsToRemove) > 0 {
			return invalid("Deferred option changes for attached option groups are unsupported")
		}

		out.OptionGroup = r.OptionGroup
		return writeRecord(tx, scope, r)
	})
	return out, err
}

func updateOptions(group *api.OptionGroup, in *api.ModifyOptionGroupMessage) error {
	engine := common.Value(group.EngineName)
	seen := make(map[string]bool)
	for _, name := range in.OptionsToRemove {
		if name != auditPlugin || (engine != EngineMySQL && engine != EngineMariaDB) || seen[name] {
			return invalid("Unsupported or duplicate option " + name)
		}

		seen[name] = true
		group.Options = slices.DeleteFunc(group.Options, func(o api.Option) bool {
			return common.Value(o.OptionName) == name
		})
	}

	for _, option := range in.OptionsToInclude {
		name := common.Value(option.OptionName)
		if name != auditPlugin || (engine != EngineMySQL && engine != EngineMariaDB) || seen[name] {
			return invalid("Unsupported, duplicate or conflicting option " + name)
		}

		seen[name] = true
		index := slices.IndexFunc(group.Options, func(o api.Option) bool { return common.Value(o.OptionName) == name })
		if index == -1 {
			index = len(group.Options)
			group.Options = append(group.Options, api.Option{OptionName: new(name)})
		}

		if err := updateAuditSettings(&group.Options[index], option.OptionSettings); err != nil {
			return err
		}
	}

	return nil
}

func updateAuditSettings(option *api.Option, updates []api.OptionSetting) error {
	seen := make(map[string]bool)
	for _, setting := range updates {
		name := common.Value(setting.Name)
		if seen[name] || setting.Value == nil {
			return invalid("Each option setting requires a distinct name and value")
		}

		seen[name] = true
		switch name {
		case "SERVER_AUDIT_EVENTS":
			for event := range strings.SplitSeq(*setting.Value, ",") {
				switch strings.TrimSpace(event) {
				case "CONNECT", "QUERY", "TABLE", "QUERY_DDL", "QUERY_DML", "QUERY_DML_NO_SELECT", "QUERY_DCL":
				default:
					return invalid("Unsupported SERVER_AUDIT_EVENTS value")
				}
			}
		case "SERVER_AUDIT_EXCL_USERS", "SERVER_AUDIT_INCL_USERS":
			if len(*setting.Value) > 1024 {
				return invalid("Audit user allowlists and denylists must not exceed 1024 characters")
			}
		default:
			return invalid("Unsupported option setting " + name)
		}

		i := slices.IndexFunc(option.OptionSettings, func(s api.OptionSetting) bool {
			return common.Value(s.Name) == name
		})
		if i < 0 {
			option.OptionSettings = append(option.OptionSettings, setting)
		} else {
			option.OptionSettings[i] = setting
		}
	}

	return nil
}
