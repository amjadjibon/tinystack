package apigatewayv2

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/openapi"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	openAPIType           = "type"
	defaultOpenAPIPath    = "/$default"
	openAPIAuthType       = "x-amazon-apigateway-authtype"
	openAPISigV4          = "awsSigv4"
	openAPIPayloadFormat  = "payloadFormatVersion"
	openAPIIdentitySource = "identitySource"
	openAPITTL            = "authorizerResultTtlInSeconds"
	openAPITimeout        = "timeoutInMillis"
	openAPIConnectionType = "connectionType"
)

func (s *Service) ImportApi(ctx context.Context, in *api.ImportApiRequest) (*api.ImportApiResponse, error) {
	a, err := s.importDefinition(ctx, nil, common.Value(in.Body), in.Basepath, common.Value(in.FailOnWarnings))
	if err != nil {
		return nil, err
	}

	var out api.ImportApiResponse
	err = openapi.Decode(a, &out)
	return &out, err
}

func (s *Service) ReimportApi(ctx context.Context, in *api.ReimportApiRequest) (*api.ReimportApiResponse, error) {
	a, err := s.importDefinition(ctx, in.ApiId, common.Value(in.Body), in.Basepath, common.Value(in.FailOnWarnings))
	if err != nil {
		return nil, err
	}

	var out api.ReimportApiResponse
	err = openapi.Decode(a, &out)
	return &out, err
}

func (s *Service) importDefinition(
	ctx context.Context,
	id *string,
	body string,
	basepath *string,
	failOnWarnings bool,
) (*api.Api, error) {
	if basepath != nil && *basepath != "ignore" {
		return nil, badRequest("Only Basepath=ignore is supported")
	}

	d, err := openapi.Parse([]byte(body))
	if err != nil {
		return nil, err
	}

	for _, key := range []string{
		"x-amazon-apigateway-api-key-source", "x-amazon-apigateway-binary-media-types",
		"x-amazon-apigateway-endpoint-configuration", "x-amazon-apigateway-policy",
		"x-amazon-apigateway-request-validators", "x-amazon-apigateway-request-validator",
	} {
		if _, ok := d.Root[key]; ok {
			return nil, badRequest("HTTP APIs do not support %s", key)
		}
	}

	if d.Root["swagger"] != nil {
		return nil, badRequest("HTTP APIs require OpenAPI 3.0")
	}

	if d.Schemas != nil {
		return nil, badRequest("HTTP APIs do not support models")
	}

	if failOnWarnings && len(d.Warnings) > 0 {
		return nil, badRequest("Import warnings: %s", strings.Join(d.Warnings, "; "))
	}

	var out *api.Api
	err = storage.Atomic(ctx, s.store, func(store storage.Store) error {
		staged := New(store, s.functions, s.logs)
		staged.now, staged.authorizer, staged.metrics = s.now, s.authorizer, s.metrics
		var applyErr error
		out, applyErr = staged.applyDefinition(ctx, id, d)
		if applyErr != nil {
			return applyErr
		}

		out.Warnings = d.Warnings
		m, scopeErr := request.Regional(ctx)
		if scopeErr != nil {
			return scopeErr
		}

		return store.Update(ctx, func(tx storage.WriteTx) error {
			if writeErr := storage.Write(tx, apiKey(m.Scope, *out.ApiId), out); writeErr != nil {
				return writeErr
			}

			// An empty reimport has no child write to deploy the cleared API.
			if id != nil && len(d.Operations) == 0 && len(d.Schemes) == 0 {
				return staged.deploy(tx, m.Scope, *id, staged.now().UTC())
			}

			return nil
		})
	})
	return out, err
}

func (s *Service) applyDefinition(ctx context.Context, id *string, d *openapi.Document) (*api.Api, error) {
	info := openapi.Map(d.Root["info"])
	if err := openapi.Check(openapi.Map(d.Root["x-amazon-apigateway-cors"]), "allowCredentials", "allowHeaders",
		"allowMethods", "allowOrigins", "exposeHeaders", "maxAge"); err != nil {
		return nil, err
	}

	var cors *api.Cors
	if decodeErr := openapi.Decode(d.Root["x-amazon-apigateway-cors"], &cors); decodeErr != nil {
		return nil, decodeErr
	}

	if id == nil {
		a, err := s.CreateApi(ctx, &api.CreateApiRequest{
			Name:         new(openapi.Text(info["title"])),
			ProtocolType: new(protocolHTTP),
			Description: new(
				openapi.Text(info["description"]),
			),
			Version:           new(openapi.Text(info["version"])),
			CorsConfiguration: cors,
		})
		if err != nil {
			return nil, err
		}

		id = a.ApiId
	} else {
		m, err := request.Regional(ctx)
		if err != nil {
			return nil, err
		}

		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			a, loadErr := loadAPI(tx, m.Scope, id)
			if loadErr != nil {
				return loadErr
			}

			if common.Value(a.ProtocolType) != protocolHTTP {
				return badRequest("Only HTTP APIs support reimport")
			}

			for _, kind := range []string{kindRoute, kindIntegration, kindAuthorizer} {
				for _, key := range tx.Keys(m.Prefix(service, kind) + *id + "/") {
					tx.Delete(key)
				}
			}

			a.CorsConfiguration = cors
			return storage.Write(tx, apiKey(m.Scope, *id), a)
		})
		if err != nil {
			return nil, err
		}

		if _, err = s.UpdateApi(ctx, &api.UpdateApiRequest{
			ApiId: id,
			Name:  new(openapi.Text(info["title"])),
			Description: new(
				openapi.Text(info["description"]),
			),
			Version:           new(openapi.Text(info["version"])),
			CorsConfiguration: cors,
		}); err != nil {
			return nil, err
		}
	}

	authors := map[string]*api.CreateAuthorizerResponse{}
	iamSchemes := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(d.Schemes)) {
		scheme := openapi.Map(d.Schemes[name])
		ext := openapi.Map(scheme["x-amazon-apigateway-authorizer"])
		if openapi.Text(scheme[openAPIAuthType]) == openAPISigV4 {
			if _, hasAuthorizer := scheme["x-amazon-apigateway-authorizer"]; hasAuthorizer {
				return nil, badRequest("IAM security scheme %s cannot include a custom authorizer", name)
			}

			iamSchemes[name] = true
			continue
		}

		if ext == nil {
			return nil, badRequest("Unsupported security scheme %s", name)
		}

		if decodeErr := openapi.Check(
			ext,
			openAPIType,
			openAPIIdentitySource,
			"jwtConfiguration",
			"authorizerUri",
			"authorizerPayloadFormatVersion",
			openAPITTL,
			"enableSimpleResponses",
		); decodeErr != nil {
			return nil, decodeErr
		}

		ext = maps.Clone(ext)
		if source, ok := ext[openAPIIdentitySource].(string); ok {
			sources := strings.Split(source, ",")
			for i, value := range sources {
				sources[i] = strings.TrimSpace(value)
			}

			ext[openAPIIdentitySource] = sources
		}

		if ttl, ok := ext[openAPITTL].(string); ok {
			value, err := strconv.ParseInt(ttl, 10, 64)
			if err != nil {
				return nil, badRequest("Invalid authorizerResultTtlInSeconds %q", ttl)
			}

			ext[openAPITTL] = value
		}

		in := &api.CreateAuthorizerRequest{ApiId: id, Name: new(name)}
		if decodeErr := openapi.Decode(ext, in); decodeErr != nil {
			return nil, decodeErr
		}

		in.AuthorizerType = new(strings.ToUpper(openapi.Text(ext[openAPIType])))
		author, err := s.CreateAuthorizer(ctx, in)
		if err != nil {
			return nil, err
		}

		authors[name] = author
	}

	for _, op := range d.Operations {
		if len(op.Parameters) > 0 {
			return nil, badRequest("HTTP API request parameters are unsupported")
		}

		if _, hasBody := op.Body["requestBody"]; hasBody || op.Body["x-amazon-apigateway-request-validator"] != nil {
			return nil, badRequest("HTTP APIs do not support request models or validators")
		}

		routeKey := op.Method + " " + op.Path
		if raw, present := op.Body["isDefaultRoute"]; present {
			isDefault, ok := raw.(bool)
			if !ok || op.Method != methodAny || (isDefault && op.Path != defaultOpenAPIPath) {
				return nil, badRequest("isDefaultRoute must be a boolean on the ANY operation for /$default")
			}

			if isDefault {
				routeKey = defaultRoute
			}
		}

		route := &api.CreateRouteRequest{
			ApiId: id, RouteKey: &routeKey, AuthorizationType: new(authNone),
		}
		if name := openapi.Text(op.Body["operationId"]); name != "" {
			route.OperationName = &name
		}

		security, err := d.Security(op)
		if err != nil {
			return nil, err
		}

		for _, requirement := range security {
			if len(requirement) > 1 {
				return nil, badRequest("HTTP API operations can have only one authorization scheme")
			}

			for name, scopes := range requirement {
				if decodeErr := openapi.Decode(scopes, &route.AuthorizationScopes); decodeErr != nil {
					return nil, decodeErr
				}

				if iamSchemes[name] {
					if len(route.AuthorizationScopes) > 0 {
						return nil, badRequest("IAM authorization does not accept authorization scopes")
					}

					route.AuthorizationType = new(authIAM)
					continue
				}

				author := authors[name]
				if author == nil {
					return nil, badRequest("Unknown security scheme %s", name)
				}

				route.AuthorizerId = author.AuthorizerId
				route.AuthorizationType = author.AuthorizerType
				if common.Value(route.AuthorizationType) == typeRequest {
					route.AuthorizationType = new(authCustom)
				}
			}
		}

		ext, err := d.Integration(op)
		if err != nil {
			return nil, err
		}

		if ext != nil {
			if checkErr := openapi.Check(
				ext,
				openAPIType,
				"uri",
				"httpMethod",
				openAPIPayloadFormat,
				openAPIConnectionType,
				openAPITimeout,
			); checkErr != nil {
				return nil, checkErr
			}

			integration := &api.CreateIntegrationRequest{ApiId: id}
			if decodeErr := openapi.Decode(ext, integration); decodeErr != nil {
				return nil, decodeErr
			}

			integration.IntegrationType = new(strings.ToUpper(openapi.Text(ext[openAPIType])))
			integration.IntegrationUri = new(openapi.Text(ext["uri"]))
			if v := openapi.Text(ext["httpMethod"]); v != "" {
				integration.IntegrationMethod = new(strings.ToUpper(v))
			}

			i, createErr := s.CreateIntegration(ctx, integration)
			if createErr != nil {
				return nil, createErr
			}

			route.Target = new("integrations/" + common.Value(i.IntegrationId))
		}

		if _, err = s.CreateRoute(ctx, route); err != nil {
			return nil, err
		}
	}

	a, err := s.GetApi(ctx, &api.GetApiRequest{ApiId: id})
	if err != nil {
		return nil, err
	}

	out := api.Api(*a)
	return &out, nil
}

func (s *Service) ExportApi(ctx context.Context, in *api.ExportApiRequest) (*api.ExportApiResponse, error) {
	if common.Value(in.Specification) != "OAS30" {
		return nil, badRequest("Specification must be OAS30")
	}

	if in.ExportVersion != nil && *in.ExportVersion != "1.0" {
		return nil, badRequest("ExportVersion must be 1.0")
	}

	a, err := s.GetApi(ctx, &api.GetApiRequest{ApiId: in.ApiId})
	if err != nil {
		return nil, err
	}

	if common.Value(a.ProtocolType) != protocolHTTP {
		return nil, badRequest("Only HTTP APIs support export")
	}

	root := openapi.New(common.Value(a.Name), common.Value(a.Version), false)
	if a.Description != nil {
		root["info"].(openapi.Object)["description"] = *a.Description
	}

	includeExtensions := in.IncludeExtensions == nil || *in.IncludeExtensions
	if includeExtensions && a.CorsConfiguration != nil {
		root["x-amazon-apigateway-cors"] = a.CorsConfiguration
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var snapshotData *deployment
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.StageName == nil {
			var snapshotErr error
			snapshotData, snapshotErr = snapshot(tx, m.Scope, *in.ApiId, s.now())
			return snapshotErr
		}

		stage, ok, readErr := storage.Read[api.Stage](tx, owned(m.Scope, kindStage, *in.ApiId, *in.StageName))
		if readErr != nil {
			return readErr
		}

		if !ok {
			return notFound("Stage not found")
		}

		snapshotData, ok, readErr = storage.Read[deployment](
			tx,
			owned(m.Scope, kindDeployment, *in.ApiId, common.Value(stage.DeploymentId)),
		)
		if readErr != nil {
			return readErr
		}

		if !ok {
			return notFound("Deployment not found")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	names := map[string]string{}
	schemes := openapi.Object{}
	if includeExtensions {
		for _, author := range snapshotData.Authorizers {
			name := common.Value(author.Name)
			var sources any
			if len(author.IdentitySource) > 0 {
				sources = strings.Join(author.IdentitySource, ", ")
			}
			names[common.Value(author.AuthorizerId)] = name
			schemes[name] = openapi.Object{
				openAPIType: "apiKey", "name": "Authorization", "in": "header",
				"x-amazon-apigateway-authorizer": openapi.Object{
					openAPIType:                      strings.ToLower(common.Value(author.AuthorizerType)),
					openAPIIdentitySource:            sources,
					"authorizerUri":                  author.AuthorizerUri,
					"jwtConfiguration":               author.JwtConfiguration,
					"authorizerPayloadFormatVersion": author.AuthorizerPayloadFormatVersion,
					openAPITTL:                       author.AuthorizerResultTtlInSeconds,
					"enableSimpleResponses":          author.EnableSimpleResponses,
				},
			}
		}

		root["components"] = openapi.Object{"securitySchemes": schemes}
	}

	iamSchemeName := openAPISigV4
	for suffix := 1; schemes[iamSchemeName] != nil; suffix++ {
		iamSchemeName = openAPISigV4 + strconv.Itoa(suffix)
	}

	for _, r := range snapshotData.Routes {
		op := openapi.Object{"responses": openapi.Object{"200": openapi.Object{"description": "Success"}}}
		method, path, ok := strings.Cut(common.Value(r.RouteKey), " ")
		if common.Value(r.RouteKey) == defaultRoute {
			method, path, ok = methodAny, defaultOpenAPIPath, true
			op["isDefaultRoute"] = true
		}

		if !ok {
			return nil, badRequest("Cannot export route %s", common.Value(r.RouteKey))
		}

		if r.OperationName != nil {
			op["operationId"] = *r.OperationName
		}

		if includeExtensions && common.Value(r.AuthorizationType) == authIAM {
			schemes[iamSchemeName] = openapi.Object{
				openAPIType: "apiKey", "name": "Authorization", "in": "header", openAPIAuthType: openAPISigV4,
			}
			op["security"] = []openapi.Object{{iamSchemeName: []string{}}}
		} else if name := names[common.Value(r.AuthorizerId)]; name != "" {
			scopes := append([]string{}, r.AuthorizationScopes...)
			op["security"] = []openapi.Object{{name: scopes}}
		}

		if includeExtensions && r.Target != nil {
			i, ok := snapshotData.Integrations[integrationID(*r.Target)]
			if !ok {
				return nil, badRequest("Integration not found")
			}

			op["x-amazon-apigateway-integration"] = openapi.Object{
				openAPIType:           strings.ToLower(common.Value(i.IntegrationType)),
				"uri":                 i.IntegrationUri,
				"httpMethod":          i.IntegrationMethod,
				openAPIPayloadFormat:  i.PayloadFormatVersion,
				openAPIConnectionType: i.ConnectionType,
				openAPITimeout:        i.TimeoutInMillis,
			}
		}

		openapi.Add(root, path, method, op)
	}

	body, _, err := openapi.Encode(root, common.Value(in.OutputType))
	if err != nil {
		return nil, err
	}

	return &api.ExportApiResponse{Body: body}, nil
}
