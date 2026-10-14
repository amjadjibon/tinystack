package cloudformation

import (
	"context"
	"maps"
	"slices"
)

const (
	propertyAccessControl     = "AccessControl"
	propertyBucketEncryption  = "BucketEncryption"
	propertyPublicAccessBlock = "PublicAccessBlockConfiguration"
	propertyLifecycle         = "LifecycleConfiguration"
	memberRules               = "Rules"
)

// lifecycleKeys are the rule keys that mean the same in the template and the S3 API.
var lifecycleKeys = []string{
	"Status", "Prefix", "NoncurrentVersionExpiration", "AbortIncompleteMultipartUpload",
}

// configureBucketGuards applies the encryption, public access block and
// lifecycle properties of a bucket that changed, removing those it dropped.
func configureBucketGuards(ctx context.Context, c *client, name string, old, p props) error {
	if v := p.str(propertyAccessControl); v != "" && v != "Private" {
		return validation(
			"TinyStack supports only Private for the %s property of AWS::S3::Bucket", propertyAccessControl,
		)
	}

	base := map[string]any{memberBucket: name}
	if p.changed(old, propertyBucketEncryption) {
		err := bucketSetting(ctx, c, p, propertyBucketEncryption, "PutBucketEncryption", "DeleteBucketEncryption",
			base, bucketEncryption)
		if err != nil {
			return err
		}
	}

	if p.changed(old, propertyPublicAccessBlock) {
		err := bucketSetting(ctx, c, p, propertyPublicAccessBlock, "PutPublicAccessBlock", "DeletePublicAccessBlock",
			base, func(v any) (map[string]any, error) {
				return map[string]any{propertyPublicAccessBlock: v}, nil
			})
		if err != nil {
			return err
		}
	}

	if p.changed(old, propertyLifecycle) {
		return bucketSetting(ctx, c, p, propertyLifecycle, "PutBucketLifecycleConfiguration", "DeleteBucketLifecycle",
			base, bucketLifecycle)
	}

	return nil
}

// bucketSetting puts a bucket setting built from property by convert, or
// deletes the setting when the property is gone.
func bucketSetting(
	ctx context.Context, c *client, p props, property, put, remove string, base map[string]any,
	convert func(any) (map[string]any, error),
) error {
	value, ok := p[property]
	if !ok {
		_, err := c.call(ctx, "s3", remove, base)
		return err
	}

	params, err := convert(value)
	if err != nil {
		return err
	}

	maps.Copy(params, base)
	_, err = c.call(ctx, "s3", put, params)
	return err
}

// bucketEncryption turns BucketEncryption into PutBucketEncryption's input.
func bucketEncryption(value any) (map[string]any, error) {
	spec, _ := value.(map[string]any)
	rules, _ := spec["ServerSideEncryptionConfiguration"].([]any)
	if len(spec) != 1 || len(rules) == 0 {
		return nil, validation("BucketEncryption must have only ServerSideEncryptionConfiguration, with rules")
	}

	// The template calls a rule's ApplyServerSideEncryptionByDefault ServerSideEncryptionByDefault.
	converted := make([]any, 0, len(rules))
	for _, item := range rules {
		rule, _ := item.(map[string]any)
		out := maps.Clone(rule)
		if byDefault, ok := out["ServerSideEncryptionByDefault"]; ok {
			delete(out, "ServerSideEncryptionByDefault")
			out["ApplyServerSideEncryptionByDefault"] = byDefault
		}

		converted = append(converted, out)
	}

	return map[string]any{"ServerSideEncryptionConfiguration": map[string]any{memberRules: converted}}, nil
}

// bucketLifecycle turns LifecycleConfiguration into PutBucketLifecycleConfiguration's input.
func bucketLifecycle(value any) (map[string]any, error) {
	spec, _ := value.(map[string]any)
	templateRules, _ := spec[memberRules].([]any)
	if len(spec) != 1 || len(templateRules) == 0 {
		return nil, validation("LifecycleConfiguration must have only Rules, with at least one rule")
	}

	rules := make([]any, 0, len(templateRules))
	for _, item := range templateRules {
		rule, _ := item.(map[string]any)
		out := map[string]any{}
		for k, v := range rule {
			switch {
			case k == "Id":
				out["ID"] = v
			case k == "ExpirationInDays":
				out["Expiration"] = map[string]any{"Days": v}
			case slices.Contains(lifecycleKeys, k):
				out[k] = v
			default:
				return nil, validation("TinyStack does not support the %s property of a lifecycle rule", k)
			}
		}

		rules = append(rules, out)
	}

	return map[string]any{propertyLifecycle: map[string]any{memberRules: rules}}, nil
}
