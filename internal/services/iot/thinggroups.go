package iot

import (
	"context"
	"maps"
	"slices"
	"strings"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxGroupDepth is how deep a thing group hierarchy may be.
const maxGroupDepth = 7

// thingGroup is a static thing group's record.
type thingGroup struct {
	Tags        map[string]string
	Attributes  map[string]string
	Name        string
	ID          string
	Parent      string `json:",omitempty"`
	Description string `json:",omitempty"`
	Version     int64
	Created     float64
}

func loadThingGroup(tx storage.ReadTx, scope request.Scope, name string) (*thingGroup, error) {
	g, ok, err := storage.Read[thingGroup](tx, scope.Key(service, kindThingGroup, name))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Thing group %s cannot be found.", name))
	}

	return g, nil
}

// ancestors returns a group's ancestors, from the root down to its parent.
func ancestors(tx storage.ReadTx, scope request.Scope, g *thingGroup) ([]*thingGroup, error) {
	var out []*thingGroup
	for parent := g.Parent; parent != ""; {
		p, err := loadThingGroup(tx, scope, parent)
		if err != nil {
			return nil, err
		}

		out = append(out, p)
		parent = p.Parent
	}

	slices.Reverse(out)
	return out, nil
}

func (g *thingGroup) properties() *api.ThingGroupProperties {
	p := &api.ThingGroupProperties{AttributePayload: &api.AttributePayload{Attributes: g.Attributes}}
	if g.Description != "" {
		p.ThingGroupDescription = new(g.Description)
	}

	return p
}

func (s *Service) CreateThingGroup(
	ctx context.Context,
	in *api.CreateThingGroupRequest,
) (*api.CreateThingGroupResponse, error) {
	if err := checkName("thing group", in.ThingGroupName); err != nil {
		return nil, err
	}

	g := &thingGroup{
		Name: *in.ThingGroupName, ID: uuid.NewV4().String(), Parent: common.Value(in.ParentGroupName), Version: 1,
		Created: common.Unix(s.now()), Attributes: map[string]string{},
	}
	if err := g.apply(in.ThingGroupProperties); err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	g.Tags = tags

	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		key := scope.Key(service, kindThingGroup, g.Name)
		if existing, found, readErr := storage.Read[thingGroup](tx, key); readErr != nil || found {
			if readErr == nil && existing.Parent == g.Parent && existing.Description == g.Description {
				// Creating a group again with the same settings succeeds.
				g = existing
				return nil
			}

			return common.ErrorsOr(readErr, alreadyExists("Thing group %s already exists.", g.Name))
		}

		if g.Parent != "" {
			if parentErr := checkParent(tx, scope, g.Parent); parentErr != nil {
				return parentErr
			}
		}

		return storage.Write(tx, key, g)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateThingGroupResponse{
		ThingGroupName: new(g.Name), ThingGroupArn: new(arn(scope, "thinggroup", g.Name)), ThingGroupId: new(g.ID),
	}, nil
}

// checkParent checks that a new group can be a child of the named group.
func checkParent(tx storage.ReadTx, scope request.Scope, name string) error {
	parent, err := loadThingGroup(tx, scope, name)
	if err != nil {
		return err
	}

	above, err := ancestors(tx, scope, parent)
	if err != nil {
		return err
	}

	if len(above)+2 > maxGroupDepth {
		return invalid("A thing group hierarchy can be at most %d levels deep.", maxGroupDepth)
	}

	return nil
}

// apply sets a group's description and attributes from properties.
func (g *thingGroup) apply(p *api.ThingGroupProperties) error {
	if p == nil {
		return nil
	}

	if p.ThingGroupDescription != nil {
		if len(*p.ThingGroupDescription) > maxDescription {
			return invalid("A thing group description can have at most %d characters.", maxDescription)
		}

		g.Description = *p.ThingGroupDescription
	}

	attrs, err := applyAttributes(g.Attributes, p.AttributePayload)
	if err != nil {
		return err
	}

	g.Attributes = attrs
	return nil
}

func (s *Service) DescribeThingGroup(
	ctx context.Context,
	in *api.DescribeThingGroupRequest,
) (*api.DescribeThingGroupResponse, error) {
	var out *api.DescribeThingGroupResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		g, err := loadThingGroup(tx, scope, common.Value(in.ThingGroupName))
		if err != nil {
			return err
		}

		above, err := ancestors(tx, scope, g)
		if err != nil {
			return err
		}

		meta := &api.ThingGroupMetadata{CreationDate: new(g.Created)}
		if g.Parent != "" {
			meta.ParentGroupName = new(g.Parent)
			for _, a := range above {
				meta.RootToParentThingGroups = append(meta.RootToParentThingGroups,
					api.GroupNameAndArn{GroupName: new(a.Name), GroupArn: new(arn(scope, "thinggroup", a.Name))})
			}
		}

		out = &api.DescribeThingGroupResponse{
			ThingGroupName: new(g.Name), ThingGroupId: new(g.ID), ThingGroupArn: new(arn(scope, "thinggroup", g.Name)),
			Version: new(g.Version), ThingGroupProperties: g.properties(), ThingGroupMetadata: meta,
		}
		return nil
	})

	return out, err
}

func (s *Service) UpdateThingGroup(
	ctx context.Context,
	in *api.UpdateThingGroupRequest,
) (*api.UpdateThingGroupResponse, error) {
	if in.ThingGroupProperties == nil {
		return nil, invalid("thingGroupProperties is required.")
	}

	var version int64
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		g, err := loadThingGroup(tx, scope, common.Value(in.ThingGroupName))
		if err != nil {
			return err
		}

		if in.ExpectedVersion != nil && *in.ExpectedVersion != g.Version {
			return versionConflict("thing group " + g.Name)
		}

		if err := g.apply(in.ThingGroupProperties); err != nil {
			return err
		}

		g.Version++
		version = g.Version
		return storage.Write(tx, scope.Key(service, kindThingGroup, g.Name), g)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateThingGroupResponse{Version: new(version)}, nil
}

// DeleteThingGroup deletes a group without child groups, removing its things
// from it and detaching its policies.
func (s *Service) DeleteThingGroup(
	ctx context.Context,
	in *api.DeleteThingGroupRequest,
) (*api.DeleteThingGroupResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := scope.Key(service, kindThingGroup, common.Value(in.ThingGroupName))
		g, found, err := storage.Read[thingGroup](tx, key)
		if err != nil || !found {
			// Deleting a group that does not exist succeeds.
			return err
		}

		if in.ExpectedVersion != nil && *in.ExpectedVersion != g.Version {
			return versionConflict("thing group " + g.Name)
		}

		groups, err := storage.Records[thingGroup](tx, scope.Prefix(service, kindThingGroup))
		if err != nil {
			return err
		}

		for _, child := range groups {
			if child.Parent == g.Name {
				return invalid("Thing group %s cannot be deleted while it has child group %s.", g.Name, child.Name)
			}
		}

		things, err := storage.Records[thing](tx, scope.Prefix(service, kindThing))
		if err != nil {
			return err
		}

		for _, t := range things {
			if slices.Contains(t.Groups, g.Name) {
				t.Groups = detach(t.Groups, g.Name)
				if err := storage.Write(tx, scope.Key(service, kindThing, t.Name), t); err != nil {
					return err
				}
			}
		}

		if err := detachAll(tx, scope, arn(scope, "thinggroup", g.Name)); err != nil {
			return err
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteThingGroupResponse{}, nil
}

func (s *Service) ListThingGroups(
	ctx context.Context,
	in *api.ListThingGroupsRequest,
) (*api.ListThingGroupsResponse, error) {
	var out []api.GroupNameAndArn
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		parents, err := groupParents(tx, scope)
		if err != nil {
			return err
		}

		if in.ParentGroup != nil {
			if _, ok := parents[*in.ParentGroup]; !ok {
				return notFound("Thing group %s cannot be found.", *in.ParentGroup)
			}
		}

		// recursive defaults to true; it only matters with parentGroup.
		recursive := in.Recursive == nil || *in.Recursive
		for _, name := range slices.Sorted(maps.Keys(parents)) {
			if !strings.HasPrefix(name, common.Value(in.NamePrefixFilter)) {
				continue
			}

			if in.ParentGroup != nil && !under(parents, name, *in.ParentGroup, recursive) {
				continue
			}

			out = append(out, api.GroupNameAndArn{GroupName: new(name), GroupArn: new(arn(scope, "thinggroup", name))})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(out, func(g api.GroupNameAndArn) string { return *g.GroupName }, in.NextToken,
		in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListThingGroupsResponse{ThingGroups: selected, NextToken: next}, nil
}

// groupParents maps every thing group to its parent, "" for a root.
func groupParents(tx storage.ReadTx, scope request.Scope) (map[string]string, error) {
	groups, err := storage.Records[thingGroup](tx, scope.Prefix(service, kindThingGroup))
	if err != nil {
		return nil, err
	}

	parents := make(map[string]string, len(groups))
	for _, g := range groups {
		parents[g.Name] = g.Parent
	}

	return parents, nil
}

// under reports whether group is a child of parent, or, when recursive, any
// descendant.
func under(parents map[string]string, group, parent string, recursive bool) bool {
	for p := parents[group]; p != ""; p = parents[p] {
		if p == parent {
			return true
		}

		if !recursive {
			return false
		}
	}

	return false
}

// groupAndThing resolves the group and thing an add or remove names, by name
// or ARN.
func groupAndThing(scope request.Scope, groupName, groupARN, thingName, thingARN *string) (group, name string,
	err error,
) {
	group, err = nameOrARN(scope, "thinggroup", "thing group", groupName, groupARN)
	if err != nil {
		return "", "", err
	}

	name, err = nameOrARN(scope, kindThing, "thing", thingName, thingARN)
	return group, name, err
}

// nameOrARN returns the name a request gives directly or in an ARN of the
// caller's account and region.
func nameOrARN(scope request.Scope, arnKind, what string, name, resource *string) (string, error) {
	switch {
	case name != nil:
		return *name, nil
	case resource != nil:
		n, ok := strings.CutPrefix(*resource, arn(scope, arnKind, ""))
		if !ok || n == "" {
			return "", invalid("%s is not a %s ARN of this account and Region.", *resource, what)
		}

		return n, nil
	}

	return "", invalid("The %s name or ARN is required.", what)
}

func (s *Service) AddThingToThingGroup(
	ctx context.Context,
	in *api.AddThingToThingGroupRequest,
) (*api.AddThingToThingGroupResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		group, name, err := groupAndThing(scope, in.ThingGroupName, in.ThingGroupArn, in.ThingName, in.ThingArn)
		if err != nil {
			return err
		}

		if _, err = loadThingGroup(tx, scope, group); err != nil {
			return err
		}

		t, err := loadThing(tx, scope, name)
		if err != nil {
			return err
		}

		if !slices.Contains(t.Groups, group) && len(t.Groups) >= maxThingGroups {
			return invalid("Thing %s is already in %d thing groups.", name, maxThingGroups)
		}

		t.Groups = attach(t.Groups, group)
		return storage.Write(tx, scope.Key(service, kindThing, name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.AddThingToThingGroupResponse{}, nil
}

func (s *Service) RemoveThingFromThingGroup(
	ctx context.Context,
	in *api.RemoveThingFromThingGroupRequest,
) (*api.RemoveThingFromThingGroupResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		group, name, err := groupAndThing(scope, in.ThingGroupName, in.ThingGroupArn, in.ThingName, in.ThingArn)
		if err != nil {
			return err
		}

		if _, err = loadThingGroup(tx, scope, group); err != nil {
			return err
		}

		t, err := loadThing(tx, scope, name)
		if err != nil {
			return err
		}

		t.Groups = detach(t.Groups, group)
		return storage.Write(tx, scope.Key(service, kindThing, name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveThingFromThingGroupResponse{}, nil
}

func (s *Service) ListThingsInThingGroup(
	ctx context.Context,
	in *api.ListThingsInThingGroupRequest,
) (*api.ListThingsInThingGroupResponse, error) {
	var names []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		name := common.Value(in.ThingGroupName)
		parents, err := groupParents(tx, scope)
		if err != nil {
			return err
		}

		if _, ok := parents[name]; !ok {
			return notFound("Thing group %s cannot be found.", name)
		}

		members := map[string]bool{name: true}
		for sub := range parents {
			if common.Value(in.Recursive) && under(parents, sub, name, true) {
				members[sub] = true
			}
		}

		things, err := storage.Records[thing](tx, scope.Prefix(service, kindThing))
		if err != nil {
			return err
		}

		for _, t := range things {
			if slices.ContainsFunc(t.Groups, func(group string) bool { return members[group] }) {
				names = append(names, t.Name)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(names, func(n string) string { return n }, in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListThingsInThingGroupResponse{Things: selected, NextToken: next}, nil
}

func (s *Service) ListThingGroupsForThing(
	ctx context.Context,
	in *api.ListThingGroupsForThingRequest,
) (*api.ListThingGroupsForThingResponse, error) {
	var out []api.GroupNameAndArn
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadThing(tx, scope, common.Value(in.ThingName))
		if err != nil {
			return err
		}

		for _, g := range t.Groups {
			out = append(out, api.GroupNameAndArn{GroupName: new(g), GroupArn: new(arn(scope, "thinggroup", g))})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(out, func(g api.GroupNameAndArn) string { return *g.GroupName }, in.NextToken,
		in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListThingGroupsForThingResponse{ThingGroups: selected, NextToken: next}, nil
}
