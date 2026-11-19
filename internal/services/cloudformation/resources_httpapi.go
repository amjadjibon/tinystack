package cloudformation

import (
	"context"
)

const (
	httpIntegrationKind      = "Integration"
	httpResponseParameters   = "ResponseParameters"
	httpRequestParameters    = "RequestParameters"
	httpDefaultRouteSettings = "DefaultRouteSettings"
	httpRouteSettings        = "RouteSettings"
)

var (
	// httpAPIProperties are the AWS::ApiGatewayV2::Api properties CreateApi
	// and UpdateApi take by the same names, but for ProtocolType, which only
	// CreateApi takes.
	httpAPIProperties = []string{
		memberName, memberDescription, "CorsConfiguration", "RouteSelectionExpression", "ApiKeySelectionExpression",
		memberDisableExecuteAPIEndpoint, memberVersion, propertyTarget, propertyRouteKey, "IpAddressType",
	}
	httpIntegrationProperties = []string{
		"IntegrationType", "IntegrationUri", "IntegrationMethod", propertyPayloadFormatVersion, propertyTimeoutInMillis,
		"ConnectionType", memberDescription, httpRequestParameters, httpResponseParameters,
	}
	httpRouteProperties = []string{
		propertyRouteKey,
		propertyTarget,
		propertyAuthorizationType,
		memberAuthorizerID,
		"AuthorizationScopes",
		"ApiKeyRequired",
		"OperationName",
	}
	httpStageProperties = []string{
		"AutoDeploy", memberDeploymentID, memberDescription, "StageVariables", "AccessLogSettings",
		httpDefaultRouteSettings, httpRouteSettings,
	}
	httpAuthorizerProperties = []string{
		memberName, "AuthorizerType", memberAuthorizerURI, memberIdentitySource, "JwtConfiguration",
		"AuthorizerPayloadFormatVersion", memberAuthorizerResultTTL, "EnableSimpleResponses",
	}
)

func init() {
	register("AWS::ApiGatewayV2::Api", &resourceType{
		properties: append(
			[]string{memberProtocolType, memberTags, apiBody, "BodyS3Location", apiFailOnWarnings},
			httpAPIProperties...),
		attributes: []string{memberAPIID, "ApiEndpoint"},
		replace:    []string{memberProtocolType},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, append([]string{memberProtocolType}, httpAPIProperties...)...)
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			operation := "CreateApi"
			if p.has(apiBody) || p.has("BodyS3Location") {
				body, bodyErr := apiDefinition(ctx, c, p)
				if bodyErr != nil {
					return "", nil, bodyErr
				}

				operation = "ImportApi"
				params = map[string]any{apiBody: body, apiFailOnWarnings: p[apiFailOnWarnings]}
			}

			out, err := c.call(ctx, "apigatewayv2", operation, params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, memberAPIID)
			return id, map[string]any{memberAPIID: id, "ApiEndpoint": outText(out, "ApiEndpoint")}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if p.has(apiBody) || p.has("BodyS3Location") {
				body, err := apiDefinition(ctx, c, p)
				if err != nil {
					return nil, err
				}

				_, err = c.call(ctx, "apigatewayv2", "ReimportApi", map[string]any{
					"ApiId": r.Physical,
					apiBody: body, apiFailOnWarnings: p[apiFailOnWarnings],
				})
				if err != nil {
					return nil, err
				}
			}

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
	registerHTTPChild("AWS::ApiGatewayV2::Integration", httpIntegrationKind, "IntegrationId", httpIntegrationProperties)
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
			for _, property := range []string{httpDefaultRouteSettings, httpRouteSettings} {
				if !p.has(property) && props(r.Properties).has(property) {
					params[property] = map[string]any{}
				}
			}

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
			params := pick(map[string]any{}, p, append([]string{memberAPIID}, properties...)...)
			if kind == httpIntegrationKind {
				if err := httpIntegrationResponses(params); err != nil {
					return "", nil, err
				}
			}

			out, err := c.call(ctx, "apigatewayv2", "Create"+kind, params)
			if err != nil {
				return "", nil, err
			}

			physical := outText(out, id)
			return physical, map[string]any{id: physical}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params := pick(map[string]any{memberAPIID: p.str(memberAPIID), id: r.Physical}, p, properties...)
			if kind == httpIntegrationKind {
				if err := httpIntegrationResponses(params); err != nil {
					return nil, err
				}

				for _, property := range []string{httpRequestParameters, httpResponseParameters} {
					if !p.has(property) && props(r.Properties).has(property) {
						params[property] = map[string]any{}
					}
				}
			}

			if kind == "Authorizer" && !p.has(memberAuthorizerResultTTL) &&
				props(r.Properties).has(memberAuthorizerResultTTL) {
				params[memberAuthorizerResultTTL] = float64(0)
			}

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

// httpIntegrationResponses converts CloudFormation's list of destinations and sources
// per status code to the service's nested map.
func httpIntegrationResponses(params map[string]any) error {
	raw, present := params[httpResponseParameters]
	if !present {
		return nil
	}

	statuses, ok := raw.(map[string]any)
	if !ok {
		return validation("ResponseParameters must be an object keyed by HTTP status code")
	}

	responses := make(map[string]any, len(statuses))
	for status, value := range statuses {
		group, ok := value.(map[string]any)
		if !ok || len(group) != 1 {
			return validation("ResponseParameters entry must contain a ResponseParameters list")
		}

		parameters, ok := group[httpResponseParameters].([]any)
		if !ok {
			return validation("ResponseParameters entry must contain a ResponseParameters list")
		}

		rules := make(map[string]any, len(parameters))
		for _, rawRule := range parameters {
			rule, _ := rawRule.(map[string]any)
			destination, ok := rule["Destination"].(string)
			if !ok || destination == "" {
				return validation("ResponseParameters requires a Destination string")
			}

			source, ok := rule["Source"].(string)
			if _, present := rule["Source"]; (present && !ok) || (len(rule) > 1 && !present) || len(rule) > 2 {
				return validation("ResponseParameters accepts only Destination and Source strings")
			}

			if _, duplicate := rules[destination]; duplicate {
				return validation("ResponseParameters contains a duplicate Destination")
			}

			rules[destination] = source
		}

		responses[status] = rules
	}

	params[httpResponseParameters] = responses
	return nil
}
