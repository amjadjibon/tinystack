package amplify

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// CreateWebhook serves the corresponding records-only Hosting operation.
func (s *Service) CreateWebhook(ctx context.Context, in *api.CreateWebhookRequest) (*api.CreateWebhookResult, error) {
	if err := validateCreateWebhookRequest(in); err != nil {
		return nil, err
	}

	id := common.RandomID(16)
	r, err := s.change(
		ctx,
		"webhook",
		id,
		common.Value(in.AppId),
		common.Value(in.BranchName),
		true,
		false,
		func(r *record, scope request.Scope) error {
			r.Webhook = &api.Webhook{}
			r.Name = id
			r.ARN = resourceARN(scope, "webhooks/"+id)
			r.Webhook.WebhookArn = new(r.ARN)
			r.Webhook.WebhookId = new(id)
			r.Webhook.WebhookUrl = new("https://amplify.invalid/webhooks/" + id)
			r.Webhook.Description = new("")
			r.Webhook.CreateTime = common.SecondsMilli(time.Now())
			r.Webhook.UpdateTime = common.SecondsMilli(time.Now())
			if in.Description != nil {
				r.Webhook.Description = in.Description
			}

			r.Webhook.AppId = in.AppId
			r.Webhook.BranchName = in.BranchName
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.CreateWebhookResult{Webhook: common.Clone(r.Webhook)}, nil
}

// GetWebhook serves the corresponding records-only Hosting operation.
func (s *Service) GetWebhook(ctx context.Context, in *api.GetWebhookRequest) (*api.GetWebhookResult, error) {
	if err := validateGetWebhookRequest(in); err != nil {
		return nil, err
	}

	id := common.Value(in.WebhookId)
	r, err := s.get(ctx, "webhook", id)
	if err != nil {
		return nil, err
	}

	return &api.GetWebhookResult{Webhook: common.Clone(r.Webhook)}, nil
}

// UpdateWebhook serves the corresponding records-only Hosting operation.
func (s *Service) UpdateWebhook(ctx context.Context, in *api.UpdateWebhookRequest) (*api.UpdateWebhookResult, error) {
	if err := validateUpdateWebhookRequest(in); err != nil {
		return nil, err
	}

	id := common.Value(in.WebhookId)
	r, err := s.change(ctx, "webhook", id, "", "", false, false, func(r *record, scope request.Scope) error {
		r.Webhook.UpdateTime = common.SecondsMilli(time.Now())
		if in.WebhookId != nil {
			r.Webhook.WebhookId = in.WebhookId
		}

		if in.Description != nil {
			r.Webhook.Description = in.Description
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateWebhookResult{Webhook: common.Clone(r.Webhook)}, nil
}

// DeleteWebhook serves the corresponding records-only Hosting operation.
func (s *Service) DeleteWebhook(ctx context.Context, in *api.DeleteWebhookRequest) (*api.DeleteWebhookResult, error) {
	if err := validateDeleteWebhookRequest(in); err != nil {
		return nil, err
	}

	id := common.Value(in.WebhookId)
	r, err := s.change(ctx, "webhook", id, "", "", false, true, func(r *record, scope request.Scope) error {
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteWebhookResult{Webhook: common.Clone(r.Webhook)}, nil
}

// ListWebhooks serves the corresponding records-only Hosting operation.
func (s *Service) ListWebhooks(ctx context.Context, in *api.ListWebhooksRequest) (*api.ListWebhooksResult, error) {
	if err := validateListWebhooksRequest(in); err != nil {
		return nil, err
	}

	items, next, err := s.list(ctx, "webhook", common.Value(in.AppId), "", "", in.NextToken, in.MaxResults, 50)
	if err != nil {
		return nil, err
	}

	out := &api.ListWebhooksResult{NextToken: next, Webhooks: []api.Webhook{}}
	for _, r := range items {
		out.Webhooks = append(out.Webhooks, *common.Clone(r.Webhook))
	}

	return out, nil
}
