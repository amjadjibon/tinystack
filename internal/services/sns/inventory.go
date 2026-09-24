package sns

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// subscriptionView is a subscription as the resource browser shows it.
type subscriptionView struct {
	SubscriptionArn    string
	Protocol           string
	Endpoint           string
	RawMessageDelivery bool
}

// Resources lists every topic with its attributes and subscriptions, for the
// resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, name, ok := storage.Scoped(key, "sns")
			if !ok {
				continue
			}

			t, err := load(tx, key)
			if err != nil {
				return err
			}

			arn := keyARN(key)
			subscriptions := make([]subscriptionView, 0, len(t.Subscriptions))
			for _, sub := range t.Subscriptions {
				subscriptions = append(subscriptions, subscriptionView{
					SubscriptionArn:    arn + ":" + sub.ID,
					Protocol:           sub.Protocol,
					Endpoint:           sub.Endpoint,
					RawMessageDelivery: sub.Raw,
				})
			}

			out = append(out, catalog.Resource{
				Type:    "topic",
				Account: account,
				Region:  region,
				Name:    name,
				ARN:     arn,
				Tags:    t.Tags,
				Details: map[string]any{"Attributes": t.Attributes, "Subscriptions": subscriptions},
			})
		}

		return nil
	})

	return out, err
}
