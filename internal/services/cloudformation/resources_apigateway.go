package cloudformation

import (
	"context"
	"maps"
	"slices"
	"strings"
)

// patchField maps a property to the path of the API Gateway member a patch
// operation changes.
type patchField struct{ property, path string }

var (
	restAPIFields = []patchField{
		{memberName, "/name"},
		{memberDescription, "/description"},
		{propertyBinaryMediaTypes, "/binaryMediaTypes"},
		{propertyMinimumCompressionSize, "/minimumCompressionSize"},
		{"ApiKeySourceType", "/apiKeySource"},
		{memberPolicy, "/policy"},
		{memberDisableExecuteAPIEndpoint, "/disableExecuteApiEndpoint"},
	}
	stageFields = []patchField{
		{memberDescription, "/description"},
		{memberDeploymentID, "/deploymentId"},
		{memberVariables, "/variables"},
		{"TracingEnabled", "/tracingEnabled"},
		{"CacheClusterEnabled", "/cacheClusterEnabled"},
		{"CacheClusterSize", "/cacheClusterSize"},
	}
	authorizerFields = []patchField{
		{memberName, "/name"},
		{memberType, "/type"},
		{memberAuthorizerURI, "/authorizerUri"},
		{memberIdentitySource, "/identitySource"},
		{"IdentityValidationExpression", "/identityValidationExpression"},
		{memberAuthorizerResultTTL, "/authorizerResultTtlInSeconds"},
		{"ProviderARNs", "/providerARNs"},
		{"AuthType", "/authType"},
		{"AuthorizerCredentials", "/authorizerCredentials"},
	}
	// methodProperties are the AWS::ApiGateway::Method properties PutMethod
	// takes by the same names.
	methodProperties = []string{
		propertyAuthorizationType,
		"AuthorizerId",
		"ApiKeyRequired",
		"OperationName",
		"RequestParameters",
		"RequestModels",
		"AuthorizationScopes",
	}
	// integrationProperties are the members of a method's Integration that
	// PutIntegration takes by the same names.
	integrationProperties = []string{
		memberType, "IntegrationHttpMethod", "Uri", "ConnectionType", "Credentials", "RequestParameters",
		"RequestTemplates", "PassthroughBehavior", "CacheNamespace", "CacheKeyParameters", "ContentHandling",
		propertyTimeoutInMillis,
	}
)

func init() {
	register("AWS::ApiGateway::RestApi", &resourceType{
		properties: []string{
			memberName, memberDescription, propertyBinaryMediaTypes, propertyMinimumCompressionSize, "ApiKeySourceType",
			propertyEndpointConfiguration, memberPolicy, memberDisableExecuteAPIEndpoint, memberTags,
		},
		attributes: []string{memberRestAPIID, "RootResourceId"},
		replace:    []string{propertyEndpointConfiguration},
		create:     createRestAPI,
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if err := patchRest(ctx, c, "UpdateRestApi", map[string]any{memberRestAPIID: r.Physical},
				patches(r.Properties, p, restAPIFields)); err != nil {
				return nil, err
			}

			return r.Attributes, retag(ctx, c, r.Properties, p, restTagger(c, "/restapis/"+r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigateway", "DeleteRestApi", map[string]any{memberRestAPIID: r.Physical})
			return ignoreNotFound(err)
		},
	})
	register("AWS::ApiGateway::Resource", &resourceType{
		properties: []string{memberRestAPIID, propertyParentID, propertyPathPart},
		attributes: []string{memberResourceID},
		replace:    []string{memberRestAPIID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "apigateway", "CreateResource",
				pick(map[string]any{}, p, memberRestAPIID, propertyParentID, propertyPathPart))
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Id")
			return id, map[string]any{memberResourceID: id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, patchRest(ctx, c, "UpdateResource", map[string]any{
				memberRestAPIID: p.str(memberRestAPIID), memberResourceID: r.Physical,
			}, patches(r.Properties, p, []patchField{{propertyParentID, "/parentId"}, {propertyPathPart, "/pathPart"}}))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigateway", "DeleteResource", map[string]any{
				memberRestAPIID: props(r.Properties).str(memberRestAPIID), memberResourceID: r.Physical,
			})
			return ignoreNotFound(err)
		},
	})
	register("AWS::ApiGateway::Method", &resourceType{
		properties: append([]string{
			memberRestAPIID, memberResourceID, memberHTTPMethod, "Integration", "MethodResponses",
		}, methodProperties...),
		replace: []string{memberRestAPIID, memberResourceID, memberHTTPMethod},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			return c.generate(128), nil, putMethod(ctx, c, p)
		},
		// A changed method is put again, with its integration and responses.
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if _, err := c.call(ctx, "apigateway", "DeleteMethod", methodTarget(r.Properties)); err != nil {
				return nil, err
			}

			return nil, putMethod(ctx, c, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigateway", "DeleteMethod", methodTarget(r.Properties))
			return ignoreNotFound(err)
		},
	})
	register("AWS::ApiGateway::Deployment", &resourceType{
		properties: []string{memberRestAPIID, memberDescription, memberStageName},
		attributes: []string{memberDeploymentID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			out, err := c.call(ctx, "apigateway", "CreateDeployment",
				pick(map[string]any{}, p, memberRestAPIID, memberDescription, memberStageName))
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Id")
			return id, map[string]any{memberDeploymentID: id}, nil
		},
		remove: removeDeployment,
	})
	register("AWS::ApiGateway::Stage", &resourceType{
		name: memberStageName,
		properties: []string{
			memberRestAPIID, memberStageName, memberDeploymentID, memberDescription, memberVariables, "TracingEnabled",
			"CacheClusterEnabled", "CacheClusterSize", memberTags,
		},
		replace: []string{memberRestAPIID, memberStageName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberStageName, 128)
			params := pick(map[string]any{memberStageName: name}, p, memberRestAPIID, memberDeploymentID,
				memberDescription, memberVariables, "TracingEnabled", "CacheClusterEnabled", "CacheClusterSize")
			if tags := p.tags(); len(tags) > 0 {
				params[memberTags] = tagMap(tags)
			}

			_, err := c.call(ctx, "apigateway", "CreateStage", params)
			return name, nil, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			api := p.str(memberRestAPIID)
			if err := patchRest(ctx, c, "UpdateStage", map[string]any{
				memberRestAPIID: api, memberStageName: r.Physical,
			}, patches(r.Properties, p, stageFields)); err != nil {
				return nil, err
			}

			return nil, retag(ctx, c, r.Properties, p, restTagger(c, "/restapis/"+api+"/stages/"+r.Physical))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigateway", "DeleteStage", map[string]any{
				memberRestAPIID: props(r.Properties).str(memberRestAPIID), memberStageName: r.Physical,
			})
			return ignoreNotFound(err)
		},
	})
	register("AWS::ApiGateway::Authorizer", &resourceType{
		properties: []string{
			memberRestAPIID, memberName, memberType, memberAuthorizerURI, "AuthorizerCredentials",
			memberAuthorizerResultTTL, memberIdentitySource, "IdentityValidationExpression", "ProviderARNs",
			"AuthType",
		},
		attributes: []string{memberAuthorizerID},
		replace:    []string{memberRestAPIID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberRestAPIID, memberName, memberType, memberAuthorizerURI,
				"AuthorizerCredentials", memberAuthorizerResultTTL, memberIdentitySource,
				"IdentityValidationExpression", "ProviderARNs", "AuthType")
			out, err := c.call(ctx, "apigateway", "CreateAuthorizer", params)
			if err != nil {
				return "", nil, err
			}

			id := outText(out, "Id")
			return id, map[string]any{memberAuthorizerID: id}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, patchRest(ctx, c, "UpdateAuthorizer", map[string]any{
				memberRestAPIID: p.str(memberRestAPIID), memberAuthorizerID: r.Physical,
			}, patches(r.Properties, p, authorizerFields))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "apigateway", "DeleteAuthorizer", map[string]any{
				memberRestAPIID: props(r.Properties).str(memberRestAPIID), memberAuthorizerID: r.Physical,
			})
			return ignoreNotFound(err)
		},
	})
}

func ignoreNotFound(err error) error {
	if gone(err, "NotFoundException") {
		return nil
	}

	return err
}

// restTagger tags the API Gateway resource at path, which starts with
// /restapis/ or /apis/.
func restTagger(c *client, path string) *tagger {
	service := "apigateway"
	if strings.HasPrefix(path, "/apis/") {
		service = "apigatewayv2"
	}

	return &tagger{
		target:  map[string]any{memberResourceArn: "arn:aws:apigateway:" + c.scope.Region + "::" + path},
		service: service, tag: opTagResource, untag: opUntagResource,
	}
}

func createRestAPI(ctx context.Context, c *client, p props) (physical string, attributes map[string]any, err error) {
	params := pick(
		map[string]any{},
		p,
		memberName,
		memberDescription,
		propertyBinaryMediaTypes,
		propertyMinimumCompressionSize,
		propertyEndpointConfiguration,
		memberDisableExecuteAPIEndpoint,
	)
	if p.has("ApiKeySourceType") {
		params["ApiKeySource"] = p.str("ApiKeySourceType")
	}

	if p.has(memberPolicy) {
		params[memberPolicy] = p.document(memberPolicy)
	}

	if tags := p.tags(); len(tags) > 0 {
		params[memberTags] = tagMap(tags)
	}

	out, err := c.call(ctx, "apigateway", "CreateRestApi", params)
	if err != nil {
		return "", nil, err
	}

	id := outText(out, "Id")
	return id, map[string]any{memberRestAPIID: id, "RootResourceId": outText(out, "RootResourceId")}, nil
}

// escapePatch escapes a patch path segment ("~" is "~0" and "/" is "~1").
func escapePatch(s string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(s) }

// patches returns the patch operations that change fields from old's values
// to p's: scalars are replaced or removed, and list elements and map entries
// are added and removed one by one.
func patches(old, p props, fields []patchField) []any {
	var ops []any
	op := func(name, path string, value any) {
		o := map[string]any{"Op": name, "Path": path}
		if value != nil {
			o["Value"] = value
		}

		ops = append(ops, o)
	}

	for _, f := range fields {
		if !p.changed(old, f.property) {
			continue
		}

		before, after := old[f.property], p[f.property]
		_, isList := after.([]any)
		_, wasList := before.([]any)
		_, isMap := after.(map[string]any)
		_, wasMap := before.(map[string]any)
		switch {
		case (isList || wasList) && f.property != memberPolicy:
			was, is := old.strings(f.property), p.strings(f.property)
			for _, v := range was {
				if !slices.Contains(is, v) {
					op("remove", f.path+"/"+escapePatch(v), nil)
				}
			}

			for _, v := range is {
				if !slices.Contains(was, v) {
					op("add", f.path+"/"+escapePatch(v), nil)
				}
			}
		case (isMap || wasMap) && f.property != memberPolicy:
			was, is := props(asObject(before)), props(asObject(after))
			for _, k := range slices.Sorted(maps.Keys(was)) {
				if !is.has(k) {
					op("remove", f.path+"/"+escapePatch(k), nil)
				}
			}

			for _, k := range slices.Sorted(maps.Keys(is)) {
				if is.changed(was, k) {
					op("add", f.path+"/"+escapePatch(k), is.str(k))
				}
			}
		case !p.has(f.property):
			op("remove", f.path, nil)
		case f.property == memberPolicy:
			op("replace", f.path, p.document(f.property))
		default:
			op("replace", f.path, p.str(f.property))
		}
	}

	return ops
}

func asObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// patchRest runs an API Gateway update operation with ops, if there are any.
func patchRest(ctx context.Context, c *client, operation string, target map[string]any, ops []any) error {
	if len(ops) == 0 {
		return nil
	}

	target["PatchOperations"] = ops
	_, err := c.call(ctx, "apigateway", operation, target)
	return err
}

func methodTarget(p props) map[string]any {
	return pick(map[string]any{}, p, memberRestAPIID, memberResourceID, memberHTTPMethod)
}

// putMethod puts a method with its responses and integration.
func putMethod(ctx context.Context, c *client, p props) error {
	target := methodTarget(p)
	params := pick(maps.Clone(target), p, methodProperties...)
	if _, err := c.call(ctx, "apigateway", "PutMethod", params); err != nil {
		return err
	}

	for _, el := range p.list("MethodResponses") {
		params = pick(maps.Clone(target), props(asObject(el)), memberStatusCode, "ResponseModels",
			"ResponseParameters")
		if _, err := c.call(ctx, "apigateway", "PutMethodResponse", params); err != nil {
			return err
		}
	}

	integration, ok := p["Integration"].(map[string]any)
	if !ok {
		return nil
	}

	for k := range integration {
		if k != "IntegrationResponses" && !slices.Contains(integrationProperties, k) {
			return validation("TinyStack does not support the %s property of a method's Integration", k)
		}
	}

	params = pick(maps.Clone(target), integration, integrationProperties...)
	if _, err := c.call(ctx, "apigateway", "PutIntegration", params); err != nil {
		return err
	}

	for _, el := range props(integration).list("IntegrationResponses") {
		params = pick(maps.Clone(target), props(asObject(el)), memberStatusCode, "SelectionPattern",
			"ResponseParameters", "ResponseTemplates", "ContentHandling")
		if _, err := c.call(ctx, "apigateway", "PutIntegrationResponse", params); err != nil {
			return err
		}
	}

	return nil
}

// removeDeployment deletes a deployment, and the stage it created when that
// stage still serves it, as CloudFormation does.
func removeDeployment(ctx context.Context, c *client, r *resource) error {
	p := props(r.Properties)
	api := p.str(memberRestAPIID)
	if stage := p.str(memberStageName); stage != "" {
		target := map[string]any{memberRestAPIID: api, memberStageName: stage}
		out, err := c.call(ctx, "apigateway", "GetStage", target)
		switch {
		case gone(err, "NotFoundException"):
		case err != nil:
			return err
		case outText(out, memberDeploymentID) == r.Physical:
			if _, err = c.call(ctx, "apigateway", "DeleteStage", target); err != nil {
				return ignoreNotFound(err)
			}
		}
	}

	_, err := c.call(ctx, "apigateway", "DeleteDeployment", map[string]any{
		memberRestAPIID: api, memberDeploymentID: r.Physical,
	})
	return ignoreNotFound(err)
}
