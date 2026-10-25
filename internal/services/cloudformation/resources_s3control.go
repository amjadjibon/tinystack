package cloudformation

import (
	"context"
	"encoding/json"
	"reflect"
)

const s3ControlService = "s3control"

func accessPointParams(c *client, id string) map[string]any {
	return map[string]any{"AccountId": c.scope.AccountID, "Name": id}
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

	return map[string]any{"Name": id, "Arn": out["AccessPointArn"], "Alias": out["Alias"], "NetworkOrigin": out["NetworkOrigin"]}, nil
}

func init() {
	register("AWS::S3::AccessPoint", &resourceType{
		name:       memberName,
		properties: []string{memberName, memberBucket, "BucketAccountId", "VpcConfiguration", propertyPublicAccessBlock, memberPolicy, memberTags},
		attributes: []string{memberName, memberArn, "Alias", "NetworkOrigin"},
		replace:    []string{memberName, memberBucket, "BucketAccountId", "VpcConfiguration"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			id := p.str(memberName)
			if id == "" {
				id = "ap-" + c.generate(20)
			}

			params := accessPointParams(c, id)
			for _, field := range []string{memberBucket, "BucketAccountId", "VpcConfiguration", propertyPublicAccessBlock, memberTags} {
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
				return nil, validation("Updating access point PublicAccessBlockConfiguration is unsupported")
			}

			if err := configureAccessPoint(ctx, c, r.Physical, props(r.Properties), p); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, &tagger{service: s3ControlService, tag: opTagResource, untag: opUntagResource, target: map[string]any{"AccountId": c.scope.AccountID, "ResourceArn": r.Attributes[memberArn]}}); err != nil {
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
