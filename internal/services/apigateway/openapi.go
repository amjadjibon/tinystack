package apigateway

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/openapi"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const openAPIType = "type"

const exportSwagger = "swagger"

const openAPIKey = "apiKey"

const openAPIHeader = "header"

const openAPIName = "name"

func (s *Service) ImportRestApi(ctx context.Context, in *api.ImportRestApiRequest) (*api.RestApi, error) {
	return s.importDefinition(ctx, nil, in.Body, "overwrite", in.Parameters, common.Value(in.FailOnWarnings))
}

func (s *Service) PutRestApi(ctx context.Context, in *api.PutRestApiRequest) (*api.RestApi, error) {
	return s.importDefinition(
		ctx,
		in.RestApiId,
		in.Body,
		common.Value(in.Mode),
		in.Parameters,
		common.Value(in.FailOnWarnings),
	)
}

func (s *Service) importDefinition(ctx context.Context, id *string, body []byte, mode string,
	parameters map[string]string, failOnWarnings bool,
) (*api.RestApi, error) {
	if mode != "" && mode != "merge" && mode != "overwrite" {
		return nil, badRequest("mode must be merge or overwrite")
	}

	if len(parameters) > 0 {
		return nil, badRequest("Import parameters are unsupported")
	}

	d, err := openapi.Parse(body)
	if err != nil {
		return nil, err
	}

	if failOnWarnings && len(d.Warnings) > 0 {
		return nil, badRequest("Import warnings: %s", strings.Join(d.Warnings, "; "))
	}

	if d.Root["x-amazon-apigateway-cors"] != nil {
		return nil, badRequest("REST API CORS must be defined with OPTIONS methods")
	}

	var out *api.RestApi
	err = storage.Atomic(ctx, s.store, func(store storage.Store) error {
		staged := *s
		staged.store = store
		var applyErr error
		out, applyErr = staged.applyDefinition(ctx, id, d, mode)
		if applyErr != nil {
			return applyErr
		}

		return staged.changeAPI(ctx, out.Id, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
			a.API.Warnings = d.Warnings
			out = &a.API
			return nil
		})
	})
	return out, err
}

func (s *Service) applyDefinition(
	ctx context.Context,
	id *string,
	d *openapi.Document,
	mode string,
) (*api.RestApi, error) {
	info := openapi.Map(d.Root["info"])
	create := &api.CreateRestApiRequest{
		Name:        new(openapi.Text(info["title"])),
		Description: new(openapi.Text(info["description"])), Version: new(openapi.Text(info["version"])),
	}

	for key, dest := range map[string]any{
		"x-amazon-apigateway-api-key-source":         &create.ApiKeySource,
		"x-amazon-apigateway-binary-media-types":     &create.BinaryMediaTypes,
		"x-amazon-apigateway-endpoint-configuration": &create.EndpointConfiguration,
	} {
		if key == "x-amazon-apigateway-endpoint-configuration" {
			if decodeErr := openapi.Check(openapi.Map(d.Root[key]), "types", "ipAddressType"); decodeErr != nil {
				return nil, decodeErr
			}
		}

		if v, ok := d.Root[key]; ok {
			if decodeErr := openapi.Decode(v, dest); decodeErr != nil {
				return nil, decodeErr
			}
		}
	}

	if v, ok := d.Root["x-amazon-apigateway-policy"]; ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}

		create.Policy = new(string(b))
	}

	var a *api.RestApi
	var err error
	if id == nil {
		a, err = s.CreateRestApi(ctx, create)
	} else {
		err = s.changeAPI(ctx, id, func(_ storage.WriteTx, scope request.Scope, record *restAPI) error {
			record.API.Name, record.API.Description, record.API.Version = create.Name, create.Description, create.Version
			if create.ApiKeySource != nil {
				record.API.ApiKeySource = create.ApiKeySource
			}

			if create.BinaryMediaTypes != nil {
				record.API.BinaryMediaTypes = create.BinaryMediaTypes
			}

			if create.EndpointConfiguration != nil {
				record.API.EndpointConfiguration = create.EndpointConfiguration
			}

			if create.Policy != nil {
				record.API.Policy = create.Policy
			}

			if mode == "overwrite" {
				record.API.BinaryMediaTypes, record.API.Policy = create.BinaryMediaTypes, create.Policy
				record.API.ApiKeySource = common.OrElse(create.ApiKeySource, new("HEADER"))
				record.API.EndpointConfiguration = common.OrElse(
					create.EndpointConfiguration,
					&api.EndpointConfiguration{
						Types: []string{endpointEdge}, IpAddressType: new("ipv4"),
					},
				)
				root := *record.API.RootResourceId
				record.Resources = map[string]*api.Resource{root: {Id: new(root), Path: new("/")}}
				record.Authorizers = map[string]*api.Authorizer{}
			}

			if checkErr := checkAPI(&record.API); checkErr != nil {
				return checkErr
			}

			a = &record.API
			return checkPolicy(scope, &record.API)
		})
	}

	if err != nil {
		return nil, err
	}

	authors := map[string]*api.Authorizer{}
	for _, name := range slices.Sorted(maps.Keys(d.Schemes)) {
		scheme := openapi.Map(d.Schemes[name])
		ext := openapi.Map(scheme["x-amazon-apigateway-authorizer"])
		if ext == nil {
			if openapi.Text(scheme[openAPIType]) != openAPIKey {
				return nil, badRequest("Unsupported security scheme %s", name)
			}

			continue
		}

		if decodeErr := openapi.Check(
			ext,
			openAPIType,
			"authorizerUri",
			"authorizerCredentials",
			"identitySource",
			"identityValidationExpression",
			"authorizerResultTtlInSeconds",
			"providerARNs",
			"authType",
		); decodeErr != nil {
			return nil, decodeErr
		}

		in := &api.CreateAuthorizerRequest{RestApiId: a.Id, Name: new(name)}
		if decodeErr := openapi.Decode(ext, in); decodeErr != nil {
			return nil, decodeErr
		}

		in.Type = new(strings.ToUpper(common.Value(in.Type)))
		if in.IdentitySource == nil {
			in.IdentitySource = new("method.request.header." + openapi.Text(scheme[openAPIName]))
		}

		author, authErr := s.CreateAuthorizer(ctx, in)
		if authErr != nil {
			return nil, authErr
		}

		authors[name] = author
	}

	resources, err := s.GetResources(
		ctx,
		&api.GetResourcesRequest{RestApiId: a.Id, Limit: new(int64(500)), Embed: []string{"methods"}},
	)
	if err != nil {
		return nil, err
	}

	paths := map[string]*string{}
	for _, r := range resources.Items {
		paths[common.Value(r.Path)] = r.Id
	}

	for _, op := range d.Operations {
		parent, path := paths["/"], ""
		if op.Path != "/" {
			for part := range strings.SplitSeq(strings.TrimPrefix(op.Path, "/"), "/") {
				path += "/" + part
				if paths[path] == nil {
					r, createErr := s.CreateResource(
						ctx,
						&api.CreateResourceRequest{RestApiId: a.Id, ParentId: parent, PathPart: new(part)},
					)
					if createErr != nil {
						return nil, createErr
					}

					paths[path] = r.Id
				}

				parent = paths[path]
			}
		}

		// Merge replaces an operation while preserving other methods and resources.
		err = s.changeAPI(ctx, a.Id, func(_ storage.WriteTx, _ request.Scope, record *restAPI) error {
			delete(record.Resources[*parent].ResourceMethods, op.Method)
			return nil
		})
		if err != nil {
			return nil, err
		}

		in := &api.PutMethodRequest{
			RestApiId: a.Id, ResourceId: parent, HttpMethod: new(op.Method), AuthorizationType: new("NONE"),
			RequestParameters: map[string]bool{},
		}

		for _, p := range op.Parameters {
			location := p.In
			if location == "query" {
				location = "querystring"
			}

			in.RequestParameters["method.request."+location+"."+p.Name] = p.Required
		}

		security, secErr := d.Security(op)
		if secErr != nil {
			return nil, secErr
		}

		for _, requirement := range security {
			for name, scopes := range requirement {
				if author := authors[name]; author != nil {
					in.AuthorizationType, in.AuthorizerId = new(authCustom), author.Id
					if common.Value(author.Type) == typeCognito {
						in.AuthorizationType = new(authCognito)
					}

					if decodeErr := openapi.Decode(scopes, &in.AuthorizationScopes); decodeErr != nil {
						return nil, decodeErr
					}
				} else if scheme := openapi.Map(d.Schemes[name]); scheme != nil {
					if openapi.Text(scheme["x-amazon-apigateway-authtype"]) == "awsSigv4" {
						in.AuthorizationType = new(authIAM)
					} else {
						in.ApiKeyRequired = new(true)
					}
				} else {
					return nil, badRequest("Unknown security scheme %s", name)
				}
			}
		}

		if _, err = s.PutMethod(ctx, in); err != nil {
			return nil, err
		}

		ext, extErr := d.Integration(op)
		if extErr != nil {
			return nil, extErr
		}

		if ext != nil {
			if _, ok := ext["payloadFormatVersion"]; ok {
				return nil, badRequest("payloadFormatVersion is only supported for HTTP APIs")
			}

			integration := &api.PutIntegrationRequest{RestApiId: a.Id, ResourceId: parent, HttpMethod: new(op.Method)}
			if decodeErr := openapi.Decode(ext, integration); decodeErr != nil {
				return nil, decodeErr
			}

			integration.Type = new(strings.ToUpper(openapi.Text(ext[openAPIType])))
			integration.IntegrationHttpMethod = new(strings.ToUpper(openapi.Text(ext["httpMethod"])))
			if _, err = s.PutIntegration(ctx, integration); err != nil {
				return nil, err
			}
		}
	}

	return a, nil
}

func (s *Service) GetExport(ctx context.Context, in *api.GetExportRequest) (*api.ExportResponse, error) {
	if common.Value(in.ExportType) != "oas30" && common.Value(in.ExportType) != exportSwagger {
		return nil, badRequest("exportType must be oas30 or swagger")
	}

	extensions := in.Parameters["extensions"]
	for key, value := range in.Parameters {
		if key != "extensions" || (value != "integrations" && value != "authorizers" && value != service) {
			return nil, badRequest("Unsupported export parameter %s=%s", key, value)
		}
	}

	var root openapi.Object
	err := s.viewAPI(ctx, in.RestApiId, func(tx storage.ReadTx, scope request.Scope, a *restAPI) error {
		stage, err := loadStage(tx, scope, *a.API.Id, in.StageName)
		if err != nil {
			return err
		}

		dep, ok, err := storage.Read[deployment](
			tx,
			owned(scope, kindDeployment, *a.API.Id, common.Value(stage.DeploymentId)),
		)
		if err != nil {
			return err
		}

		if !ok {
			return notFound("Deployment not found")
		}

		root = openapi.New(
			common.Value(a.API.Name),
			common.Value(a.API.Version),
			common.Value(in.ExportType) == exportSwagger,
		)
		names := map[string]string{}
		schemes := openapi.Object{}
		if extensions == "authorizers" || extensions == service {
			for id, author := range dep.Authorizers {
				name := common.Value(author.Name)
				names[id] = name
				ext := openapi.Object{}
				if err := openapi.Decode(author, &ext); err != nil {
					return err
				}

				delete(ext, "id")
				delete(ext, openAPIName)
				ext[openAPIType] = strings.ToLower(common.Value(author.Type))
				schemes[name] = openapi.Object{
					openAPIType: openAPIKey, openAPIName: "Authorization", "in": openAPIHeader,
					"x-amazon-apigateway-authorizer": ext,
				}
			}
		}

		for _, r := range dep.Resources {
			for _, m := range r.ResourceMethods {
				if common.Value(m.ApiKeyRequired) {
					schemes["api_key"] = openapi.Object{
						openAPIType: openAPIKey,
						openAPIName: "x-api-key",
						"in":        openAPIHeader,
					}
				}

				if common.Value(m.AuthorizationType) == authIAM {
					schemes["sigv4"] = openapi.Object{
						openAPIType: openAPIKey, openAPIName: "Authorization", "in": openAPIHeader,
						"x-amazon-apigateway-authtype": "awsSigv4",
					}
				}
			}
		}

		if common.Value(in.ExportType) == exportSwagger {
			root["securityDefinitions"] = schemes
		} else {
			root["components"] = openapi.Object{"securitySchemes": schemes}
		}

		if extensions == service {
			root["x-amazon-apigateway-api-key-source"] = a.API.ApiKeySource
			root["x-amazon-apigateway-binary-media-types"] = a.API.BinaryMediaTypes
			root["x-amazon-apigateway-endpoint-configuration"] = a.API.EndpointConfiguration
			if a.API.Policy != nil {
				var policy any
				if err := json.Unmarshal([]byte(*a.API.Policy), &policy); err != nil {
					return err
				}

				root["x-amazon-apigateway-policy"] = policy
			}
		}

		for _, r := range dep.Resources {
			for method, m := range r.ResourceMethods {
				op := openapi.Object{"responses": openapi.Object{"200": openapi.Object{"description": "Success"}}}
				requirement := openapi.Object{}
				if common.Value(m.ApiKeyRequired) {
					requirement["api_key"] = []string{}
				}

				if common.Value(m.AuthorizationType) == authIAM {
					requirement["sigv4"] = []string{}
				}

				if name := names[common.Value(m.AuthorizerId)]; name != "" {
					requirement[name] = m.AuthorizationScopes
				}

				if len(requirement) > 0 {
					op["security"] = []openapi.Object{requirement}
				}

				params := []openapi.Object{}
				for key, required := range m.RequestParameters {
					parts := strings.SplitN(strings.TrimPrefix(key, "method.request."), ".", 2)
					if len(parts) != 2 {
						return badRequest("Cannot export request parameter %s", key)
					}

					location := parts[0]
					if location == "querystring" {
						location = "query"
					}

					p := openapi.Object{openAPIName: parts[1], "in": location, "required": required}
					if common.Value(in.ExportType) == exportSwagger {
						p[openAPIType] = "string"
					} else {
						p["schema"] = openapi.Object{openAPIType: "string"}
					}

					params = append(params, p)
				}

				op["parameters"] = params
				if extensions == "integrations" || extensions == service {
					if i := m.MethodIntegration; i != nil {
						ext := openapi.Object{}
						if err := openapi.Decode(i, &ext); err != nil {
							return err
						}

						delete(ext, "integrationResponses")
						ext[openAPIType] = strings.ToLower(common.Value(i.Type))
						op["x-amazon-apigateway-integration"] = ext
					}
				}

				openapi.Add(root, common.Value(r.Path), method, op)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	body, content, err := openapi.Encode(root, common.Value(in.Accepts))
	if err != nil {
		return nil, err
	}

	return &api.ExportResponse{Body: body, ContentType: new(content)}, nil
}
