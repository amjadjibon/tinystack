package apigatewayv2

import (
	"context"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/openapi"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const openAPIType = "type"

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
	} {
		if _, ok := d.Root[key]; ok {
			return nil, badRequest("HTTP APIs do not support %s", key)
		}
	}

	if d.Root["swagger"] != nil {
		return nil, badRequest("HTTP APIs require OpenAPI 3.0")
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

		return store.Update(
			ctx,
			func(tx storage.WriteTx) error { return storage.Write(tx, apiKey(m.Scope, *out.ApiId), out) },
		)
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
	for _, name := range slices.Sorted(maps.Keys(d.Schemes)) {
		scheme := openapi.Map(d.Schemes[name])
		ext := openapi.Map(scheme["x-amazon-apigateway-authorizer"])
		if ext == nil {
			return nil, badRequest("Unsupported security scheme %s", name)
		}

		if decodeErr := openapi.Check(
			ext,
			openAPIType,
			"identitySource",
			"jwtConfiguration",
			"authorizerUri",
			"authorizerPayloadFormatVersion",
			"authorizerResultTtlInSeconds",
			"enableSimpleResponses",
		); decodeErr != nil {
			return nil, decodeErr
		}

		ext = maps.Clone(ext)
		if source, ok := ext["identitySource"].(string); ok {
			ext["identitySource"] = []string{source}
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

		route := &api.CreateRouteRequest{
			ApiId:             id,
			RouteKey:          new(op.Method + " " + op.Path),
			AuthorizationType: new("NONE"),
		}

		security, err := d.Security(op)
		if err != nil {
			return nil, err
		}

		for _, requirement := range security {
			for name, scopes := range requirement {
				author := authors[name]
				if author == nil {
					return nil, badRequest("Unknown security scheme %s", name)
				}

				route.AuthorizerId = author.AuthorizerId
				route.AuthorizationType = author.AuthorizerType
				if common.Value(route.AuthorizationType) == "REQUEST" {
					route.AuthorizationType = new("CUSTOM")
				}

				if decodeErr := openapi.Decode(scopes, &route.AuthorizationScopes); decodeErr != nil {
					return nil, decodeErr
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
				"payloadFormatVersion",
				"connectionType",
				"timeoutInMillis",
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
	if common.Value(in.IncludeExtensions) && a.CorsConfiguration != nil {
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
	if common.Value(in.IncludeExtensions) {
		schemes := openapi.Object{}
		for _, author := range snapshotData.Authorizers {
			name := common.Value(author.Name)
			names[common.Value(author.AuthorizerId)] = name
			schemes[name] = openapi.Object{
				openAPIType: "apiKey", "name": "Authorization", "in": "header",
				"x-amazon-apigateway-authorizer": openapi.Object{
					openAPIType:                      strings.ToLower(common.Value(author.AuthorizerType)),
					"identitySource":                 author.IdentitySource,
					"authorizerUri":                  author.AuthorizerUri,
					"jwtConfiguration":               author.JwtConfiguration,
					"authorizerPayloadFormatVersion": author.AuthorizerPayloadFormatVersion,
					"authorizerResultTtlInSeconds":   author.AuthorizerResultTtlInSeconds,
					"enableSimpleResponses":          author.EnableSimpleResponses,
				},
			}
		}

		root["components"] = openapi.Object{"securitySchemes": schemes}
	}

	for _, r := range snapshotData.Routes {
		method, path, ok := strings.Cut(common.Value(r.RouteKey), " ")
		if !ok {
			return nil, badRequest("Cannot export route %s", common.Value(r.RouteKey))
		}

		op := openapi.Object{"responses": openapi.Object{"200": openapi.Object{"description": "Success"}}}
		if name := names[common.Value(r.AuthorizerId)]; name != "" {
			op["security"] = []openapi.Object{{name: r.AuthorizationScopes}}
		}

		if common.Value(in.IncludeExtensions) && r.Target != nil {
			i, ok := snapshotData.Integrations[integrationID(*r.Target)]
			if !ok {
				return nil, badRequest("Integration not found")
			}

			op["x-amazon-apigateway-integration"] = openapi.Object{
				openAPIType:            strings.ToLower(common.Value(i.IntegrationType)),
				"uri":                  i.IntegrationUri,
				"httpMethod":           i.IntegrationMethod,
				"payloadFormatVersion": i.PayloadFormatVersion,
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
