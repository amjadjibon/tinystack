package appsync

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// Members that many introspection objects have.
const (
	keyName              = "name"
	keyDescription       = "description"
	keyKind              = "kind"
	keyDeprecationReason = "deprecationReason"
	keyOfType            = "ofType"
	keyIsDeprecated      = "isDeprecated"
)

// lazy is a field of an introspection object computed from its arguments, so
// the type graph, which is cyclic, is built only as deep as a query reads it.
type lazy func(args map[string]any) any

// introspection builds the __Schema, __Type, and other introspection objects
// of a schema as maps the executor reads like any other source.
type introspection struct{ schema *ast.Schema }

// own reports whether a field is one of the schema's, rather than one of the
// __schema and __type fields the validator adds to the query type.
func own(f *ast.FieldDefinition) bool { return !strings.HasPrefix(f.Name, "__") }

func deprecation(directives ast.DirectiveList) (deprecated bool, reason any) {
	d := directives.ForName("deprecated")
	if d == nil {
		return false, nil
	}

	if a := d.Arguments.ForName("reason"); a != nil {
		return true, a.Value.Raw
	}

	return true, "No longer supported"
}

func describe(s string) any {
	if s == "" {
		return nil
	}

	return s
}

func (in introspection) schemaObject() map[string]any {
	named := func(d *ast.Definition) any {
		if d == nil {
			return nil
		}

		return in.typeObject(ast.NamedType(d.Name, nil))
	}

	return map[string]any{
		keyDescription:     describe(in.schema.Description),
		"queryType":        named(in.schema.Query),
		"mutationType":     named(in.schema.Mutation),
		"subscriptionType": named(in.schema.Subscription),
		"types": lazy(func(map[string]any) any {
			names := make([]string, 0, len(in.schema.Types))
			for name := range in.schema.Types {
				names = append(names, name)
			}

			slices.Sort(names)
			out := make([]any, 0, len(names))
			for _, name := range names {
				out = append(out, in.typeObject(ast.NamedType(name, nil)))
			}

			return out
		}),
		"directives": lazy(func(map[string]any) any {
			names := make([]string, 0, len(in.schema.Directives))
			for name := range in.schema.Directives {
				names = append(names, name)
			}

			slices.Sort(names)
			out := make([]any, 0, len(names))
			for _, name := range names {
				d := in.schema.Directives[name]
				locations := make([]any, 0, len(d.Locations))
				for _, l := range d.Locations {
					locations = append(locations, string(l))
				}

				out = append(out, map[string]any{
					keyName: d.Name, keyDescription: describe(d.Description), "locations": locations,
					"args": in.arguments(d.Arguments), "isRepeatable": d.IsRepeatable,
				})
			}

			return out
		}),
	}
}

// typeObject is the __Type of a type reference: a wrapper (NON_NULL or LIST)
// around its inner type, or a named type.
func (in introspection) typeObject(t *ast.Type) any {
	if t == nil {
		return nil
	}

	if t.NonNull {
		inner := *t
		inner.NonNull = false
		return map[string]any{keyKind: "NON_NULL", keyName: nil, keyOfType: in.typeObject(&inner)}
	}

	if t.Elem != nil {
		return map[string]any{keyKind: "LIST", keyName: nil, keyOfType: in.typeObject(t.Elem)}
	}

	d := in.schema.Types[t.NamedType]
	if d == nil {
		return nil
	}

	o := map[string]any{
		keyKind: string(d.Kind), keyName: d.Name, keyDescription: describe(d.Description), keyOfType: nil,
		"specifiedByURL": nil, "isOneOf": d.Directives.ForName("oneOf") != nil,
		"fields": nil, "interfaces": nil, "possibleTypes": nil, "enumValues": nil, "inputFields": nil,
	}
	switch d.Kind {
	case ast.Object, ast.Interface:
		o["fields"] = lazy(func(args map[string]any) any { return in.fields(d, args) })
		o["interfaces"] = lazy(func(map[string]any) any { return in.named(d.Interfaces) })
		if d.Kind == ast.Interface {
			o["possibleTypes"] = lazy(func(map[string]any) any { return in.possible(d) })
		}
	case ast.Union:
		o["possibleTypes"] = lazy(func(map[string]any) any { return in.possible(d) })
	case ast.Enum:
		o["enumValues"] = lazy(func(args map[string]any) any { return in.enumValues(d, args) })
	case ast.InputObject:
		o["inputFields"] = lazy(func(map[string]any) any { return in.inputValues(d.Fields) })
	case ast.Scalar:
		if s := d.Directives.ForName("specifiedBy"); s != nil {
			if a := s.Arguments.ForName("url"); a != nil {
				o["specifiedByURL"] = a.Value.Raw
			}
		}
	}

	return o
}

func (in introspection) named(names []string) []any {
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, in.typeObject(ast.NamedType(name, nil)))
	}

	return out
}

func (in introspection) possible(d *ast.Definition) []any {
	var names []string
	for _, p := range in.schema.GetPossibleTypes(d) {
		names = append(names, p.Name)
	}

	slices.Sort(names)
	return in.named(names)
}

func includeDeprecated(args map[string]any) bool {
	b, _ := args["includeDeprecated"].(bool)
	return b
}

func (in introspection) fields(d *ast.Definition, args map[string]any) []any {
	out := []any{}
	for _, f := range d.Fields {
		deprecated, reason := deprecation(f.Directives)
		if !own(f) || (deprecated && !includeDeprecated(args)) {
			continue
		}

		out = append(out, map[string]any{
			keyName: f.Name, keyDescription: describe(f.Description), "args": in.arguments(f.Arguments),
			"type": in.typeObject(f.Type), keyIsDeprecated: deprecated, keyDeprecationReason: reason,
		})
	}

	return out
}

func (in introspection) arguments(args ast.ArgumentDefinitionList) []any {
	out := []any{}
	for _, a := range args {
		out = append(out, in.inputValue(a.Name, a.Description, a.Type, a.DefaultValue, a.Directives))
	}

	return out
}

func (in introspection) inputValues(fields ast.FieldList) []any {
	out := []any{}
	for _, f := range fields {
		out = append(out, in.inputValue(f.Name, f.Description, f.Type, f.DefaultValue, f.Directives))
	}

	return out
}

func (in introspection) inputValue(
	name, description string,
	t *ast.Type,
	value *ast.Value,
	directives ast.DirectiveList,
) map[string]any {
	deprecated, reason := deprecation(directives)
	var defaultValue any
	if value != nil {
		defaultValue = value.String()
	}

	return map[string]any{
		keyName: name, keyDescription: describe(description), "type": in.typeObject(t), "defaultValue": defaultValue,
		keyIsDeprecated: deprecated, keyDeprecationReason: reason,
	}
}

func (in introspection) enumValues(d *ast.Definition, args map[string]any) []any {
	out := []any{}
	for _, v := range d.EnumValues {
		deprecated, reason := deprecation(v.Directives)
		if deprecated && !includeDeprecated(args) {
			continue
		}

		out = append(out, map[string]any{
			keyName: v.Name, keyDescription: describe(v.Description), keyIsDeprecated: deprecated,
			keyDeprecationReason: reason,
		})
	}

	return out
}

// introspectionQuery is the query GetIntrospectionSchema answers JSON with.
const introspectionQuery = `query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types { ...FullType }
    directives { name description locations args { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind name description
  fields(includeDeprecated: true) {
    name description args { ...InputValue } type { ...TypeRef } isDeprecated deprecationReason
  }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name description type { ...TypeRef } defaultValue }
fragment TypeRef on __Type {
  kind name
  ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name
    ofType { kind name ofType { kind name } } } } } } }
}`

// introspect renders a schema as the result of the standard introspection
// query, the JSON format of GetIntrospectionSchema.
func introspect(ctx context.Context, schema *ast.Schema) ([]byte, error) {
	doc, errs := gqlparser.LoadQueryWithRules(schema, introspectionQuery, nil)
	if len(errs) > 0 {
		return nil, errs
	}

	e := &executor{schema: schema, doc: doc, op: doc.Operations[0]}
	data, _ := e.run(ctx)
	return json.Marshal(map[string]any{"data": data})
}
