package iotwireless

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	iotapi "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iotwireless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/iot"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	expressionRule  = "RuleName"
	expressionTopic = "MqttTopic"
)

// checkExpression fails unless the expression names an IoT topic rule that exists or an MQTT topic.
func (s *Service) checkExpression(ctx context.Context, expressionType, expression string) error {
	if expressionType == expressionRule {
		_, err := s.things.GetTopicRule(ctx, &iotapi.GetTopicRuleRequest{RuleName: new(expression)})
		if err != nil && awserror.From(err).Code == "UnauthorizedException" {
			return missing("IoT topic rule", expression)
		}

		return err
	}

	if !iot.PublishableTopic(expression) {
		return invalid("The expression %q is not an MQTT topic that can be published to", expression)
	}

	return nil
}

// CreateDestination creates a destination for uplinks.
func (s *Service) CreateDestination(
	ctx context.Context,
	in *api.CreateDestinationRequest,
) (*api.CreateDestinationResponse, error) {
	tags, err := tagsOf(in.Tags)
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if err := s.checkExpression(ctx, *in.ExpressionType, *in.Expression); err != nil {
		return nil, err
	}

	if err := s.checkRole(ctx, scope, *in.RoleArn); err != nil {
		return nil, err
	}

	var r *record
	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := idempotency.Key(scope, serviceName, kindDestination, in.ClientRequestToken)
		existing, found, err := idempotency.Replay[*record](
			tx,
			key,
			in,
			conflict(
				"The ClientRequestToken %s was used with different parameters",
				common.Value(in.ClientRequestToken),
			),
		)
		if err != nil || found {
			r = existing
			return err
		}

		if _, found, readErr := storage.Read[record](
			tx,
			scope.Key(serviceName, kindDestination, *in.Name),
		); readErr != nil ||
			found {
			return common.ErrorsOr(readErr, conflict("The destination %s already exists", *in.Name))
		}

		r = newRecord(scope, kindDestination, *in.Name, tags)
		r.Name = *in.Name
		r.Destination = &api.GetDestinationResponse{
			Arn: new(r.Arn), Name: in.Name, Expression: in.Expression, ExpressionType: in.ExpressionType,
			Description: in.Description, RoleArn: in.RoleArn,
		}

		if checkErr := storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r); checkErr != nil {
			return checkErr
		}

		return idempotency.Remember(tx, key, in, r)
	})
	if updateErr != nil {
		return nil, updateErr
	}

	return &api.CreateDestinationResponse{Arn: new(r.Arn), Name: new(r.Name)}, nil
}

// GetDestination returns a destination.
func (s *Service) GetDestination(
	ctx context.Context,
	in *api.GetDestinationRequest,
) (*api.GetDestinationResponse, error) {
	var r *record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		r, err = load(tx, scope, kindDestination, *in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}

	return r.Destination, nil
}

// ListDestinations lists destinations by name.
func (s *Service) ListDestinations(
	ctx context.Context,
	in *api.ListDestinationsRequest,
) (*api.ListDestinationsResponse, error) {
	var all []*record
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[record](tx, scope.Prefix(serviceName, kindDestination))
		return err
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		all,
		func(r *record) string { return r.ID },
		in.NextToken,
		pageSize(in.MaxResults),
		badToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListDestinationsResponse{NextToken: next, DestinationList: []api.Destinations{}}
	for _, r := range page {
		out.DestinationList = append(out.DestinationList, api.Destinations(*r.Destination))
	}

	return out, nil
}

// UpdateDestination changes a destination; a changed expression or role is checked like a new one.
func (s *Service) UpdateDestination(
	ctx context.Context,
	in *api.UpdateDestinationRequest,
) (*api.UpdateDestinationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	current, err := s.GetDestination(ctx, &api.GetDestinationRequest{Name: in.Name})
	if err != nil {
		return nil, err
	}

	expressionType := common.OrElse(in.ExpressionType, current.ExpressionType)
	expression := common.OrElse(in.Expression, current.Expression)
	if in.ExpressionType != nil || in.Expression != nil {
		if err := s.checkExpression(ctx, *expressionType, *expression); err != nil {
			return nil, err
		}
	}

	if in.RoleArn != nil {
		if err := s.checkRole(ctx, scope, *in.RoleArn); err != nil {
			return nil, err
		}
	}

	updateErr := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := load(tx, scope, kindDestination, *in.Name)
		if err != nil {
			return err
		}

		d := r.Destination
		d.ExpressionType = common.OrElse(in.ExpressionType, d.ExpressionType)
		d.Expression = common.OrElse(in.Expression, d.Expression)
		d.RoleArn = common.OrElse(in.RoleArn, d.RoleArn)
		d.Description = common.OrElse(in.Description, d.Description)
		return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
	})

	return &api.UpdateDestinationResponse{}, updateErr
}

// DeleteDestination deletes a destination that no wireless device routes to.
func (s *Service) DeleteDestination(
	ctx context.Context,
	in *api.DeleteDestinationRequest,
) (*api.DeleteDestinationResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := load(tx, scope, kindDestination, *in.Name); err != nil {
			return err
		}

		devices, err := storage.Records[record](tx, scope.Prefix(serviceName, kindDevice))
		if err != nil {
			return err
		}

		for _, d := range devices {
			if usesDestination(d.Device, *in.Name) {
				return conflict("The destination %s is used by the wireless device %s", *in.Name, d.ID)
			}
		}

		tx.Delete(scope.Key(serviceName, kindDestination, *in.Name))
		return nil
	})

	return &api.DeleteDestinationResponse{}, err
}

// usesDestination reports whether a device routes uplinks to the destination, by default or by an application port.
func usesDestination(d *api.GetWirelessDeviceResponse, name string) bool {
	if common.Value(d.DestinationName) == name {
		return true
	}

	if d.LoRaWAN == nil || d.LoRaWAN.FPorts == nil {
		return false
	}

	for _, app := range d.LoRaWAN.FPorts.Applications {
		if common.Value(app.DestinationName) == name {
			return true
		}
	}

	return false
}
