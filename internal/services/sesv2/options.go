package sesv2

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) PutConfigurationSetDeliveryOptions(
	ctx context.Context,
	in *api.PutConfigurationSetDeliveryOptionsRequest,
) (*api.PutConfigurationSetDeliveryOptionsResponse, error) {
	if in.TlsPolicy != nil && !slices.Contains([]string{"REQUIRE", "OPTIONAL"}, *in.TlsPolicy) {
		return nil, badRequest("Invalid TlsPolicy.")
	}

	if in.MaxDeliverySeconds != nil && (*in.MaxDeliverySeconds < 300 || *in.MaxDeliverySeconds > 50400) {
		return nil, badRequest("MaxDeliverySeconds must be between 300 and 50400.")
	}

	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		c.Settings.DeliveryOptions = &api.DeliveryOptions{
			TlsPolicy: in.TlsPolicy, SendingPoolName: in.SendingPoolName, MaxDeliverySeconds: in.MaxDeliverySeconds,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutConfigurationSetDeliveryOptionsResponse{}, nil
}

func (s *Service) PutConfigurationSetReputationOptions(
	ctx context.Context,
	in *api.PutConfigurationSetReputationOptionsRequest,
) (*api.PutConfigurationSetReputationOptionsResponse, error) {
	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		if c.Settings.ReputationOptions == nil {
			c.Settings.ReputationOptions = &api.ReputationOptions{}
		}

		c.Settings.ReputationOptions.ReputationMetricsEnabled = in.ReputationMetricsEnabled
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutConfigurationSetReputationOptionsResponse{}, nil
}

func (s *Service) PutConfigurationSetSendingOptions(
	ctx context.Context,
	in *api.PutConfigurationSetSendingOptionsRequest,
) (*api.PutConfigurationSetSendingOptionsResponse, error) {
	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		c.Settings.SendingOptions = &api.SendingOptions{SendingEnabled: in.SendingEnabled}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutConfigurationSetSendingOptionsResponse{}, nil
}

func (s *Service) PutConfigurationSetTrackingOptions(
	ctx context.Context,
	in *api.PutConfigurationSetTrackingOptionsRequest,
) (*api.PutConfigurationSetTrackingOptionsResponse, error) {
	if in.HttpsPolicy != nil &&
		!slices.Contains([]string{"REQUIRE", "REQUIRE_OPEN_ONLY", "OPTIONAL"}, *in.HttpsPolicy) {
		return nil, badRequest("Invalid HttpsPolicy.")
	}

	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		c.Settings.TrackingOptions = &api.TrackingOptions{
			CustomRedirectDomain: in.CustomRedirectDomain, HttpsPolicy: in.HttpsPolicy,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutConfigurationSetTrackingOptionsResponse{}, nil
}

func (s *Service) PutConfigurationSetSuppressionOptions(
	ctx context.Context,
	in *api.PutConfigurationSetSuppressionOptionsRequest,
) (*api.PutConfigurationSetSuppressionOptionsResponse, error) {
	for _, reason := range in.SuppressedReasons {
		if reason != "BOUNCE" && reason != "COMPLAINT" {
			return nil, badRequest("Invalid suppressed reason %s.", reason)
		}
	}

	err := s.editConfigSet(ctx, in.ConfigurationSetName, func(c *configSet) error {
		c.Settings.SuppressionOptions = &api.SuppressionOptions{SuppressedReasons: in.SuppressedReasons}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutConfigurationSetSuppressionOptionsResponse{}, nil
}

func (s *Service) PutEmailIdentityConfigurationSetAttributes(
	ctx context.Context, in *api.PutEmailIdentityConfigurationSetAttributesRequest,
) (*api.PutEmailIdentityConfigurationSetAttributesResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		i, err := loadIdentity(tx, scope, in.EmailIdentity)
		if err != nil {
			return err
		}

		if common.Value(in.ConfigurationSetName) != "" {
			if _, setErr := loadConfigSet(tx, scope, in.ConfigurationSetName); setErr != nil {
				return setErr
			}
		}

		i.ConfigurationSet = common.Value(in.ConfigurationSetName)
		return storage.Write(tx, identityKey(scope, i.Name), i)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutEmailIdentityConfigurationSetAttributesResponse{}, nil
}
