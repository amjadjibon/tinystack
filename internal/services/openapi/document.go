// Package openapi handles the document formats shared by API Gateway import and export.
package openapi

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

const anyMethod = "x-amazon-apigateway-any-method"

const mediaJSON = "application/json"

type Object map[string]any

type Operation struct {
	Body       Object
	Bodies     map[string]any
	Path       string
	Method     string
	Parameters []Parameter
}

type Parameter struct {
	Schema   any    `json:"schema"`
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
}

// Document is a parsed Swagger 2.0 or OpenAPI 3.0 definition. Schemas holds the reusable schemas (Swagger
// definitions or OpenAPI components.schemas), and each operation's Bodies maps a request content type to its schema.
type Document struct {
	Root       Object
	Schemes    Object
	Schemas    Object
	Operations []Operation
	Warnings   []string
}

func Invalid(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func Map(v any) Object { m, _ := v.(map[string]any); return m }

func Text(v any) string { s, _ := v.(string); return s }

// Decode converts extension members into the existing management API input types.
func Decode(v, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return Invalid("Invalid OpenAPI value: %s", err)
	}

	if err = json.Unmarshal(b, out); err != nil {
		return Invalid("Invalid OpenAPI value: %s", err)
	}

	return nil
}

func Check(m Object, allowed ...string) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if !slices.Contains(allowed, k) {
			return Invalid("Unsupported OpenAPI construct %q", k)
		}
	}

	return nil
}

func Parse(body []byte) (*Document, error) {
	var root map[string]any
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, Invalid("Invalid OpenAPI document: %s", err)
	}

	if Text(root["swagger"]) != "2.0" && !strings.HasPrefix(Text(root["openapi"]), "3.0.") {
		return nil, Invalid("Expected Swagger 2.0 or OpenAPI 3.0")
	}

	if err := Check(
		root,
		"swagger",
		"openapi",
		"info",
		"paths",
		"components",
		"securityDefinitions",
		"security",
		"host",
		"basePath",
		"schemes",
		"servers",
		"consumes",
		"produces",
		"tags",
		"externalDocs",
		"x-amazon-apigateway-api-key-source",
		"x-amazon-apigateway-binary-media-types",
		"x-amazon-apigateway-endpoint-configuration",
		"x-amazon-apigateway-policy",
		"x-amazon-apigateway-cors",
		"definitions",
		"x-amazon-apigateway-request-validators",
		"x-amazon-apigateway-request-validator",
	); err != nil {
		return nil, err
	}

	info := Map(root["info"])
	if Text(info["title"]) == "" {
		return nil, Invalid("info.title is required")
	}

	components := Map(root["components"])
	if err := Check(components, "securitySchemes", "schemas", "x-amazon-apigateway-integrations"); err != nil {
		return nil, err
	}

	d := &Document{Root: root, Schemes: Map(root["securityDefinitions"]), Schemas: Map(root["definitions"])}
	if d.Schemes == nil {
		d.Schemes = Map(components["securitySchemes"])
	}

	if d.Schemas == nil {
		d.Schemas = Map(components["schemas"])
	}

	paths, ok := root["paths"].(map[string]any)
	if !ok {
		return nil, Invalid("paths must be an object")
	}

	for _, path := range slices.Sorted(maps.Keys(paths)) {
		if !strings.HasPrefix(path, "/") {
			return nil, Invalid("Invalid path %q", path)
		}

		item := Map(paths[path])
		if item == nil {
			return nil, Invalid("Path %s must be an object", path)
		}

		for _, method := range slices.Sorted(maps.Keys(item)) {
			if method == "parameters" {
				continue
			}

			if !slices.Contains([]string{
				"get", "put", "post", "delete", "patch", "head", "options",
				anyMethod,
			}, method) {
				return nil, Invalid("Unsupported path member %q", method)
			}

			op := Map(item[method])
			if op == nil {
				return nil, Invalid("Operation %s %s must be an object", method, path)
			}

			if err := Check(
				op,
				"operationId",
				"summary",
				"description",
				"tags",
				"deprecated",
				"externalDocs",
				"responses",
				"parameters",
				"security",
				"consumes",
				"produces",
				"requestBody",
				"x-amazon-apigateway-integration",
				"isDefaultRoute",
				"x-amazon-apigateway-request-validator",
			); err != nil {
				return nil, err
			}

			bodies, err := requestBodies(root, op)
			if err != nil {
				return nil, err
			}

			params := []Parameter{}
			for _, raw := range []any{item["parameters"], op["parameters"]} {
				var pp []Parameter
				if err := Decode(raw, &pp); err != nil {
					return nil, err
				}

				for _, p := range pp {
					if p.In == "body" {
						if err := swaggerBody(root, op, p, bodies); err != nil {
							return nil, err
						}

						continue
					}

					if p.Name == "" || !slices.Contains([]string{"path", "query", "header"}, p.In) {
						return nil, Invalid("Unsupported parameter %q in %q", p.Name, p.In)
					}

					params = append(params, p)
				}
			}

			upper := strings.ToUpper(method)
			if method == anyMethod {
				upper = "ANY"
			}

			if op["x-amazon-apigateway-integration"] == nil {
				d.Warnings = append(d.Warnings, fmt.Sprintf("No integration defined for %s %s", upper, path))
			}

			d.Operations = append(d.Operations, Operation{
				Body: op, Bodies: bodies, Path: path, Method: upper, Parameters: params,
			})
		}
	}

	return d, nil
}

// requestBodies reads an OpenAPI 3.0 operation's requestBody into a schema per content type.
func requestBodies(root, op Object) (map[string]any, error) {
	bodies := map[string]any{}
	raw, ok := op["requestBody"]
	if !ok {
		return bodies, nil
	}

	body := Map(raw)
	if root["swagger"] != nil || body == nil {
		return nil, Invalid("requestBody must be an OpenAPI 3.0 object")
	}

	if err := Check(body, "description", "required", "content"); err != nil {
		return nil, err
	}

	content := Map(body["content"])
	for _, contentType := range slices.Sorted(maps.Keys(content)) {
		media := Map(content[contentType])
		if err := Check(media, "schema", "example", "examples"); err != nil {
			return nil, err
		}

		if media["schema"] != nil {
			bodies[contentType] = media["schema"]
		}
	}

	return bodies, nil
}

// swaggerBody records a Swagger 2.0 body parameter's schema under each content type the operation consumes.
func swaggerBody(root, op Object, p Parameter, bodies map[string]any) error {
	if root["swagger"] == nil {
		return Invalid("Body parameters are only supported in Swagger 2.0; use requestBody")
	}

	if p.Schema == nil {
		return Invalid("Body parameter %q has no schema", p.Name)
	}

	var consumes []string
	for _, raw := range []any{op["consumes"], root["consumes"]} {
		if err := Decode(raw, &consumes); err != nil {
			return err
		}

		if len(consumes) > 0 {
			break
		}
	}

	if len(consumes) == 0 {
		consumes = []string{mediaJSON}
	}

	for _, contentType := range consumes {
		bodies[contentType] = p.Schema
	}

	return nil
}

func (d *Document) Integration(op Operation) (Object, error) {
	raw, present := op.Body["x-amazon-apigateway-integration"]
	m := Map(raw)
	if present && m == nil {
		return nil, Invalid("Integration must be an object")
	}

	if rawRef, present := m["$ref"]; present {
		ref := Text(rawRef)
		if len(m) != 1 {
			return nil, Invalid("Integration reference %q cannot have other properties", ref)
		}

		prefix := "#/components/x-amazon-apigateway-integrations/"
		if !strings.HasPrefix(ref, prefix) {
			return nil, Invalid("Unsupported integration reference %q", ref)
		}

		m = Map(Map(Map(d.Root["components"])["x-amazon-apigateway-integrations"])[strings.TrimPrefix(ref, prefix)])
		if m == nil {
			return nil, Invalid("Unresolved integration reference %q", ref)
		}
	}

	if err := Check(m, "type", "uri", "httpMethod", "credentials", "requestParameters", "requestTemplates",
		"passthroughBehavior", "contentHandling", "timeoutInMillis", "cacheNamespace", "cacheKeyParameters",
		"connectionType", "payloadFormatVersion", "responses"); err != nil {
		return nil, err
	}

	return m, nil
}

func (d *Document) Security(op Operation) ([]Object, error) {
	raw, ok := op.Body["security"]
	if !ok {
		raw = d.Root["security"]
	}

	var out []Object
	if err := Decode(raw, &out); err != nil {
		return nil, err
	}

	if len(out) > 1 {
		return nil, Invalid("Alternative security requirements are unsupported")
	}

	return out, nil
}

func New(title, version string, swagger bool) Object {
	root := Object{"info": Object{"title": title, "version": version}, "paths": Object{}}
	if swagger {
		root["swagger"] = "2.0"
	} else {
		root["openapi"] = "3.0.1"
	}

	return root
}

func Add(root Object, path, method string, op Object) {
	paths := root["paths"].(Object)
	item, ok := paths[path].(Object)
	if !ok {
		item = Object{}
		paths[path] = item
	}

	key := strings.ToLower(method)
	if method == "ANY" {
		key = anyMethod
	}

	item[key] = op
}

func Encode(root Object, format string) (body []byte, contentType string, err error) {
	switch strings.ToLower(format) {
	case "json", mediaJSON, "":
		b, err := json.Marshal(root)
		return b, mediaJSON, err
	case "yaml", "application/yaml", "application/x-yaml", "text/yaml":
		var normalized map[string]any
		if err := Decode(root, &normalized); err != nil {
			return nil, "", err
		}

		b, err := yaml.Marshal(normalized)
		return b, "application/yaml", err
	default:
		return nil, "", Invalid("Unsupported export format %q", format)
	}
}
