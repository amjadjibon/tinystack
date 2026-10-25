package cloudformation

import (
	"context"
	"reflect"
)

const cfKVSService = "cloudfront"

func cfKVSAttributes(out map[string]any) map[string]any {
	store, _ := out["KeyValueStore"].(map[string]any)
	return map[string]any{"Arn": store["ARN"], "Id": store["Id"], "Status": store["Status"]}
}

func cfKVSDescribe(ctx context.Context, c *client, name string) (map[string]any, error) {
	return c.call(ctx, cfKVSService, "DescribeKeyValueStore", map[string]any{"Name": name})
}

func cfKVSTags(tags map[string]string) map[string]any {
	return map[string]any{"Items": tagList(tags, "Key", "Value")}
}

func init() {
	register("AWS::CloudFront::KeyValueStore", &resourceType{
		properties: []string{"Name", "Comment", "ImportSource", "Tags"},
		attributes: []string{"Arn", "Id", "Status"},
		replace:    []string{"Name"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{"Name": p.str("Name"), "Comment": p.str("Comment"), "Tags": cfKVSTags(p.tags())}
			if p.has("ImportSource") {
				params["ImportSource"] = p["ImportSource"]
			}
			_, err := c.call(ctx, cfKVSService, "CreateKeyValueStore", params)
			if err != nil {
				return "", nil, err
			}
			out, err := cfKVSDescribe(ctx, c, p.str("Name"))
			return p.str("Name"), cfKVSAttributes(out), err
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
				map[string]any{"Name": r.Physical, "Comment": p.str("Comment"), "IfMatch": out["ETag"]},
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
					map[string]any{"Resource": attrs["Arn"], "TagKeys": map[string]any{"Items": keys}},
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
					map[string]any{"Resource": attrs["Arn"], "Tags": cfKVSTags(p.tags())},
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
				map[string]any{"Name": r.Physical, "IfMatch": out["ETag"]},
			)
			return err
		},
	})
}
