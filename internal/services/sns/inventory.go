package sns

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const detailAttributes = "Attributes"

// subscriptionView is a subscription as the resource browser shows it.
type subscriptionView struct {
	SubscriptionArn     string
	Protocol            string
	Endpoint            string
	FilterPolicy        string
	FilterPolicyScope   string
	RedrivePolicy       string `json:",omitempty"`
	DeliveryPolicy      string `json:",omitempty"`
	SubscriptionRoleArn string `json:",omitempty"`
	RawMessageDelivery  bool
	PendingConfirmation bool
}

// Resources lists every topic, platform application, and platform endpoint for
// the resource browser.
func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	var out []catalog.Resource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, rest, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			scope := request.Scope{AccountID: account, Region: region}
			switch {
			case strings.HasPrefix(rest, kindSMS+"/"):
				continue
			case strings.HasPrefix(rest, kindApp+"/"):
				rec, found, readErr := storage.Read[platformApplication](tx, key)
				if readErr != nil || !found {
					if readErr != nil {
						return readErr
					}

					continue
				}

				out = append(out, catalog.Resource{
					Type:    "platform-application",
					Account: account,
					Region:  region,
					Name:    rec.Platform + "/" + rec.Name,
					ARN:     appARN(scope, rec.Platform, rec.Name),
					Details: map[string]any{"Platform": rec.Platform, detailAttributes: rec.visibleAttributes()},
				})
			case strings.HasPrefix(rest, kindEndpoint+"/"):
				ep, found, readErr := storage.Read[platformEndpoint](tx, key)
				if readErr != nil || !found {
					if readErr != nil {
						return readErr
					}

					continue
				}

				out = append(out, catalog.Resource{
					Type:    "platform-endpoint",
					Account: account,
					Region:  region,
					Name:    ep.ID,
					ARN:     endpointARN(scope, ep.Platform, ep.AppName, ep.ID),
					Details: map[string]any{
						"Platform":       ep.Platform,
						"Application":    ep.AppName,
						detailAttributes: ep.visibleAttributes(),
					},
				})
			case !strings.Contains(rest, "/"):
				t, err := load(tx, key)
				if err != nil {
					return err
				}

				arn := keyARN(key)
				subscriptions := make([]subscriptionView, 0, len(t.Subscriptions))
				for i := range t.Subscriptions {
					sub := &t.Subscriptions[i]
					subscriptions = append(subscriptions, subscriptionView{
						SubscriptionArn:     arn + ":" + sub.ID,
						Protocol:            sub.Protocol,
						Endpoint:            sub.Endpoint,
						FilterPolicy:        sub.FilterPolicy,
						FilterPolicyScope:   sub.FilterPolicyScope,
						RedrivePolicy:       sub.RedrivePolicy,
						DeliveryPolicy:      sub.DeliveryPolicy,
						SubscriptionRoleArn: sub.SubscriptionRoleArn,
						RawMessageDelivery:  sub.Raw,
						PendingConfirmation: sub.Pending,
					})
				}

				out = append(out, catalog.Resource{
					Type:    "topic",
					Account: account,
					Region:  region,
					Name:    rest,
					ARN:     arn,
					Tags:    t.Tags,
					Details: map[string]any{detailAttributes: t.Attributes, "Subscriptions": subscriptions},
				})
			}
		}

		return nil
	})

	return out, err
}
