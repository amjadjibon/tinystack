package cloudformation

import "context"

const (
	resourceExplorerService    = "resourceexplorer2"
	resourceExplorerViewArn    = "ViewArn"
	resourceExplorerProperties = "IncludedProperties"
	resourceExplorerFilters    = "Filters"
	resourceExplorerViewName   = "ViewName"
)

func resourceExplorerTagger(a string) *tagger {
	return &tagger{
		service: resourceExplorerService,
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{memberResourceArn: a},
	}
}

func init() {
	register("AWS::ResourceExplorer2::Index", &resourceType{
		properties: []string{memberType, memberTags}, attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(
				ctx,
				resourceExplorerService,
				"CreateIndex",
				map[string]any{memberTags: tagMap(p.tags())},
			)
			if err != nil {
				return "", nil, err
			}

			a := outText(out, memberArn)
			if _, err := c.call(
				ctx,
				resourceExplorerService,
				"UpdateIndexType",
				map[string]any{memberArn: a, memberType: p[memberType]},
			); err != nil {
				return "", nil, err
			}

			return a, map[string]any{memberArn: a}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if _, err := c.call(
				ctx,
				resourceExplorerService,
				"UpdateIndexType",
				map[string]any{memberArn: r.Physical, memberType: p[memberType]},
			); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, resourceExplorerTagger(r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, resourceExplorerService, "DeleteIndex", map[string]any{memberArn: r.Physical})
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})

	register("AWS::ResourceExplorer2::View", &resourceType{
		name: resourceExplorerViewName,
		properties: []string{
			resourceExplorerViewName,
			resourceExplorerProperties,
			resourceExplorerFilters,
			wafScope,
			memberTags,
		},
		attributes: []string{resourceExplorerViewArn},
		replace:    []string{resourceExplorerViewName, wafScope},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := map[string]any{
				resourceExplorerViewName: c.nameOf(p, resourceExplorerViewName, 64),
				memberTags:               tagMap(p.tags()),
			}

			for _, k := range []string{resourceExplorerProperties, resourceExplorerFilters, wafScope} {
				if p.has(k) {
					params[k] = p[k]
				}
			}

			out, err := c.call(ctx, resourceExplorerService, "CreateView", params)
			if err != nil {
				return "", nil, err
			}

			v, _ := out["View"].(map[string]any)
			a := outText(v, resourceExplorerViewArn)
			return a, map[string]any{resourceExplorerViewArn: a}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := map[string]any{
				resourceExplorerViewArn:    r.Physical,
				resourceExplorerProperties: []any{},
				resourceExplorerFilters:    map[string]any{"FilterString": ""},
			}

			for _, k := range []string{resourceExplorerProperties, resourceExplorerFilters} {
				if p.has(k) {
					params[k] = p[k]
				}
			}

			if _, err := c.call(ctx, resourceExplorerService, "UpdateView", params); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, resourceExplorerTagger(r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(
				ctx,
				resourceExplorerService,
				"DeleteView",
				map[string]any{resourceExplorerViewArn: r.Physical},
			)
			if gone(err, "ResourceNotFoundException") {
				return nil
			}

			return err
		},
	})

	register("AWS::ResourceExplorer2::DefaultViewAssociation", &resourceType{
		properties: []string{resourceExplorerViewArn}, replace: []string{resourceExplorerViewArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			_, err := c.call(
				ctx,
				resourceExplorerService,
				"AssociateDefaultView",
				map[string]any{resourceExplorerViewArn: p[resourceExplorerViewArn]},
			)
			return p.str(resourceExplorerViewArn), nil, err
		},
		remove: func(ctx context.Context, c *client, _ *resource) error {
			_, err := c.call(ctx, resourceExplorerService, "DisassociateDefaultView", nil)
			return err
		},
	})
}
