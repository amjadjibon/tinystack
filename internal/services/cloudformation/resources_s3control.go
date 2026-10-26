package cloudformation

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3control"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

type s3ControlRecords interface {
	CloudFormationPublicAccessBlock(context.Context, string, *api.PublicAccessBlockConfiguration) error
}

func (s *Service) UseS3Control(records s3ControlRecords) { s.s3control = records }

const (
	s3ControlService  = "s3control"
	s3BucketAccountID = "BucketAccountId"
	s3VpcConfig       = "VpcConfiguration"
	s3Regions         = "Regions"
)

func accessPointParams(c *client, id string) map[string]any {
	return map[string]any{shieldAccountID: c.scope.AccountID, memberName: id}
}

func configureAccessPoint(ctx context.Context, c *client, id string, old, p props) error {
	if !reflect.DeepEqual(old[memberPolicy], p[memberPolicy]) {
		params := accessPointParams(c, id)
		op := "DeleteAccessPointPolicy"
		if p.has(memberPolicy) {
			op = "PutAccessPointPolicy"
			body, err := json.Marshal(p[memberPolicy])
			if err != nil {
				return err
			}

			params[memberPolicy] = string(body)
		}

		if _, err := c.call(ctx, s3ControlService, op, params); err != nil {
			return err
		}
	}

	return nil
}

func accessPointAttributes(ctx context.Context, c *client, id string) (map[string]any, error) {
	out, err := c.call(ctx, s3ControlService, "GetAccessPoint", accessPointParams(c, id))
	if err != nil {
		return nil, err
	}

	return map[string]any{
		memberName:      id,
		"Arn":           out["AccessPointArn"],
		propAPSAlias:    out[propAPSAlias],
		"NetworkOrigin": out["NetworkOrigin"],
	}, nil
}

func init() {
	register("AWS::S3::AccessPoint", &resourceType{
		name: memberName,
		properties: []string{
			memberName,
			memberBucket,
			s3BucketAccountID,
			s3VpcConfig,
			propertyPublicAccessBlock,
			memberPolicy,
			memberTags,
		},
		attributes: []string{memberName, memberArn, propAPSAlias, "NetworkOrigin"},
		replace:    []string{memberName, memberBucket, s3BucketAccountID, s3VpcConfig},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := p.str(memberName)
			if id == "" {
				id = "ap-" + c.generate(20)
			}

			params := accessPointParams(c, id)
			for _, field := range []string{memberBucket, s3BucketAccountID, s3VpcConfig, propertyPublicAccessBlock, memberTags} {
				if p.has(field) {
					params[field] = p[field]
				}
			}

			if _, err := c.call(ctx, s3ControlService, "CreateAccessPoint", params); err != nil {
				return "", nil, err
			}

			if err := configureAccessPoint(ctx, c, id, nil, p); err != nil {
				return "", nil, err
			}

			attrs, err := accessPointAttributes(ctx, c, id)
			return id, attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if !reflect.DeepEqual(r.Properties[propertyPublicAccessBlock], p[propertyPublicAccessBlock]) {
				if c.s3control == nil {
					return nil, validation("S3 Control adapter is unavailable")
				}

				var config *api.PublicAccessBlockConfiguration
				if p.has(propertyPublicAccessBlock) {
					body, err := json.Marshal(p[propertyPublicAccessBlock])
					if err != nil {
						return nil, err
					}

					if err := json.Unmarshal(body, &config); err != nil {
						return nil, err
					}
				}

				updateCtx := request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: s3ControlService})
				if err := c.s3control.CloudFormationPublicAccessBlock(updateCtx, r.Physical, config); err != nil {
					return nil, err
				}
			}

			if err := configureAccessPoint(ctx, c, r.Physical, props(r.Properties), p); err != nil {
				return nil, err
			}

			if err := retag(
				ctx,
				c,
				r.Properties,
				p,
				&tagger{
					service: s3ControlService,
					tag:     opTagResource,
					untag:   opUntagResource,
					target: map[string]any{
						shieldAccountID:   c.scope.AccountID,
						memberResourceArn: r.Attributes[memberArn],
					},
				},
			); err != nil {
				return nil, err
			}

			return accessPointAttributes(ctx, c, r.Physical)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, s3ControlService, "DeleteAccessPoint", accessPointParams(c, r.Physical))
			return err
		},
	})
}

func awaitS3Control(ctx context.Context, c *client, token any) error {
	for {
		out, err := c.call(
			ctx,
			s3ControlService,
			"DescribeMultiRegionAccessPointOperation",
			map[string]any{shieldAccountID: c.scope.AccountID, "RequestTokenARN": token},
		)
		if err != nil {
			return err
		}

		operation, _ := out["AsyncOperation"].(map[string]any)
		if outText(operation, "RequestStatus") == "SUCCEEDED" {
			return nil
		}

		if outText(operation, "RequestStatus") == "FAILED" {
			return validation("Multi-Region Access Point operation failed")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func init() {
	register("AWS::S3::MultiRegionAccessPoint", &resourceType{
		name:       memberName,
		properties: []string{memberName, s3Regions, propertyPublicAccessBlock},
		attributes: []string{memberName, propAPSAlias, "CreatedAt"},
		replace:    []string{memberName, s3Regions, propertyPublicAccessBlock},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := p.str(memberName)
			if id == "" {
				id = "mrap-" + c.generate(20)
			}

			details := map[string]any{memberName: id, s3Regions: p[s3Regions]}
			if p.has(propertyPublicAccessBlock) {
				details["PublicAccessBlock"] = p[propertyPublicAccessBlock]
			}

			out, err := c.call(
				ctx,
				s3ControlService,
				"CreateMultiRegionAccessPoint",
				map[string]any{shieldAccountID: c.scope.AccountID, "ClientToken": c.generate(32), "Details": details},
			)
			if err != nil {
				return "", nil, err
			}

			if waitErr := awaitS3Control(ctx, c, out["RequestTokenARN"]); waitErr != nil {
				return "", nil, waitErr
			}

			got, err := c.call(ctx, s3ControlService, "GetMultiRegionAccessPoint", accessPointParams(c, id))
			if err != nil {
				return "", nil, err
			}

			attrs, _ := got["AccessPoint"].(map[string]any)
			return id, attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			out, err := c.call(
				ctx,
				s3ControlService,
				"DeleteMultiRegionAccessPoint",
				map[string]any{
					shieldAccountID: c.scope.AccountID,
					"ClientToken":   c.generate(32),
					"Details":       map[string]any{memberName: r.Physical},
				},
			)
			if gone(err, "NoSuchMultiRegionAccessPoint") {
				return nil
			}

			if err != nil {
				return err
			}

			return awaitS3Control(ctx, c, out["RequestTokenARN"])
		},
	})
}
