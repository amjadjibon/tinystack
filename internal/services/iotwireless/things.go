package iotwireless

import (
	"context"
	"strings"

	iotapi "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const idThingName = "ThingName"

// thingName returns the name of the IoT thing a ThingArn names, which must exist.
func (s *Service) thingName(ctx context.Context, scope request.Scope, thingARN string) (string, error) {
	name, ok := strings.CutPrefix(thingARN, "arn:aws:iot:"+scope.Region+":"+scope.AccountID+":thing/")
	if !ok || name == "" {
		return "", invalid("The ThingArn %s is not the ARN of an IoT thing in this account and Region", thingARN)
	}

	if _, err := s.things.DescribeThing(ctx, &iotapi.DescribeThingRequest{ThingName: new(name)}); err != nil {
		return "", err
	}

	return name, nil
}

// thingOwner returns the kind and ID of the device or gateway associated with a thing.
func thingOwner(tx storage.ReadTx, scope request.Scope, name string) (kind, id string, found bool) {
	owner, found := indexOf(tx, scope, kindThing, name)
	kind, id, _ = strings.Cut(owner, "/")
	return kind, id, found
}

// thingRefs points at the thing members of a device or gateway.
func thingRefs(r *record) (name, thingARN **string) {
	if r.Device != nil {
		return &r.Device.ThingName, &r.Device.ThingArn
	}

	return &r.Gateway.ThingName, &r.Gateway.ThingArn
}

// associateThing associates a device or gateway with the thing a ThingArn names. A thing can have one owner.
func (s *Service) associateThing(ctx context.Context, kind, id, thingARN string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	name, err := s.thingName(ctx, scope, thingARN)
	if err != nil {
		return err
	}

	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kind, id)
		if err != nil {
			return err
		}

		current, linked := thingRefs(r)
		if *current != nil && **current != name {
			return conflict("The %s %s is already associated with the thing %s", kind, id, **current)
		}

		if checkErr := claim(tx, scope, kindThing, name, kind+"/"+id); checkErr != nil {
			return checkErr
		}

		*current, *linked = new(name), new(thingARN)
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})
}

// disassociateThing removes the association of a device or gateway with its thing.
func (s *Service) disassociateThing(ctx context.Context, kind, id string) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kind, id)
		if err != nil {
			return err
		}

		current, linked := thingRefs(r)
		if *current == nil {
			return missing("Thing association of the "+kind, id)
		}

		tx.Delete(scope.Key(serviceName, kindThing, **current))
		*current, *linked = nil, nil
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})
}
