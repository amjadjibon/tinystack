package cloudformation

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mediaconvert"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	mediaConvertTemplate = "JobTemplate"
	mediaConvertQueue    = "Queue"
)

func init() {
	for _, kind := range []string{mediaConvertQueue, "Preset", mediaConvertTemplate} {
		fields := []string{memberName, memberDescription, memberTags}
		switch kind {
		case mediaConvertQueue:
			fields = append(fields, "ConcurrentJobs", "PricingPlan", "Status")
		case "Preset":
			fields = append(fields, "Category", "SettingsJson")
		case mediaConvertTemplate:
			fields = append(fields, "Category", "SettingsJson", "Priority", mediaConvertQueue, "StatusUpdateInterval")
		}

		register("AWS::MediaConvert::"+kind, &resourceType{
			properties: fields, attributes: []string{memberArn, memberName}, replace: []string{memberName},
			create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
				params, err := mediaConvertParams(p, kind, false)
				if err != nil {
					return "", nil, err
				}

				if !p.has(memberName) {
					params[memberName] = "tinystack-" + common.RandomID(16)
				}

				params[memberTags] = tagMap(p.tags())
				out, err := c.call(ctx, "mediaconvert", "Create"+kind, params)
				if err != nil {
					return "", nil, err
				}

				value, _ := out[kind].(map[string]any)
				name := outText(value, memberName)
				return name, map[string]any{memberName: name, memberArn: outText(value, memberArn)}, nil
			},
			update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
				params, err := mediaConvertParams(p, kind, true)
				if err != nil {
					return nil, err
				}

				params[memberName] = r.Physical
				if updateErr := c.mediaConvertUpdate(ctx, kind, params); updateErr != nil {
					return nil, updateErr
				}

				err = retag(
					ctx,
					c,
					r.Properties,
					p,
					&tagger{
						service: "mediaconvert",
						tag:     opTagResource,
						untag:   opUntagResource,
						target:  map[string]any{memberArn: r.Attributes[memberArn]},
					},
				)
				return r.Attributes, err
			},
			remove: func(ctx context.Context, c *client, r *resource) error {
				_, err := c.call(ctx, "mediaconvert", "Delete"+kind, map[string]any{memberName: r.Physical})
				if gone(err, "NotFoundException") {
					return nil
				}

				return err
			},
		})
	}
}

func mediaConvertParams(p props, kind string, update bool) (map[string]any, error) {
	out := map[string]any{}
	if kind == mediaConvertQueue && p.has("PricingPlan") && p.str("PricingPlan") != "ON_DEMAND" {
		return nil, awserror.Validation("CloudFormation supports only ON_DEMAND MediaConvert queues")
	}

	for _, field := range []string{
		memberName, memberDescription, "Category", "ConcurrentJobs", "Status", "PricingPlan",
		"Priority", mediaConvertQueue, "StatusUpdateInterval",
	} {
		if p.has(field) {
			out[field] = p[field]
		}
	}

	if update {
		delete(out, "PricingPlan")
		for _, field := range []string{memberDescription, "Category"} {
			if !p.has(field) && (field == memberDescription || kind != mediaConvertQueue) {
				out[field] = ""
			}
		}

		if kind == mediaConvertQueue && !p.has("Status") {
			out["Status"] = "ACTIVE"
		}

		if kind == mediaConvertTemplate {
			if !p.has("Priority") {
				out["Priority"] = 0
			}

			if !p.has(mediaConvertQueue) {
				out[mediaConvertQueue] = "Default"
			}

			if !p.has("StatusUpdateInterval") {
				out["StatusUpdateInterval"] = "SECONDS_60"
			}
		}
	}

	if kind != mediaConvertQueue {
		if !p.has("SettingsJson") {
			return nil, awserror.Validation("SettingsJson is required")
		}

		out["Settings"] = p["SettingsJson"]
	}

	return out, nil
}

type mediaConvertRecords interface {
	CloudFormationUpdateQueue(context.Context, *api.UpdateQueueRequest) (*api.UpdateQueueResponse, error)
}

// UseMediaConvert connects complete queue updates, including concurrency property removal.
func (s *Service) UseMediaConvert(records mediaConvertRecords) { s.mediaConvert = records }

func (c *client) mediaConvertUpdate(ctx context.Context, kind string, params map[string]any) error {
	if kind != mediaConvertQueue {
		_, err := c.call(ctx, "mediaconvert", "Update"+kind, params)
		return err
	}

	if c.mediaConvert == nil {
		return validation("MediaConvert template updates are not connected")
	}

	_, input, err := c.caller.Decode("mediaconvert", "UpdateQueue", params)
	if err != nil {
		return err
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: "mediaconvert", Endpoint: c.endpoint})
	_, err = c.mediaConvert.CloudFormationUpdateQueue(ctx, input.(*api.UpdateQueueRequest))
	return err
}
