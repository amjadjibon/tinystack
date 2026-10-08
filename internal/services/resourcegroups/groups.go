package resourcegroups

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroups"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	filterResourceType = "resource-type"

	maxConfigurations = 2
	maxParameters     = 100

	statusComplete = "UPDATE_COMPLETE"
)

// checkText validates a text member of a group: at most max characters that
// pattern allows.
func checkText(name string, v *string, limit int, pattern interface{ MatchString(string) bool }) error {
	if v == nil {
		return nil
	}

	if utf8.RuneCountInString(*v) > limit || !pattern.MatchString(*v) {
		return bad("%s is not valid: it can be at most %d characters of letters, numbers, and _.:/=+-@.", name, limit)
	}

	return nil
}

func checkCriticality(v *int64) error {
	if v != nil && (*v < 1 || *v > 10) {
		return bad("Criticality must be between 1 and 10.")
	}

	return nil
}

// checkConfiguration validates a group's configuration items.
func checkConfiguration(items []api.GroupConfigurationItem) error {
	if len(items) > maxConfigurations {
		return bad("Configuration can hold at most %d items.", maxConfigurations)
	}

	for _, item := range items {
		t := common.Value(item.Type)
		if len(t) > 40 || !typePattern.MatchString(t) {
			return bad("%q is not a valid group configuration type.", t)
		}

		if len(item.Parameters) > maxParameters {
			return bad("A configuration item can hold at most %d parameters.", maxParameters)
		}

		for _, p := range item.Parameters {
			if n := len(common.Value(p.Name)); n < 1 || n > 80 {
				return bad("Each configuration parameter needs a name of 1 to 80 characters.")
			}
		}
	}

	return nil
}

func (g *group) configuration() *api.GroupConfiguration {
	if len(g.Configuration) == 0 {
		return nil
	}

	return &api.GroupConfiguration{Configuration: g.Configuration, Status: new(statusComplete)}
}

func (s *Service) CreateGroup(ctx context.Context, in *api.CreateGroupInput) (*api.CreateGroupOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	g := &group{
		Name: common.Value(in.Name), Description: common.Value(in.Description), Criticality: in.Criticality,
		Owner: common.Value(in.Owner), DisplayName: common.Value(in.DisplayName), Query: common.Clone(in.ResourceQuery),
		Configuration: in.Configuration, Tags: common.CloneTags(in.Tags),
	}
	if n := utf8.RuneCountInString(g.Name); n < 1 || n > maxName || !namePattern.MatchString(g.Name) {
		return nil, bad("Name must be 1 to %d characters of letters, numbers, hyphens, periods, and underscores.",
			maxName)
	}

	if strings.HasPrefix(strings.ToLower(g.Name), "aws") {
		return nil, bad("Group names cannot start with AWS.")
	}

	if validationErr := cmp.Or(
		checkText("Description", in.Description, maxDescription, descriptionPattern),
		checkText("Owner", in.Owner, maxText, textPattern),
		checkText("DisplayName", in.DisplayName, maxText, textPattern),
		checkCriticality(in.Criticality),
		checkConfiguration(in.Configuration),
	); validationErr != nil {
		return nil, validationErr
	}

	if g.Query == nil && len(g.Configuration) == 0 {
		return nil, bad("Either ResourceQuery or Configuration is required.")
	}

	if g.Query != nil {
		if queryErr := checkQuery(g.Query); queryErr != nil {
			return nil, queryErr
		}
	}

	if tagErr := tagging.CheckSet(g.Tags); tagErr != nil {
		return nil, bad("%s", tagErr.Error())
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		key := m.Key(service, kindGroup, g.Name)
		if _, found := tx.Get(key); found {
			return bad("A group named %s already exists.", g.Name)
		}

		return storage.Write(tx, key, g)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateGroupOutput{
		Group: g.output(m.Scope), ResourceQuery: g.Query, Tags: g.Tags, GroupConfiguration: g.configuration(),
	}, nil
}

func (s *Service) GetGroup(ctx context.Context, in *api.GetGroupInput) (*api.GetGroupOutput, error) {
	m, g, err := s.read(ctx, in.Group, in.GroupName)
	if err != nil {
		return nil, err
	}

	return &api.GetGroupOutput{Group: g.output(m.Scope)}, nil
}

// read loads the group a request names.
func (s *Service) read(ctx context.Context, current, deprecated *string) (request.Metadata, *group, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return m, nil, err
	}

	name, err := groupName(m.Scope, current, deprecated)
	if err != nil {
		return m, nil, err
	}

	var g *group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		g, err = load(tx, m.Scope, name)
		return err
	})

	return m, g, err
}

// change reads the group a request names, applies edit to it, and saves it.
func (s *Service) change(
	ctx context.Context, current, deprecated *string, edit func(g *group) error,
) (request.Metadata, *group, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return m, nil, err
	}

	name, err := groupName(m.Scope, current, deprecated)
	if err != nil {
		return m, nil, err
	}

	var g *group
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if g, loadErr = load(tx, m.Scope, name); loadErr != nil {
			return loadErr
		}

		if editErr := edit(g); editErr != nil {
			return editErr
		}

		return storage.Write(tx, m.Key(service, kindGroup, name), g)
	})

	return m, g, err
}

func (s *Service) DeleteGroup(ctx context.Context, in *api.DeleteGroupInput) (*api.DeleteGroupOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, err := groupName(m.Scope, in.Group, in.GroupName)
	if err != nil {
		return nil, err
	}

	var g *group
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var loadErr error
		if g, loadErr = load(tx, m.Scope, name); loadErr != nil {
			return loadErr
		}

		tx.Delete(m.Key(service, kindGroup, name))

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteGroupOutput{Group: g.output(m.Scope)}, nil
}

func (s *Service) UpdateGroup(ctx context.Context, in *api.UpdateGroupInput) (*api.UpdateGroupOutput, error) {
	if err := cmp.Or(
		checkText("Description", in.Description, maxDescription, descriptionPattern),
		checkText("Owner", in.Owner, maxText, textPattern),
		checkText("DisplayName", in.DisplayName, maxText, textPattern),
		checkCriticality(in.Criticality),
	); err != nil {
		return nil, err
	}

	m, g, err := s.change(ctx, in.Group, in.GroupName, func(g *group) error {
		g.Description = common.Value(common.OrElse(in.Description, &g.Description))
		g.Owner = common.Value(common.OrElse(in.Owner, &g.Owner))
		g.DisplayName = common.Value(common.OrElse(in.DisplayName, &g.DisplayName))
		g.Criticality = common.OrElse(in.Criticality, g.Criticality)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateGroupOutput{Group: g.output(m.Scope)}, nil
}

// matchesFilters reports whether a group passes every ListGroups filter: it
// matches one of each filter's values.
func (g *group) matchesFilters(filters []api.GroupFilter) bool {
	for _, f := range filters {
		ok := false
		for _, v := range f.Values {
			ok = ok || g.matches(common.Value(f.Name), v)
		}

		if !ok {
			return false
		}
	}

	return true
}

func (g *group) matches(filter, value string) bool {
	switch filter {
	case filterResourceType:
		if g.Query == nil {
			return false
		}

		var tq tagQuery
		if common.Value(g.Query.Type) == queryStack {
			var sq stackQuery
			if decodeQuery(common.Value(g.Query.Query), &sq) != nil {
				return false
			}

			tq.ResourceTypeFilters = sq.ResourceTypeFilters
		} else if decodeQuery(common.Value(g.Query.Query), &tq) != nil {
			return false
		}

		return slices.Contains(tq.ResourceTypeFilters, value) || slices.Contains(tq.ResourceTypeFilters, allSupported)
	case "configuration-type":
		return slices.ContainsFunc(g.Configuration, func(c api.GroupConfigurationItem) bool {
			return common.Value(c.Type) == value
		})
	case "owner":
		return g.Owner == value
	case "display-name":
		return g.DisplayName == value
	case "criticality":
		return g.Criticality != nil && strconv.FormatInt(*g.Criticality, 10) == value
	}

	return false
}

func checkGroupFilters(filters []api.GroupFilter) error {
	for _, f := range filters {
		switch common.Value(f.Name) {
		case filterResourceType, "configuration-type", "owner", "display-name", "criticality":
		default:
			return bad("%q is not a group filter name.", common.Value(f.Name))
		}

		if len(f.Values) < 1 || len(f.Values) > maxFilters {
			return bad("A filter needs 1 to %d values.", maxFilters)
		}
	}

	return nil
}

// pageLimit validates a request's MaxResults.
func pageLimit(v *int64) (int, error) {
	if v == nil {
		return maxPage, nil
	}

	if *v < 1 || *v > maxPage {
		return 0, bad("MaxResults must be between 1 and %d.", maxPage)
	}

	return int(*v), nil
}

func (s *Service) ListGroups(ctx context.Context, in *api.ListGroupsInput) (*api.ListGroupsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if filterErr := checkGroupFilters(in.Filters); filterErr != nil {
		return nil, filterErr
	}

	var groups []*group
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		groups, readErr = storage.Records[group](tx, m.Prefix(service, kindGroup))

		return readErr
	})
	if err != nil {
		return nil, err
	}

	groups = slices.DeleteFunc(groups, func(g *group) bool { return !g.matchesFilters(in.Filters) })

	page, next, err := common.Page(groups, func(g *group) string { return g.Name }, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	out := &api.ListGroupsOutput{NextToken: next}
	for _, g := range page {
		out.Groups = append(out.Groups, *g.output(m.Scope))
		out.GroupIdentifiers = append(out.GroupIdentifiers, api.GroupIdentifier{
			GroupName: new(g.Name), GroupArn: new(groupARN(m.Scope, g.Name)), Description: textOrNil(g.Description),
			Criticality: g.Criticality, Owner: textOrNil(g.Owner), DisplayName: textOrNil(g.DisplayName),
		})
	}

	return out, nil
}

func (s *Service) GetGroupQuery(ctx context.Context, in *api.GetGroupQueryInput) (*api.GetGroupQueryOutput, error) {
	_, g, err := s.read(ctx, in.Group, in.GroupName)
	if err != nil {
		return nil, err
	}

	if g.Query == nil {
		return nil, bad("The group %s has no resource query.", g.Name)
	}

	return &api.GetGroupQueryOutput{GroupQuery: &api.GroupQuery{GroupName: new(g.Name), ResourceQuery: g.Query}}, nil
}

func (s *Service) UpdateGroupQuery(
	ctx context.Context, in *api.UpdateGroupQueryInput,
) (*api.UpdateGroupQueryOutput, error) {
	if in.ResourceQuery == nil {
		return nil, bad("ResourceQuery is required.")
	}

	if err := checkQuery(in.ResourceQuery); err != nil {
		return nil, err
	}

	_, g, err := s.change(ctx, in.Group, in.GroupName, func(g *group) error {
		g.Query = common.Clone(in.ResourceQuery)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateGroupQueryOutput{
		GroupQuery: &api.GroupQuery{GroupName: new(g.Name), ResourceQuery: g.Query},
	}, nil
}

func (s *Service) GetGroupConfiguration(
	ctx context.Context, in *api.GetGroupConfigurationInput,
) (*api.GetGroupConfigurationOutput, error) {
	_, g, err := s.read(ctx, in.Group, nil)
	if err != nil {
		return nil, err
	}

	out := &api.GetGroupConfigurationOutput{GroupConfiguration: g.configuration()}
	if out.GroupConfiguration == nil {
		out.GroupConfiguration = &api.GroupConfiguration{}
	}

	return out, nil
}

func (s *Service) PutGroupConfiguration(
	ctx context.Context, in *api.PutGroupConfigurationInput,
) (*api.PutGroupConfigurationOutput, error) {
	if err := checkConfiguration(in.Configuration); err != nil {
		return nil, err
	}

	_, _, err := s.change(ctx, in.Group, nil, func(g *group) error {
		if len(in.Configuration) == 0 && g.Query == nil {
			return bad("A group needs a resource query or a configuration.")
		}

		g.Configuration = in.Configuration

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutGroupConfigurationOutput{}, nil
}

func (s *Service) ListGroupResources(
	ctx context.Context, in *api.ListGroupResourcesInput,
) (*api.ListGroupResourcesOutput, error) {
	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var types []string
	for _, f := range in.Filters {
		if common.Value(f.Name) != filterResourceType {
			return nil, bad("%q is not a resource filter name.", common.Value(f.Name))
		}

		if len(f.Values) < 1 || len(f.Values) > maxFilters {
			return nil, bad("A filter needs 1 to %d values.", maxFilters)
		}

		for _, v := range f.Values {
			if !typePattern.MatchString(v) {
				return nil, bad("%q is not a valid resource type.", v)
			}
		}

		types = append(types, f.Values...)
	}

	_, g, err := s.read(ctx, in.Group, in.GroupName)
	if err != nil {
		return nil, err
	}

	// A group made from a configuration holds the resources GroupResources
	// adds to it, which TinyStack does not support: it has none.
	var found []member
	var queryErrors []api.QueryError
	if g.Query != nil {
		if found, queryErrors, err = s.resolve(ctx, g.Query, types); err != nil {
			return nil, err
		}
	}

	page, next, err := common.Page(found, func(m member) string { return m.arn }, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	out := &api.ListGroupResourcesOutput{NextToken: next, QueryErrors: queryErrors}
	for _, m := range page {
		id := api.ResourceIdentifier{ResourceArn: new(m.arn), ResourceType: new(m.resourceType)}
		out.ResourceIdentifiers = append(out.ResourceIdentifiers, id)
		out.Resources = append(out.Resources, api.ListGroupResourcesItem{Identifier: &id})
	}

	return out, nil
}

func (s *Service) ListGroupingStatuses(
	ctx context.Context, in *api.ListGroupingStatusesInput,
) (*api.ListGroupingStatusesOutput, error) {
	if _, err := pageLimit(in.MaxResults); err != nil {
		return nil, err
	}

	for _, f := range in.Filters {
		if name := common.Value(f.Name); name != "status" && name != "resource-arn" {
			return nil, bad("%q is not a grouping status filter name.", name)
		}

		if len(f.Values) < 1 || len(f.Values) > 10 {
			return nil, bad("A filter needs 1 to 10 values.")
		}
	}

	_, g, err := s.read(ctx, in.Group, nil)
	if err != nil {
		return nil, err
	}

	// GroupResources and UngroupResources are not supported, so no grouping
	// has run.
	return &api.ListGroupingStatusesOutput{Group: new(g.Name)}, nil
}

func (s *Service) SearchResources(
	ctx context.Context, in *api.SearchResourcesInput,
) (*api.SearchResourcesOutput, error) {
	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if in.ResourceQuery == nil {
		return nil, bad("ResourceQuery is required.")
	}

	if queryErr := checkQuery(in.ResourceQuery); queryErr != nil {
		return nil, queryErr
	}

	found, queryErrors, err := s.resolve(ctx, in.ResourceQuery, nil)
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(found, func(m member) string { return m.arn }, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	out := &api.SearchResourcesOutput{NextToken: next, QueryErrors: queryErrors}
	for _, m := range page {
		out.ResourceIdentifiers = append(out.ResourceIdentifiers,
			api.ResourceIdentifier{ResourceArn: new(m.arn), ResourceType: new(m.resourceType)})
	}

	return out, nil
}
