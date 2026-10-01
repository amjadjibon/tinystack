package cloudformation

import (
	"context"
)

var (
	// httpAPIProperties are the AWS::ApiGatewayV2::Api properties CreateApi
	// and UpdateApi take by the same names, but for ProtocolType, which only
	// CreateApi takes.
	httpAPIProperties = []string{
		memberName, memberDescription, "CorsConfiguration", "RouteSelectionExpression", "ApiKeySelectionExpression",
		memberDisableExecuteAPIEndpoint, memberVersion, "Target", "RouteKey", "IpAddressType",
	}
	httpIntegrationProperties = []string{
		"IntegrationType", "IntegrationUri", "IntegrationMethod", "PayloadFormatVersion", "TimeoutInMillis",
		"ConnectionType", memberDescription,
	}
	httpRouteProperties = []string{
		"RouteKey", "Target", "AuthorizationType", memberAuthorizerID, "AuthorizationScopes", "ApiKeyRequired",
		"OperationName",
	}
	httpStageProperties = []string{
		"AutoDeploy", memberDeploymentID, memberDescription, "StageVariables", "AccessLogSettings",
		"DefaultRouteSettings", "RouteSettings",
	}
	httpAuthorizerProperties = []string{
		memberName, "AuthorizerType", memberAuthorizerURI, memberIdentitySource, "JwtConfiguration",
		"AuthorizerPayloadFormatVersion", memberAuthorizerResultTTL, "EnableSimpleResponses",
	}
)

func init() {
	register("AWS::ApiGatewayV2::Api", &resourceType{
		properties: append([]string{memberProtocolType, memberTags}, httpAPIProperties...),
		attributes: []string{memberAPIID, "ApiEndpoint"},
		replace:    []string{memberProtocolType},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, append([]string{memberProtocolType}, httpAPIProperties...)...)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			out, err := c.call(ctx, "apigatewayv2", "CreateApi", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, memberAPIID)
			return id, map[string]any{memberAPIID: id, "ApiEndpoint": outText(out, "ApiEndpoint")}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			target := map[string]any{memberAPIID: r.Physical}
			if _, err := c.call(ctx, "apigatewayv2", "UpdateApi", pick(target, p, httpAPIProperties...)); err != nil {
				return nil, err
			}

			if p.changed(r.Properties, "CorsConfiguration") && !p.has("CorsConfiguration") {
				_, corsErr := c.call(
					ctx,
					"apigatewayv2",
					"DeleteCorsConfiguration",
					map[string]any{memberAPIID: r.Physical},
				)
				if err := ignoreNotFound(corsErr); err != nil {
					return nil, err
				}
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, restTagger(c, "/apis/"+r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigatewayv2", "DeleteApi", map[string]any{memberAPIID: r.Physical})
			return ignoreNotFound(err)
		},
	})
	registerHTTPChild("AWS::ApiGatewayV2::Integration", "Integration", "IntegrationId", httpIntegrationProperties)
	registerHTTPChild("AWS::ApiGatewayV2::Route", "Route", "RouteId", httpRouteProperties)
	registerHTTPChild("AWS::ApiGatewayV2::Authorizer", "Authorizer", memberAuthorizerID, httpAuthorizerProperties)
	register("AWS::ApiGatewayV2::Deployment", &resourceType{
		properties: []string{memberAPIID, memberDescription, memberStageName},
		attributes: []string{memberDeploymentID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "apigatewayv2", "CreateDeployment",
				pick(map[string]any{}, p, memberAPIID, memberDescription, memberStageName))
			if err != nil {
				return "", nil, err
			}

			id := outText(out, memberDeploymentID)
			return id, map[string]any{memberDeploymentID: id}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigatewayv2", "DeleteDeployment", map[string]any{
				memberAPIID: props(r.Properties).str(memberAPIID), memberDeploymentID: r.Physical,
			})
			return ignoreNotFound(err)
		},
	})
	register("AWS::ApiGatewayV2::Stage", &resourceType{
		name:       memberStageName,
		properties: append([]string{memberAPIID, memberStageName, memberTags}, httpStageProperties...),
		replace:    []string{memberAPIID, memberStageName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberStageName, 128)
			params := pick(
				map[string]any{memberStageName: name},
				p,
				append([]string{memberAPIID}, httpStageProperties...)...)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			_, err := c.call(ctx, "apigatewayv2", "CreateStage", params)
			return name, nil, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			api := p.str(memberAPIID)
			params := pick(map[string]any{memberAPIID: api, memberStageName: r.Physical}, p, httpStageProperties...)
			if _, err := c.call(ctx, "apigatewayv2", "UpdateStage", params); err != nil {
				return nil, err
			}

			return nil, retag(ctx, c, r.Properties, p, restTagger(c, "/apis/"+api+"/stages/"+r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigatewayv2", "DeleteStage", map[string]any{
				memberAPIID: props(r.Properties).str(memberAPIID), memberStageName: r.Physical,
			})
			return ignoreNotFound(err)
		},
	})
}

// registerHTTPChild registers an HTTP API's integration, route, or
// authorizer type, whose Create<kind>, Update<kind>, and Delete<kind>
// operations take the API's ID, the ID member id, and properties by the
// same names.
func registerHTTPChild(name, kind, id string, properties []string) {
	register(name, &resourceType{
		properties: append([]string{memberAPIID}, properties...),
		attributes: []string{id},
		replace:    []string{memberAPIID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "apigatewayv2", "Create"+kind,
				pick(map[string]any{}, p, append([]string{memberAPIID}, properties...)...))
			if err != nil {
				return "", nil, err
			}

			physical := outText(out, id)
			return physical, map[string]any{id: physical}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberAPIID: p.str(memberAPIID), id: r.Physical}, p, properties...)
			_, err := c.call(ctx, "apigatewayv2", "Update"+kind, params)
			return r.Attributes, err
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigatewayv2", "Delete"+kind, map[string]any{
				memberAPIID: props(r.Properties).str(memberAPIID), id: r.Physical,
			})
			return ignoreNotFound(err)
		},
	})
}
