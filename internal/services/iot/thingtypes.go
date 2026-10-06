package iot

import (
	"context"
	"strings"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxSearchable  = 3
	maxDescription = 2028
)

// thingType is a thing type's record.
type thingType struct {
	Tags       map[string]string
	Properties api.ThingTypeProperties
	Name       string
	ID         string
	Deprecated bool
	Deprecate  float64 `json:",omitempty"`
	Created    float64
}

func loadThingType(tx storage.ReadTx, scope request.Scope, name string) (*thingType, error) {
	t, ok, err := storage.Read[thingType](tx, scope.Key(service, kindThingType, name))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Thing type %s cannot be found.", name))
	}

	return t, nil
}

func (t *thingType) metadata() *api.ThingTypeMetadata {
	m := &api.ThingTypeMetadata{Deprecated: new(t.Deprecated), CreationDate: new(t.Created)}
	if t.Deprecated {
		m.DeprecationDate = new(t.Deprecate)
	}

	return m
}

func checkThingTypeProperties(p *api.ThingTypeProperties) error {
	if p == nil {
		return nil
	}

	if len(common.Value(p.ThingTypeDescription)) > maxDescription {
		return invalid("A thing type description can have at most %d characters.", maxDescription)
	}

	if len(p.SearchableAttributes) > maxSearchable {
		return invalid("A thing type can have at most %d searchable attributes.", maxSearchable)
	}

	for _, a := range p.SearchableAttributes {
		if a == "" || len(a) > 128 || !attributeName.MatchString(a) {
			return invalid("The searchable attribute %q is invalid.", a)
		}
	}

	return nil
}

func (s *Service) CreateThingType(
	ctx context.Context,
	in *api.CreateThingTypeRequest,
) (*api.CreateThingTypeResponse, error) {
	if err := checkName("thing type", in.ThingTypeName); err != nil {
		return nil, err
	}

	if err := checkThingTypeProperties(in.ThingTypeProperties); err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	t := &thingType{
		Name: *in.ThingTypeName, ID: uuid.NewV4().String(), Tags: tags, Created: common.Unix(s.now()),
		Properties: common.Value(in.ThingTypeProperties),
	}
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		key := scope.Key(service, kindThingType, t.Name)
		if exists(tx, key) {
			return alreadyExists("Thing type %s already exists.", t.Name)
		}

		return storage.Write(tx, key, t)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateThingTypeResponse{
		ThingTypeName: new(t.Name), ThingTypeArn: new(arn(scope, "thingtype", t.Name)), ThingTypeId: new(t.ID),
	}, nil
}

func (s *Service) DescribeThingType(
	ctx context.Context,
	in *api.DescribeThingTypeRequest,
) (*api.DescribeThingTypeResponse, error) {
	var out *api.DescribeThingTypeResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		t, err := loadThingType(tx, scope, common.Value(in.ThingTypeName))
		if err != nil {
			return err
		}

		out = &api.DescribeThingTypeResponse{
			ThingTypeName: new(t.Name), ThingTypeId: new(t.ID), ThingTypeArn: new(arn(scope, "thingtype", t.Name)),
			ThingTypeProperties: &t.Properties, ThingTypeMetadata: t.metadata(),
		}
		return nil
	})

	return out, err
}

func (s *Service) ListThingTypes(
	ctx context.Context,
	in *api.ListThingTypesRequest,
) (*api.ListThingTypesResponse, error) {
	var types []api.ThingTypeDefinition
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		records, err := storage.Records[thingType](tx, scope.Prefix(service, kindThingType))
		if err != nil {
			return err
		}

		for _, t := range records {
			// thingTypeName filters by name prefix.
			if !strings.HasPrefix(t.Name, common.Value(in.ThingTypeName)) {
				continue
			}

			types = append(types, api.ThingTypeDefinition{
				ThingTypeName: new(t.Name), ThingTypeArn: new(arn(scope, "thingtype", t.Name)),
				ThingTypeProperties: &t.Properties, ThingTypeMetadata: t.metadata(),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(types, func(t api.ThingTypeDefinition) string { return *t.ThingTypeName },
		in.NextToken, in.MaxResults)
	if err != nil {
		return nil, err
	}

	return &api.ListThingTypesResponse{ThingTypes: selected, NextToken: next}, nil
}

func (s *Service) DeprecateThingType(
	ctx context.Context,
	in *api.DeprecateThingTypeRequest,
) (*api.DeprecateThingTypeResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadThingType(tx, scope, common.Value(in.ThingTypeName))
		if err != nil {
			return err
		}

		t.Deprecated, t.Deprecate = !common.Value(in.UndoDeprecate), 0
		if t.Deprecated {
			t.Deprecate = common.Unix(s.now())
		}

		return storage.Write(tx, scope.Key(service, kindThingType, t.Name), t)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeprecateThingTypeResponse{}, nil
}

// DeleteThingType deletes a deprecated thing type that no thing uses. AWS
// also waits five minutes after the deprecation; TinyStack does not.
func (s *Service) DeleteThingType(
	ctx context.Context,
	in *api.DeleteThingTypeRequest,
) (*api.DeleteThingTypeResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		t, err := loadThingType(tx, scope, common.Value(in.ThingTypeName))
		if err != nil {
			return err
		}

		if !t.Deprecated {
			return invalid("Thing type %s must be deprecated before it is deleted.", t.Name)
		}

		things, err := storage.Records[thing](tx, scope.Prefix(service, kindThing))
		if err != nil {
			return err
		}

		for _, th := range things {
			if th.Type == t.Name {
				return invalid("Thing type %s cannot be deleted while thing %s uses it.", t.Name, th.Name)
			}
		}

		tx.Delete(scope.Key(service, kindThingType, t.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteThingTypeResponse{}, nil
}
