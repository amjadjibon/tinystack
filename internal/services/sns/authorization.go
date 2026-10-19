package sns

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	ref := catalog.InputString(input, "TopicArn")
	if ref == "" {
		ref = catalog.InputString(input, "ResourceArn")
	}

	if ref == "" && operation == "CreateTopic" {
		ref = "arn:aws:sns:" + m.Region + ":" + m.AccountID + ":" + catalog.InputString(input, "Name")
	}

	if ref == "" {
		if subscription := catalog.InputString(input, "SubscriptionArn"); subscription != "" {
			parts := strings.Split(subscription, ":")
			if len(parts) == 7 {
				ref = strings.Join(parts[:6], ":")
			}
		}
	}

	if ref == "" {
		return nil, nil
	}

	key, err := topicKeyOf(&ref)
	if err != nil {
		return nil, err
	}

	target, err := s.topicResource(ctx, key, ref)
	return []catalog.AuthorizationResource{target}, err
}

// DeliveryResource describes the topic another service publishes to, with its access policy. A topic that does not
// exist has no owner, which leaves the delivery to fail on its own.
func (s *Service) DeliveryResource(ctx context.Context, topicARN string) (catalog.AuthorizationResource, error) {
	key, err := topicKeyOf(&topicARN)
	if err != nil {
		return catalog.AuthorizationResource{}, err
	}

	return s.topicResource(ctx, key, topicARN)
}

func (s *Service) topicResource(ctx context.Context, key, ref string) (catalog.AuthorizationResource, error) {
	target := catalog.AuthorizationResource{ARN: ref, TagsKnown: true}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		topic, _, readErr := storage.Read[topic](tx, key)
		if topic != nil {
			target.Tags = common.CloneTags(topic.Tags)
			target.OwnerAccount = strings.Split(ref, ":")[4]
			target.Policy = topic.policy(key)
		}

		return readErr
	})
	return target, err
}
