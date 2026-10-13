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

	target := catalog.AuthorizationResource{ARN: ref, TagsKnown: true}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		topic, _, readErr := storage.Read[topic](tx, key)
		if topic != nil {
			target.Tags = common.CloneTags(topic.Tags)
		}

		return readErr
	})
	return []catalog.AuthorizationResource{target}, err
}
