package apigateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/openapi"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	validatorsExtension = "x-amazon-apigateway-request-validators"
	validatorExtension  = "x-amazon-apigateway-request-validator"
	definitionsRef      = "#/definitions/"
	schemasRef          = "#/components/schemas/"
	validateBody        = "validateRequestBody"
	validateParameters  = "validateRequestParameters"
	mediaJSON           = "application/json"
	openAPIRequired     = "required"
	schemaRef           = "$ref"
)

// rewriteRefs copies a JSON value, replacing every $ref string with what rewrite returns for it.
func rewriteRefs(v any, rewrite func(string) (string, error)) (any, error) {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			if ref, ok := child.(string); ok && key == schemaRef {
				changed, err := rewrite(ref)
				if err != nil {
					return nil, err
				}

				out[key] = changed
				continue
			}

			changed, err := rewriteRefs(child, rewrite)
			if err != nil {
				return nil, err
			}

			out[key] = changed
		}

		return out, nil
	case openapi.Object:
		return rewriteRefs(map[string]any(v), rewrite)
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			changed, err := rewriteRefs(child, rewrite)
			if err != nil {
				return nil, err
			}

			out[i] = changed
		}

		return out, nil
	default:
		return v, nil
	}
}

// schemaName returns the definition a document-local $ref names; only whole schemas can be referenced.
func schemaName(ref string) (string, error) {
	for _, prefix := range []string{definitionsRef, schemasRef} {
		if name, ok := strings.CutPrefix(ref, prefix); ok && name != "" && !strings.Contains(name, "/") {
			return name, nil
		}
	}

	return "", badRequest("Unsupported schema reference %q", ref)
}

// importModels stores reusable schemas as application/json models and inline request schemas with their content
// types, replacing models of the same name. References become model URIs, and every imported model must compile.
func (s *Service) importModels(ctx context.Context, apiID *string, d *openapi.Document) error {
	schemas := maps.Clone(d.Schemas)
	if schemas == nil {
		schemas = openapi.Object{}
	}

	contentTypes := map[string]string{}
	for _, op := range d.Operations {
		for _, contentType := range slices.Sorted(maps.Keys(op.Bodies)) {
			name, inline, err := requestModel(op, contentType)
			if err != nil {
				return err
			}

			if !inline {
				continue
			}

			if _, exists := schemas[name]; exists {
				return badRequest("Generated request model name conflicts with schema %s", name)
			}

			schemas[name] = op.Bodies[contentType]
			contentTypes[name] = contentType
		}
	}

	if len(schemas) == 0 {
		return nil
	}

	return s.changeAPI(ctx, apiID, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		toURI := func(ref string) (string, error) {
			name, err := schemaName(ref)
			return modelURI(*apiID, name), err
		}

		names := slices.Sorted(maps.Keys(schemas))
		for _, name := range names {
			if !modelName.MatchString(name) {
				return badRequest("Model name must be non-empty and alphanumeric: %s", name)
			}

			raw, err := rewriteRefs(schemas[name], toURI)
			if err != nil {
				return err
			}

			schema, ok := raw.(map[string]any)
			if !ok {
				return badRequest("Schema %s must be an object", name)
			}

			if schema["$schema"] == nil {
				schema["$schema"] = draft4
			}

			if schema["title"] == nil {
				schema["title"] = name
			}

			text, err := json.Marshal(schema)
			if err != nil {
				return err
			}

			m := &api.Model{
				Id: new(common.RandomID(6)), Name: new(name), ContentType: new(mediaJSON),
				Schema: new(string(text)),
			}
			if contentType, ok := contentTypes[name]; ok {
				m.ContentType = new(contentType)
			}

			if old := a.Models[name]; old != nil {
				m.Id, m.Description = old.Id, old.Description
			}

			a.Models[name] = m
		}

		for _, name := range names {
			if _, err := compileModel(*apiID, a.Models, name); err != nil {
				return badRequest("Invalid model schema specified for %s: %s", name, err)
			}
		}

		return nil
	})
}

// importValidators creates or updates the document's named request validators and returns every validator's id by
// name.
func (s *Service) importValidators(ctx context.Context, apiID *string, d *openapi.Document) (map[string]string, error) {
	ids := map[string]string{}
	err := s.changeAPI(ctx, apiID, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		for id, v := range a.Validators {
			ids[*v.Name] = id
		}

		declared := openapi.Map(d.Root[validatorsExtension])
		for _, name := range slices.Sorted(maps.Keys(declared)) {
			flags := openapi.Map(declared[name])
			if err := openapi.Check(flags, validateBody, validateParameters); err != nil {
				return err
			}

			body, _ := flags[validateBody].(bool)
			params, _ := flags[validateParameters].(bool)
			if ids[name] == "" {
				ids[name] = common.RandomID(6)
			}

			v := &api.RequestValidator{
				Id: new(ids[name]), Name: new(name),
				ValidateRequestBody: new(body), ValidateRequestParameters: new(params),
			}
			a.Validators[*v.Id] = v
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return ids, nil
}

// requestModel names a whole-schema reference or an inline request schema.
func requestModel(op openapi.Operation, contentType string) (name string, inline bool, err error) {
	schema := openapi.Map(op.Bodies[contentType])
	if schema == nil {
		return "", false, badRequest("Request body schema for %s %s must be an object", op.Method, op.Path)
	}

	if raw, ok := schema[schemaRef]; ok {
		ref := openapi.Text(raw)
		if len(schema) != 1 || ref == "" {
			return "", false, badRequest("Request body $ref must be a whole schema without siblings")
		}

		name, err = schemaName(ref)
		return name, false, err
	}

	// AWS requires alphanumeric model names but does not document its generated-name algorithm.
	// Use Body + method + the first 8 bytes of SHA-256(path NUL content type): stable across schema edits and
	// reimports, distinct for paths that differ only in punctuation, and independent of operationId or iteration order.
	digest := sha256.Sum256([]byte(op.Path + "\x00" + contentType))
	return fmt.Sprintf("Body%s%x", op.Method, digest[:8]), true, nil
}

// requestModels maps request content types to imported models and returns the operation's (or document's) validator.
func requestModels(
	d *openapi.Document,
	op openapi.Operation,
	validators map[string]string,
) (models map[string]string, validator *string, err error) {
	models = map[string]string{}
	for _, contentType := range slices.Sorted(maps.Keys(op.Bodies)) {
		name, _, nameErr := requestModel(op, contentType)
		if nameErr != nil {
			return nil, nil, nameErr
		}

		models[contentType] = name
	}

	name, ok := op.Body[validatorExtension]
	if !ok {
		name = d.Root[validatorExtension]
	}

	if openapi.Text(name) == "" {
		return models, nil, nil
	}

	id, ok := validators[openapi.Text(name)]
	if !ok {
		return nil, nil, badRequest("Unknown request validator %s", openapi.Text(name))
	}

	return models, new(id), nil
}

// exportModels adds a deployment's models to the document as Swagger definitions or OpenAPI component schemas.
func exportModels(root openapi.Object, apiID string, models map[string]*api.Model, swagger bool) error {
	prefix := schemasRef
	if swagger {
		prefix = definitionsRef
	}

	toLocal := func(ref string) (string, error) {
		if name, ok := strings.CutPrefix(ref, modelPrefix+apiID+"/models/"); ok {
			return prefix + name, nil
		}

		return ref, nil
	}

	schemas := openapi.Object{}
	for name, m := range models {
		schema := map[string]any{}
		if text := common.Value(m.Schema); text != "" {
			if err := json.Unmarshal([]byte(text), &schema); err != nil {
				return err
			}
		}

		delete(schema, "$schema")
		local, err := rewriteRefs(schema, toLocal)
		if err != nil {
			return err
		}

		schemas[name] = local
	}

	if swagger {
		root["definitions"] = schemas
	} else {
		root["components"].(openapi.Object)["schemas"] = schemas
	}

	return nil
}

// exportRequestBody describes a method's request models on its exported operation.
func exportRequestBody(op openapi.Object, models map[string]string, swagger bool) {
	if len(models) == 0 {
		return
	}

	types := slices.Sorted(maps.Keys(models))
	if swagger {
		// A Swagger operation has one body schema; it takes the JSON model when there is one.
		contentType := types[0]
		if _, ok := models[mediaJSON]; ok {
			contentType = mediaJSON
		}

		op["consumes"] = types
		op["parameters"] = append(op["parameters"].([]openapi.Object), openapi.Object{
			"in": "body", openAPIName: models[contentType], openAPIRequired: true,
			"schema": openapi.Object{schemaRef: definitionsRef + models[contentType]},
		})
		return
	}

	content := openapi.Object{}
	for _, contentType := range types {
		content[contentType] = openapi.Object{"schema": openapi.Object{schemaRef: schemasRef + models[contentType]}}
	}

	op["requestBody"] = openapi.Object{"content": content, openAPIRequired: true}
}

// exportValidators adds a deployment's request validators to the document.
func exportValidators(root openapi.Object, validators map[string]*api.RequestValidator) {
	if len(validators) == 0 {
		return
	}

	declared := openapi.Object{}
	for _, v := range validators {
		declared[*v.Name] = openapi.Object{
			validateBody:       common.Value(v.ValidateRequestBody),
			validateParameters: common.Value(v.ValidateRequestParameters),
		}
	}

	root[validatorsExtension] = declared
}
