package apigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	draft4      = "http://json-schema.org/draft-04/schema#"
	draft7      = "http://json-schema.org/draft-07/schema#"
	modelPrefix = "https://apigateway.amazonaws.com/restapis/"
)

var modelName = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// defaultModels are the models every new REST API has, as on AWS.
func defaultModels() map[string]*api.Model {
	model := func(name, schema string) *api.Model {
		return &api.Model{
			Id: new(common.RandomID(6)), Name: new(name), ContentType: new("application/json"),
			Schema: new(schema), Description: new("This is a default " + strings.ToLower(name) + " schema model"),
		}
	}

	return map[string]*api.Model{
		"Empty": model("Empty", `{"$schema":"`+draft4+`","title":"Empty Schema","type":"object"}`),
		"Error": model("Error", `{"$schema":"`+draft4+`","title":"Error Schema","type":"object",`+
			`"properties":{"message":{"type":"string"}}}`),
	}
}

// modelURI is the URI other models' $ref name a model by.
func modelURI(apiID, name string) string { return modelPrefix + apiID + "/models/" + name }

// compileModel resolves a model's draft-4 schema, loading the API's other models it refers to by URI. A model
// without a schema accepts any body.
func compileModel(apiID string, models map[string]*api.Model, name string) (*jsonschema.Resolved, error) {
	m := models[name]
	if m == nil {
		return nil, fmt.Errorf("model %s does not exist", name)
	}

	root, err := draft7Schema(common.Value(m.Schema))
	if err != nil {
		return nil, err
	}

	return root.Resolve(&jsonschema.ResolveOptions{
		BaseURI: modelURI(apiID, name),
		Loader: func(uri *url.URL) (*jsonschema.Schema, error) {
			ref, ok := strings.CutPrefix(uri.String(), modelPrefix+apiID+"/models/")
			if !ok || models[ref] == nil {
				return nil, fmt.Errorf("%s is not a model of this REST API", uri)
			}

			return draft7Schema(common.Value(models[ref].Schema))
		},
	})
}

// draft7Schema parses a JSON Schema draft 4 document as draft 7, which differs in the boolean exclusiveMaximum and
// exclusiveMinimum and in id.
func draft7Schema(text string) (*jsonschema.Schema, error) {
	if strings.TrimSpace(text) == "" {
		return &jsonschema.Schema{Schema: draft7}, nil
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("the schema is not a JSON object: %w", err)
	}

	if s, ok := doc["$schema"].(string); ok && strings.TrimSuffix(s, "#") != strings.TrimSuffix(draft4, "#") {
		return nil, fmt.Errorf("models use JSON Schema draft 4, not %s", s)
	}

	upgrade(doc)
	doc["$schema"] = draft7
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}

	s := &jsonschema.Schema{}
	if unmarshalErr := json.Unmarshal(data, s); unmarshalErr != nil {
		return nil, unmarshalErr
	}

	return s, nil
}

// upgrade rewrites a draft-4 schema object in place, visiting only the keywords that hold subschemas so that
// property names are left alone.
func upgrade(s map[string]any) {
	delete(s, "$schema")
	if id, ok := s["id"].(string); ok {
		delete(s, "id")
		s["$id"] = id
	}

	for _, bound := range []string{"Maximum", "Minimum"} {
		exclusive, limit := "exclusive"+bound, strings.ToLower(bound)
		if flag, ok := s[exclusive].(bool); ok {
			delete(s, exclusive)
			if flag && s[limit] != nil {
				s[exclusive] = s[limit]
				delete(s, limit)
			}
		}
	}

	visit := func(v any) {
		if child, ok := v.(map[string]any); ok {
			upgrade(child)
		}
	}

	for _, key := range []string{"items", "additionalItems", "additionalProperties", "not"} {
		if list, ok := s[key].([]any); ok {
			for _, v := range list {
				visit(v)
			}
		} else {
			visit(s[key])
		}
	}

	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if list, ok := s[key].([]any); ok {
			for _, v := range list {
				visit(v)
			}
		}
	}

	for _, key := range []string{"properties", "patternProperties", "definitions", "dependencies"} {
		if children, ok := s[key].(map[string]any); ok {
			for _, v := range children {
				visit(v)
			}
		}
	}
}

func loadModel(a *restAPI, name *string) (*api.Model, error) {
	m, ok := a.Models[common.Value(name)]
	if !ok {
		return nil, notFound("Invalid model name specified: %s", common.Value(name))
	}

	return m, nil
}

// checkModel validates a model and the schema it would store, against the API's other models.
func checkModel(a *restAPI, m *api.Model) error {
	if common.Value(m.ContentType) == "" {
		return badRequest("Content type must be specified")
	}

	models := maps.Clone(a.Models)
	models[*m.Name] = m
	if _, err := compileModel(*a.API.Id, models, *m.Name); err != nil {
		return badRequest("Invalid model schema specified: %s", err)
	}

	return nil
}

func (s *Service) CreateModel(ctx context.Context, in *api.CreateModelRequest) (*api.Model, error) {
	if !modelName.MatchString(common.Value(in.Name)) {
		return nil, badRequest("Model name must be non-empty and alphanumeric")
	}

	m := &api.Model{
		Id: new(common.RandomID(6)), Name: in.Name, Description: in.Description, Schema: in.Schema,
		ContentType: in.ContentType,
	}
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		if _, exists := a.Models[*m.Name]; exists {
			return conflict("Model name already exists for this REST API")
		}

		if checkErr := checkModel(a, m); checkErr != nil {
			return checkErr
		}

		a.Models[*m.Name] = m
		return nil
	})
	if err != nil {
		return nil, err
	}

	return m, nil
}

func (s *Service) GetModel(ctx context.Context, in *api.GetModelRequest) (*api.Model, error) {
	var out *api.Model
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		var loadErr error
		out, loadErr = loadModel(a, in.ModelName)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetModels(ctx context.Context, in *api.GetModelsRequest) (*api.Models, error) {
	out := &api.Models{Items: []api.Model{}}
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		items := []api.Model{}
		for _, name := range slices.Sorted(maps.Keys(a.Models)) {
			items = append(items, *a.Models[name])
		}

		var pageErr error
		items, out.Position, pageErr = page(items, func(m api.Model) string { return *m.Name }, in.Position, in.Limit)
		out.Items = append(out.Items, items...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateModel(ctx context.Context, in *api.UpdateModelRequest) (*api.Model, error) {
	var out *api.Model
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		m, loadErr := loadModel(a, in.ModelName)
		if loadErr != nil {
			return loadErr
		}

		changed := common.Clone(m)
		if patchErr := patch(changed, in.PatchOperations, "description", "schema"); patchErr != nil {
			return patchErr
		}

		if checkErr := checkModel(a, changed); checkErr != nil {
			return checkErr
		}

		a.Models[*m.Name], out = changed, changed
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteModel(ctx context.Context, in *api.DeleteModelRequest) (*api.DeleteModelOutput, error) {
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		m, loadErr := loadModel(a, in.ModelName)
		if loadErr != nil {
			return loadErr
		}

		if where := modelUse(a, *m.Name); where != "" {
			return conflict("Cannot delete model '%s', is referenced in %s", *m.Name, where)
		}

		delete(a.Models, *m.Name)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteModelOutput{}, nil
}

// modelUse names the first method request or response, or other model, that refers to the model, if any.
func modelUse(a *restAPI, name string) string {
	for _, id := range slices.Sorted(maps.Keys(a.Resources)) {
		r := a.Resources[id]
		for _, verb := range slices.Sorted(maps.Keys(r.ResourceMethods)) {
			m := r.ResourceMethods[verb]
			where := common.Value(r.Path) + "/" + verb
			if slices.Contains(slices.Collect(maps.Values(m.RequestModels)), name) {
				return "method request: " + where
			}

			for _, code := range slices.Sorted(maps.Keys(m.MethodResponses)) {
				if slices.Contains(slices.Collect(maps.Values(m.MethodResponses[code].ResponseModels)), name) {
					return "method response: " + where + "/" + code
				}
			}
		}
	}

	for _, other := range slices.Sorted(maps.Keys(a.Models)) {
		if other != name && strings.Contains(common.Value(a.Models[other].Schema), modelURI(*a.API.Id, name)) {
			return "model: " + other
		}
	}

	return ""
}

// checkModels requires the models a method request or response names to exist.
func checkModels(a *restAPI, models map[string]string) error {
	for _, contentType := range slices.Sorted(maps.Keys(models)) {
		if _, ok := a.Models[models[contentType]]; !ok {
			return badRequest("Invalid model identifier specified: %s", models[contentType])
		}
	}

	return nil
}

func loadValidator(a *restAPI, id *string) (*api.RequestValidator, error) {
	v, ok := a.Validators[common.Value(id)]
	if !ok {
		return nil, notFound("Invalid Request Validator identifier specified")
	}

	return v, nil
}

func (s *Service) CreateRequestValidator(
	ctx context.Context,
	in *api.CreateRequestValidatorRequest,
) (*api.RequestValidator, error) {
	v := &api.RequestValidator{
		Id: new(common.RandomID(6)), Name: in.Name,
		ValidateRequestBody:       new(common.Value(in.ValidateRequestBody)),
		ValidateRequestParameters: new(common.Value(in.ValidateRequestParameters)),
	}
	if common.Value(v.Name) == "" {
		return nil, badRequest("Request validator name must be specified")
	}

	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		for _, other := range a.Validators {
			if *other.Name == *v.Name {
				return conflict("Request validator with name '%s' already exists", *v.Name)
			}
		}

		a.Validators[*v.Id] = v
		return nil
	})
	if err != nil {
		return nil, err
	}

	return v, nil
}

func (s *Service) GetRequestValidator(
	ctx context.Context,
	in *api.GetRequestValidatorRequest,
) (*api.RequestValidator, error) {
	var out *api.RequestValidator
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		var loadErr error
		out, loadErr = loadValidator(a, in.RequestValidatorId)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetRequestValidators(
	ctx context.Context,
	in *api.GetRequestValidatorsRequest,
) (*api.RequestValidators, error) {
	out := &api.RequestValidators{Items: []api.RequestValidator{}}
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		items := []api.RequestValidator{}
		for _, id := range slices.Sorted(maps.Keys(a.Validators)) {
			items = append(items, *a.Validators[id])
		}

		var pageErr error
		items, out.Position, pageErr = page(
			items,
			func(v api.RequestValidator) string { return *v.Id },
			in.Position,
			in.Limit,
		)
		out.Items = append(out.Items, items...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateRequestValidator(
	ctx context.Context,
	in *api.UpdateRequestValidatorRequest,
) (*api.RequestValidator, error) {
	var out *api.RequestValidator
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		v, loadErr := loadValidator(a, in.RequestValidatorId)
		if loadErr != nil {
			return loadErr
		}

		changed := common.Clone(v)
		if patchErr := patch(
			changed,
			in.PatchOperations,
			"name",
			"validateRequestBody",
			"validateRequestParameters",
		); patchErr != nil {
			return patchErr
		}

		if common.Value(changed.Name) == "" {
			return badRequest("Request validator name must be specified")
		}

		a.Validators[*v.Id], out = changed, changed
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteRequestValidator(
	ctx context.Context,
	in *api.DeleteRequestValidatorRequest,
) (*api.DeleteRequestValidatorOutput, error) {
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		v, loadErr := loadValidator(a, in.RequestValidatorId)
		if loadErr != nil {
			return loadErr
		}

		for _, r := range a.Resources {
			for verb, m := range r.ResourceMethods {
				if common.Value(m.RequestValidatorId) == *v.Id {
					return conflict("Cannot delete request validator '%s', is referenced in method: %s/%s",
						*v.Id, common.Value(r.Path), verb)
				}
			}
		}

		delete(a.Validators, *v.Id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRequestValidatorOutput{}, nil
}
