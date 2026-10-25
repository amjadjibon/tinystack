package cloudformation

import (
	"context"
	"reflect"
)

const (
	cfKVSService  = "cloudfront"
	cfKVSComment  = "Comment"
	cfKVSResource = "Resource"
)

func cfKVSAttributes(out map[string]any) map[string]any {
	store, _ := out["KeyValueStore"].(map[string]any)
	return map[string]any{memberArn: store["ARN"], "Id": store["Id"], memberStatus: store[memberStatus]}
}

func cfKVSDescribe(ctx context.Context, c *client, name string) (map[string]any, error) {
	return c.call(ctx, cfKVSService, "DescribeKeyValueStore", map[string]any{memberName: name})
}

func cfKVSTags(tags map[string]string) map[string]any {
	return map[string]any{"Items": tagList(tags, "Key", "Value")}
}

func init() {
	register("AWS::CloudFront::KeyValueStore", &resourceType{
		properties: []string{memberName, cfKVSComment, "ImportSource", memberTags},
		attributes: []string{memberArn, "Id", memberStatus},
		replace:    []string{memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				memberName:   p.str(memberName),
				cfKVSComment: p.str(cfKVSComment),
				memberTags:   cfKVSTags(p.tags()),
			}
			if p.has("ImportSource") {
				params["ImportSource"] = p["ImportSource"]
			}

			_, err := c.call(ctx, cfKVSService, "CreateKeyValueStore", params)
			if err != nil {
				return "", nil, err
			}

			out, err := cfKVSDescribe(ctx, c, p.str(memberName))
			if err != nil {
				return "", nil, err
			}

			attrs := cfKVSAttributes(out)
			if attrs[memberStatus] != "READY" {
				return "", nil, validation("Key value store import failed")
			}

			return p.str(memberName), cfKVSAttributes(out), err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if !reflect.DeepEqual(r.Properties["ImportSource"], p["ImportSource"]) {
				return nil, validation("ImportSource updates are not supported")
			}

			out, err := cfKVSDescribe(ctx, c, r.Physical)
			if err != nil {
				return nil, err
			}

			out, err = c.call(
				ctx,
				cfKVSService,
				"UpdateKeyValueStore",
				map[string]any{memberName: r.Physical, cfKVSComment: p.str(cfKVSComment), "IfMatch": out["ETag"]},
			)
			if err != nil {
				return nil, err
			}

			attrs := cfKVSAttributes(out)
			old := props(r.Properties).tags()
			var keys []any
			for key := range old {
				keys = append(keys, key)
			}

			if len(keys) > 0 {
				_, err = c.call(
					ctx,
					cfKVSService,
					"UntagResource",
					map[string]any{cfKVSResource: attrs[memberArn], "TagKeys": map[string]any{"Items": keys}},
				)
				if err != nil {
					return nil, err
				}
			}

			if len(p.tags()) > 0 {
				_, err = c.call(
					ctx,
					cfKVSService,
					"TagResource",
					map[string]any{cfKVSResource: attrs[memberArn], memberTags: cfKVSTags(p.tags())},
				)
				if err != nil {
					return nil, err
				}
			}

			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			out, err := cfKVSDescribe(ctx, c, r.Physical)
			if gone(err, "EntityNotFound") {
				return nil
			}

			if err != nil {
				return err
			}

			_, err = c.call(
				ctx,
				cfKVSService,
				"DeleteKeyValueStore",
				map[string]any{memberName: r.Physical, "IfMatch": out["ETag"]},
			)
			return err
		},
	})
}
