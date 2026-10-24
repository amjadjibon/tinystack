package cloudformation

import (
	"context"
	"strings"
)

const (
	appSyncService      = "appsync"
	appSyncAPIID        = "ApiId"
	appSyncAPIARN       = "ApiArn"
	appSyncNamespaceARN = "ChannelNamespaceArn"
)

func appSyncParams(p props, namespace bool) map[string]any {
	fields := []string{memberName, "OwnerContact", "EventConfig"}
	if namespace {
		fields = []string{appSyncAPIID, memberName, "CodeHandlers", "PublishAuthModes", "SubscribeAuthModes"}
	}

	out := map[string]any{}
	for _, field := range fields {
		if p.has(field) {
			out[field] = p[field]
		}
	}

	return out
}

func appSyncAttributes(out map[string]any) map[string]any {
	dns, _ := out["Dns"].(map[string]any)
	return map[string]any{
		appSyncAPIID: out[appSyncAPIID], appSyncAPIARN: out[appSyncAPIARN],
		"Dns.Http": dns["HTTP"], "Dns.Realtime": dns["REALTIME"],
	}
}

func appSyncTagger(arn string) *tagger {
	return &tagger{
		service: appSyncService, tag: opTagResource, untag: opUntagResource,
		target: map[string]any{memberResourceArn: arn},
	}
}

func init() {
	register("AWS::AppSync::Api", &resourceType{
		properties: []string{memberName, "OwnerContact", "EventConfig", memberTags},
		attributes: []string{appSyncAPIID, appSyncAPIARN, "Dns.Http", "Dns.Realtime"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := appSyncParams(p, false)
			params[memberTags] = tagMap(p.tags())
			out, err := c.call(ctx, appSyncService, "CreateApi", params)
			if err != nil {
				return "", nil, err
			}

			a, _ := out["Api"].(map[string]any)
			return outText(a, appSyncAPIARN), appSyncAttributes(a), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := appSyncParams(p, false)
			params[appSyncAPIID] = r.Attributes[appSyncAPIID]
			out, err := c.call(ctx, appSyncService, "UpdateApi", params)
			if err != nil {
				return nil, err
			}

			a, _ := out["Api"].(map[string]any)
			err = retag(ctx, c, r.Properties, p, appSyncTagger(r.Physical))
			return appSyncAttributes(a), err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, appSyncService, "DeleteApi", map[string]any{appSyncAPIID: r.Attributes[appSyncAPIID]})
			if gone(err, "NotFoundException") {
				return nil
			}

			return err
		},
	})
	register("AWS::AppSync::ChannelNamespace", &resourceType{
		properties: []string{
			appSyncAPIID,
			memberName,
			"CodeHandlers",
			"PublishAuthModes",
			"SubscribeAuthModes",
			memberTags,
		},
		attributes: []string{appSyncNamespaceARN},
		replace:    []string{appSyncAPIID, memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := appSyncParams(p, true)
			params[memberTags] = tagMap(p.tags())
			out, err := c.call(ctx, appSyncService, "CreateChannelNamespace", params)
			if err != nil {
				return "", nil, err
			}

			ns, _ := out["ChannelNamespace"].(map[string]any)
			arn := outText(ns, appSyncNamespaceARN)
			return arn, map[string]any{appSyncNamespaceARN: arn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := appSyncParams(p, true)

			_, err := c.call(ctx, appSyncService, "UpdateChannelNamespace", params)
			if err != nil {
				return nil, err
			}

			err = retag(ctx, c, r.Properties, p, appSyncTagger(r.Physical))
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			id := strings.Split(r.Physical, "/")
			_, err := c.call(ctx, appSyncService, "DeleteChannelNamespace", map[string]any{
				appSyncAPIID: id[len(id)-3], memberName: id[len(id)-1],
			})
			if gone(err, "NotFoundException") {
				return nil
			}

			return err
		},
	})
}
