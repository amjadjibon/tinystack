package sesv2

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// eventTypes are the events a destination may match.
var eventTypes = []string{
	eventSend, "REJECT", eventBounce, eventComplaint, eventDelivery, "OPEN", "CLICK", eventRenderingFailure,
	"DELIVERY_DELAY", "SUBSCRIPTION",
}

// configSet is a configuration set's record.
type configSet struct {
	Tags         map[string]string
	Settings     api.GetConfigurationSetResponse
	Destinations []api.EventDestination
}

func loadConfigSet(tx storage.ReadTx, scope request.Scope, name *string) (*configSet, error) {
	c, ok, err := storage.Read[configSet](tx, scope.Key(service, kindConfigSet, common.Value(name)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(
			err,
			notFound(kindConfigSet, "Configuration set %s does not exist.", common.Value(name)),
		)
	}

	return c, nil
}

func (s *Service) CreateConfigurationSet(
	ctx context.Context,
	in *api.CreateConfigurationSetRequest,
) (*api.CreateConfigurationSetResponse, error) {
	if err := checkName("configuration set", in.ConfigurationSetName); err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	c := &configSet{Tags: tags, Destinations: []api.EventDestination{}, Settings: api.GetConfigurationSetResponse{
		ConfigurationSetName: in.ConfigurationSetName, TrackingOptions: in.TrackingOptions,
		DeliveryOptions: in.DeliveryOptions, ReputationOptions: in.ReputationOptions,
		SendingOptions:     common.OrElse(in.SendingOptions, &api.SendingOptions{SendingEnabled: new(true)}),
		SuppressionOptions: in.SuppressionOptions,
	}}
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := scope.Key(service, kindConfigSet, *in.ConfigurationSetName)
		if _, found, readErr := storage.Read[configSet](tx, key); readErr != nil || found {
			return common.ErrorsOr(readErr,
				alreadyExists("Configuration set %s already exists.", *in.ConfigurationSetName))
		}

		return storage.Write(tx, key, c)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateConfigurationSetResponse{}, nil
}

func (s *Service) GetConfigurationSet(
	ctx context.Context,
	in *api.GetConfigurationSetRequest,
) (*api.GetConfigurationSetResponse, error) {
	var c *configSet
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		c, err = loadConfigSet(tx, scope, in.ConfigurationSetName)
		return err
	})
	if err != nil {
		return nil, err
	}

	out := c.Settings
	out.Tags = tagList(c.Tags)
	return &out, nil
}

func (s *Service) ListConfigurationSets(
	ctx context.Context,
	in *api.ListConfigurationSetsRequest,
) (*api.ListConfigurationSetsResponse, error) {
	var names []string
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := storage.Records[configSet](tx, scope.Prefix(service, kindConfigSet))
		for _, c := range all {
			names = append(names, *c.Settings.ConfigurationSetName)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(names, func(n string) string { return n }, in.NextToken, in.PageSize, 100, 1000)
	if err != nil {
		return nil, err
	}

	return &api.ListConfigurationSetsResponse{ConfigurationSets: append([]string{}, selected...), NextToken: next}, nil
}

func (s *Service) DeleteConfigurationSet(
	ctx context.Context,
	in *api.DeleteConfigurationSetRequest,
) (*api.DeleteConfigurationSetResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadConfigSet(tx, scope, in.ConfigurationSetName); err != nil {
			return err
		}

		tx.Delete(scope.Key(service, kindConfigSet, *in.ConfigurationSetName))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteConfigurationSetResponse{}, nil
}

// checkDestination validates an event destination: one SNS topic, event
// bus, or Firehose delivery stream, and the events it receives.
func checkDestination(d *api.EventDestinationDefinition) error {
	if d == nil {
		return badRequest("EventDestination is required.")
	}

	targets := 0
	for _, arn := range []*string{
		common.OrElse(d.SnsDestination, &api.SnsDestination{}).TopicArn,
		common.OrElse(d.EventBridgeDestination, &api.EventBridgeDestination{}).EventBusArn,
		common.OrElse(d.KinesisFirehoseDestination, &api.KinesisFirehoseDestination{}).DeliveryStreamArn,
	} {
		if arn != nil {
			targets++
		}
	}

	if targets != 1 {
		return badRequest("An event destination needs exactly one SNS topic, event bus, or Firehose delivery " +
			"stream. TinyStack does not publish to CloudWatch or Pinpoint.")
	}

	if len(d.MatchingEventTypes) == 0 {
		return badRequest("MatchingEventTypes must name at least one event type.")
	}

	for _, t := range d.MatchingEventTypes {
		if !slices.Contains(eventTypes, t) {
			return badRequest("Unknown event type %s.", t)
		}
	}

	return nil
}

// editConfigSet changes a configuration set with fn.
func (s *Service) editConfigSet(ctx context.Context, set *string, fn func(*configSet) error) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		c, err := loadConfigSet(tx, scope, set)
		if err != nil {
			return err
		}

		if editErr := fn(c); editErr != nil {
			return editErr
		}

		return storage.Write(tx, scope.Key(service, kindConfigSet, *set), c)
	})
}

// destination finds the event destination name names.
func (c *configSet) destination(name *string) (int, error) {
	i := slices.IndexFunc(c.Destinations, func(d api.EventDestination) bool {
		return common.Value(d.Name) == common.Value(name)
	})
	if i < 0 {
		return 0, notFound("event-destination", "Event destination %s does not exist.", common.Value(name))
	}

	return i, nil
}

func eventDestination(name *string, d *api.EventDestinationDefinition) api.EventDestination {
	return api.EventDestination{
		Name: name, Enabled: new(common.Value(d.Enabled)), MatchingEventTypes: d.MatchingEventTypes,
		SnsDestination: d.SnsDestination, EventBridgeDestination: d.EventBridgeDestination,
		KinesisFirehoseDestination: d.KinesisFirehoseDestination,
	}
}

func (s *Service) CreateConfigurationSetEventDestination(
	ctx context.Context,
	in *api.CreateConfigurationSetEventDestinationRequest,
) (*api.CreateConfigurationSetEventDestinationResponse, error) {
	if err := checkName("event destination", in.EventDestinationName); err != nil {
		return nil, err
	}

	if err := checkDestination(in.EventDestination); err != nil {
		return nil, err
	}

	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		if _, err := c.destination(in.EventDestinationName); err == nil {
			return alreadyExists("Event destination %s already exists.", *in.EventDestinationName)
		}

		c.Destinations = append(c.Destinations, eventDestination(in.EventDestinationName, in.EventDestination))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateConfigurationSetEventDestinationResponse{}, nil
}

func (s *Service) GetConfigurationSetEventDestinations(
	ctx context.Context,
	in *api.GetConfigurationSetEventDestinationsRequest,
) (*api.GetConfigurationSetEventDestinationsResponse, error) {
	var c *configSet
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		c, err = loadConfigSet(tx, scope, in.ConfigurationSetName)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetConfigurationSetEventDestinationsResponse{EventDestinations: c.Destinations}, nil
}

func (s *Service) UpdateConfigurationSetEventDestination(
	ctx context.Context,
	in *api.UpdateConfigurationSetEventDestinationRequest,
) (*api.UpdateConfigurationSetEventDestinationResponse, error) {
	if err := checkDestination(in.EventDestination); err != nil {
		return nil, err
	}

	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		i, err := c.destination(in.EventDestinationName)
		if err == nil {
			c.Destinations[i] = eventDestination(in.EventDestinationName, in.EventDestination)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateConfigurationSetEventDestinationResponse{}, nil
}

func (s *Service) DeleteConfigurationSetEventDestination(
	ctx context.Context,
	in *api.DeleteConfigurationSetEventDestinationRequest,
) (*api.DeleteConfigurationSetEventDestinationResponse, error) {
	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		i, err := c.destination(in.EventDestinationName)
		if err == nil {
			c.Destinations = slices.Delete(c.Destinations, i, i+1)
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteConfigurationSetEventDestinationResponse{}, nil
}
