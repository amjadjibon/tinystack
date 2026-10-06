package iot

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxAttributes   = 50
	maxThingGroups  = 10
	exclusiveThing  = "EXCLUSIVE_THING"
	nonExclusive    = "NON_EXCLUSIVE_THING"
	attributeLength = 800
)

var (
	attributeName  = regexp.MustCompile(`^[a-zA-Z0-9_.,@/:#-]+$`)
	attributeValue = regexp.MustCompile(`^[a-zA-Z0-9_.,@/:#=\[\]-]*$`)
)

// thing is a thing's record. Groups are the static thing groups it is in,
// and Principals the certificates and identities attached to it.
type thing struct {
	Attributes map[string]string
	Name       string
	ID         string
	Type       string `json:",omitempty"`
	Groups     []string
	Principals []string
	Version    int64
	Created    float64
}

func loadThing(tx storage.ReadTx, scope request.Scope, name string) (*thing, error) {
	t, ok, err := storage.Read[thing](tx, scope.Key(service, kindThing, name))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Thing %s cannot be found.", name))
	}

	return t, nil
}

// checkAttributes checks attribute names and values.
func checkAttributes(attrs map[string]string) error {
	if len(attrs) > maxAttributes {
		return invalid("A thing or group can have at most %d attributes.", maxAttributes)
	}

	for k, v := range attrs {
		if k == "" || len(k) > 128 || !attributeName.MatchString(k) {
			return invalid("The attribute name %q is invalid.", k)
		}

		if len(v) > attributeLength || !attributeValue.MatchString(v) {
			return invalid("The value of attribute %s is invalid.", k)
		}
	}

	return nil
}

// applyAttributes changes attrs as a payload asks: with merge, an empty value
// removes its attribute and the others are set; without, the payload's
// attributes replace them all.
func applyAttributes(attrs map[string]string, p *api.AttributePayload) (map[string]string, error) {
	if p == nil {
		return attrs, nil
	}

	if !common.Value(p.Merge) {
		out := map[string]string{}
		for k, v := range p.Attributes {
			if v != "" {
				out[k] = v
			}
		}

		return out, checkAttributes(out)
	}

	out := maps.Clone(attrs)
	if out == nil {
		out = map[string]string{}
	}

	for k, v := range p.Attributes {
		if v == "" {
			delete(out, k)
			continue
		}

		out[k] = v
	}

	return out, checkAttributes(out)
}

// useType checks that a thing can take the named type.
func useType(tx storage.ReadTx, scope request.Scope, name string) error {
	t, err := loadThingType(tx, scope, name)
	if err != nil {
		return err
	}

	if t.Deprecated {
		return invalid("Thing type %s is deprecated, so new things cannot use it.", name)
	}

	return nil
}

func (s *Service) CreateThing(ctx context.Context, in *api.CreateThingRequest) (*api.CreateThingResponse, error) {
	if err := checkName("thing", in.ThingName); err != nil {
		return nil, err
	}

	var payload *api.AttributePayload
	if in.AttributePayload != nil {
		// A new thing's attributes are the payload's, merge or not.
		payload = &api.AttributePayload{Attributes: in.AttributePayload.Attributes}
	}

	attrs, err := applyAttributes(nil, payload)
	if err != nil {
		return nil, err
	}

	t := &thing{
		Name: *in.ThingName, ID: uuid.NewV4().String(), Type: common.Value(in.ThingTypeName), Attributes: attrs,
		Version: 1, Created: common.Unix(s.now()), Groups: []string{}, Principals: []string{},
	}
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		key := scope.Key(service, kindThing, t.Name)
		if existing, found, readErr := storage.Read[thing](tx, key); readErr != nil || found {
			if readErr == nil && sameThing(existing, t) {
				// Creating a thing again with the same settings succeeds.
				t = existing
				return nil
			}

			return common.ErrorsOr(readErr, alreadyExists("Thing %s already exists with a different configuration.",
				t.Name))
		}

		if t.Type != "" {
			if typeErr := useType(tx, scope, t.Type); typeErr != nil {
				return typeErr
			}
		}

		return storage.Write(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateThingResponse{
		ThingName: new(t.Name), ThingArn: new(arn(scope, kindThing, t.Name)), ThingId: new(t.ID),
	}, nil
}

func sameThing(a, b *thing) bool {
	return a.Type == b.Type && maps.Equal(a.Attributes, b.Attributes)
}

func (s *Service) DescribeThing(ctx context.Context, in *api.DescribeThingRequest) (*api.DescribeThingResponse, error) {
	var out *api.DescribeThingResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadThing(tx, scope, common.Value(in.ThingName))
		if err != nil {
			return err
		}

		out = &api.DescribeThingResponse{
			DefaultClientId: new(t.Name), ThingName: new(t.Name), ThingId: new(t.ID),
			ThingArn: new(arn(scope, kindThing, t.Name)), Attributes: t.Attributes, Version: new(t.Version),
		}
		if t.Type != "" {
			out.ThingTypeName = new(t.Type)
		}

		return nil
	})

	return out, err
}

func (s *Service) UpdateThing(ctx context.Context, in *api.UpdateThingRequest) (*api.UpdateThingResponse, error) {
	if in.ThingTypeName != nil && common.Value(in.RemoveThingType) {
		return nil, invalid("Set thingTypeName or removeThingType, not both.")
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadThing(tx, scope, common.Value(in.ThingName))
		if err != nil {
			return err
		}

		if in.ExpectedVersion != nil && *in.ExpectedVersion != t.Version {
			return versionConflict("thing " + t.Name)
		}

		if t.Attributes, err = applyAttributes(t.Attributes, in.AttributePayload); err != nil {
			return err
		}

		switch {
		case common.Value(in.RemoveThingType):
			t.Type = ""
		case in.ThingTypeName != nil && *in.ThingTypeName != t.Type:
			if err := useType(tx, scope, *in.ThingTypeName); err != nil {
				return err
			}

			t.Type = *in.ThingTypeName
		}

		t.Version++
		return storage.Write(tx, scope.Key(service, kindThing, t.Name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateThingResponse{}, nil
}

func (s *Service) DeleteThing(ctx context.Context, in *api.DeleteThingRequest) (*api.DeleteThingResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, found, err := storage.Read[thing](tx, scope.Key(service, kindThing, common.Value(in.ThingName)))
		if err != nil || !found {
			// Deleting a thing that does not exist succeeds.
			return err
		}

		if in.ExpectedVersion != nil && *in.ExpectedVersion != t.Version {
			return versionConflict("thing " + t.Name)
		}

		if len(t.Principals) > 0 {
			return invalid("Thing %s cannot be deleted while principals are attached to it.", t.Name)
		}

		tx.Delete(scope.Key(service, kindThing, t.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteThingResponse{}, nil
}

func (s *Service) ListThings(ctx context.Context, in *api.ListThingsRequest) (*api.ListThingsResponse, error) {
	if in.AttributeValue != nil && in.AttributeName == nil {
		return nil, invalid("attributeValue needs attributeName.")
	}

	var things []api.ThingAttribute
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		records, err := storage.Records[thing](tx, scope.Prefix(service, kindThing))
		if err != nil {
			return err
		}

		for _, t := range records {
			if !thingMatches(t, in) {
				continue
			}

			a := api.ThingAttribute{
				ThingName: new(t.Name), ThingArn: new(arn(scope, kindThing, t.Name)), Attributes: t.Attributes,
				Version: new(t.Version),
			}
			if t.Type != "" {
				a.ThingTypeName = new(t.Type)
			}

			things = append(things, a)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(things, func(t api.ThingAttribute) string { return *t.ThingName }, in.NextToken,
		in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListThingsResponse{Things: selected, NextToken: next}, nil
}

// thingMatches applies ListThings' filters.
func thingMatches(t *thing, in *api.ListThingsRequest) bool {
	if in.ThingTypeName != nil && t.Type != *in.ThingTypeName {
		return false
	}

	if in.AttributeName == nil {
		return true
	}

	v, ok := t.Attributes[*in.AttributeName]
	switch {
	case !ok:
		return false
	case in.AttributeValue == nil:
		return true
	case common.Value(in.UsePrefixAttributeValue):
		return strings.HasPrefix(v, *in.AttributeValue)
	default:
		return v == *in.AttributeValue
	}
}

// attach adds name to list, keeping it sorted and without duplicates.
func attach(list []string, name string) []string {
	if i, found := slices.BinarySearch(list, name); !found {
		return slices.Insert(list, i, name)
	}

	return list
}

// detach removes name from list.
func detach(list []string, name string) []string {
	return slices.DeleteFunc(list, func(s string) bool { return s == name })
}
