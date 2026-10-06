package appsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/vektah/gqlparser/v2/ast"
)

// gqlError is one entry of a GraphQL response's errors, in AppSync's shape.
type gqlError struct {
	Data      any              `json:"data"`
	ErrorInfo any              `json:"errorInfo"`
	ErrorType *string          `json:"errorType"`
	Path      []any            `json:"path"`
	Message   string           `json:"message"`
	Locations []sourceLocation `json:"locations"`
}

type sourceLocation struct {
	SourceName *string `json:"sourceName"`
	Line       int     `json:"line"`
	Column     int     `json:"column"`
}

// fieldError is a resolver's failure, with the errorType, data, and errorInfo
// AppSync reports for it.
type fieldError struct {
	Data    any
	Info    any
	Type    string
	Message string
}

func (e *fieldError) Error() string { return e.Message }

// entry is one field of a response object: its response key and field name.
type entry struct {
	value any
	key   string
	name  string
}

// object is a response object, which keeps its fields in selection order.
type object []entry

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}

		key, err := json.Marshal(e.key)
		if err != nil {
			return nil, err
		}

		value, err := json.Marshal(e.value)
		if err != nil {
			return nil, err
		}

		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}

	b.WriteByte('}')
	return b.Bytes(), nil
}

// group is the fields a selection set asks for under one response key.
type group struct {
	key    string
	fields []*ast.Field
}

// resolution is what a resolver needs to resolve one field.
type resolution struct {
	source any
	// appended are errors the resolver reports beside its value.
	appended []*fieldError
	args     map[string]any
	parent   *ast.Definition
	def      *ast.FieldDefinition
	group    group
	path     []any
}

// executor runs one operation of a validated query document.
type executor struct {
	schema *ast.Schema
	doc    *ast.QueryDocument
	op     *ast.OperationDefinition
	vars   map[string]any
	// resolve resolves a field that has a resolver, reporting false for a
	// field without one, whose value is its source's member of the same name.
	resolve func(ctx context.Context, r *resolution) (value any, handled bool, err error)
	// allowed reports whether the caller may read a field.
	allowed func(parent *ast.Definition, f *ast.FieldDefinition) bool
	errors  []*gqlError
	// noIntrospection refuses __schema and __type.
	noIntrospection bool
}

// run executes the operation, returning its data, which is nil when a
// non-null root field fails, and its errors.
func (e *executor) run(ctx context.Context) (any, []*gqlError) {
	root := map[ast.Operation]*ast.Definition{
		ast.Query: e.schema.Query, ast.Mutation: e.schema.Mutation, ast.Subscription: e.schema.Subscription,
	}[e.op.Operation]
	if root == nil {
		e.fail(nil, nil, "UnsupportedOperation", fmt.Sprintf("TinyStack does not run %s operations.", e.op.Operation))
		return nil, e.errors
	}

	data, ok := e.selectionSet(ctx, root, e.collect(root, e.op.SelectionSet, nil), nil, nil)
	if !ok {
		return nil, e.errors
	}

	return data, e.errors
}

// fail records an error at path, located at f when there is one.
func (e *executor) fail(path []any, f *ast.Field, errorType, message string) {
	ge := &gqlError{Path: path, Message: message, Locations: []sourceLocation{}}
	if errorType != "" {
		ge.ErrorType = new(errorType)
	}

	if f != nil && f.Position != nil {
		ge.Locations = append(ge.Locations, sourceLocation{Line: f.Position.Line, Column: f.Position.Column})
	}

	e.errors = append(e.errors, ge)
}

func (e *executor) included(directives ast.DirectiveList) bool {
	if d := directives.ForName("skip"); d != nil && d.ArgumentMap(e.vars)["if"] == true {
		return false
	}

	d := directives.ForName("include")
	return d == nil || d.ArgumentMap(e.vars)["if"] == true
}

// applies reports whether a fragment on condition applies to obj.
func (e *executor) applies(obj *ast.Definition, condition string) bool {
	if condition == "" || condition == obj.Name {
		return true
	}

	d := e.schema.Types[condition]
	return d != nil && slices.ContainsFunc(e.schema.GetPossibleTypes(d), func(p *ast.Definition) bool {
		return p.Name == obj.Name
	})
}

// collect groups the fields set selects on obj by response key, in order,
// following fragments.
func (e *executor) collect(obj *ast.Definition, set ast.SelectionSet, groups []group) []group {
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			if !e.included(s.Directives) {
				continue
			}

			i := slices.IndexFunc(groups, func(g group) bool { return g.key == s.Alias })
			if i < 0 {
				groups = append(groups, group{key: s.Alias})
				i = len(groups) - 1
			}

			groups[i].fields = append(groups[i].fields, s)
		case *ast.FragmentSpread:
			if e.included(s.Directives) && s.Definition != nil && e.applies(obj, s.Definition.TypeCondition) {
				groups = e.collect(obj, s.Definition.SelectionSet, groups)
			}
		case *ast.InlineFragment:
			if e.included(s.Directives) && e.applies(obj, s.TypeCondition) {
				groups = e.collect(obj, s.SelectionSet, groups)
			}
		}
	}

	return groups
}

// selectionSet resolves groups on obj, reporting false when a non-null field
// fails, which makes the whole object null.
func (e *executor) selectionSet(ctx context.Context, obj *ast.Definition, groups []group, source any, path []any) (
	object, bool,
) {
	out := make(object, 0, len(groups))
	for _, g := range groups {
		v, ok := e.field(ctx, obj, g, source, append(slices.Clone(path), g.key))
		if !ok {
			return nil, false
		}

		out = append(out, entry{key: g.key, name: g.fields[0].Name, value: v})
	}

	return out, true
}

func (e *executor) field(ctx context.Context, obj *ast.Definition, g group, source any, path []any) (any, bool) {
	f := g.fields[0]
	if f.Name == "__typename" {
		return obj.Name, true
	}

	def := obj.Fields.ForName(f.Name)
	args := f.ArgumentMap(e.vars)
	var value any
	switch {
	case (f.Name == "__schema" || f.Name == "__type") && e.noIntrospection:
		e.fail(path, f, "IntrospectionDisabled", "GraphQL introspection is not allowed for this API.")
		return nil, !def.Type.NonNull
	case f.Name == "__schema":
		value = introspection{e.schema}.schemaObject()
	case f.Name == "__type":
		name, _ := args["name"].(string)
		value = introspection{e.schema}.typeObject(ast.NamedType(name, nil))
	case e.allowed != nil && !e.allowed(obj, def):
		e.fail(path, f, "Unauthorized", fmt.Sprintf("Not Authorized to access %s on type %s", f.Name, obj.Name))
		return nil, !def.Type.NonNull
	default:
		var handled bool
		var err error
		var r *resolution
		if e.resolve != nil {
			r = &resolution{source: source, args: args, parent: obj, def: def, group: g, path: path}
			value, handled, err = e.resolve(ctx, r)
		}

		if r != nil {
			for _, fe := range r.appended {
				e.failWith(path, f, fe)
			}
		}

		if err != nil {
			e.failWith(path, f, err)
			return nil, !def.Type.NonNull
		}

		if !handled {
			value = member(source, f.Name, args)
		}
	}

	return e.complete(ctx, def.Type, g, value, path)
}

// failWith records a resolver's error.
func (e *executor) failWith(path []any, f *ast.Field, err error) {
	fe, ok := errors.AsType[*fieldError](err)
	if !ok {
		e.fail(path, f, "", err.Error())
		return
	}

	e.fail(path, f, fe.Type, fe.Message)
	last := e.errors[len(e.errors)-1]
	last.Data, last.ErrorInfo = fe.Data, fe.Info
}

// member is the value of a field without a resolver: its source's member of
// the same name.
func member(source any, name string, args map[string]any) any {
	m, ok := source.(map[string]any)
	if !ok {
		return nil
	}

	if fn, isLazy := m[name].(lazy); isLazy {
		return fn(args)
	}

	return m[name]
}

// complete shapes a resolved value as its type says, reporting false when
// a non-null value is null, which makes its parent null.
func (e *executor) complete(ctx context.Context, t *ast.Type, g group, v any, path []any) (any, bool) {
	r, ok := e.completeNullable(ctx, t, g, v, path)
	if !t.NonNull {
		if !ok {
			return nil, true
		}

		return r, true
	}

	if ok && r == nil {
		e.fail(path, g.fields[0], "", fmt.Sprintf(
			"Cannot return null for non-nullable type: '%s' within parent '%s' (%s)", t.Name(),
			g.fields[0].ObjectDefinition.Name, pathString(path)))
	}

	return r, ok && r != nil
}

func pathString(path []any) string {
	var b bytes.Buffer
	for _, p := range path {
		b.WriteByte('/')
		fmt.Fprint(&b, p)
	}

	return b.String()
}

func (e *executor) completeNullable(ctx context.Context, t *ast.Type, g group, v any, path []any) (any, bool) {
	if v == nil {
		return nil, true
	}

	if t.Elem != nil {
		items, ok := v.([]any)
		if !ok {
			e.fail(path, g.fields[0], "",
				fmt.Sprintf("Can't resolve value (%s) : type mismatch error, expected type LIST", pathString(path)))
			return nil, true
		}

		out := make([]any, 0, len(items))
		for i, item := range items {
			r, itemOK := e.complete(ctx, t.Elem, g, item, append(slices.Clone(path), i))
			if !itemOK {
				return nil, false
			}

			out = append(out, r)
		}

		return out, true
	}

	d := e.schema.Types[t.NamedType]
	switch d.Kind {
	case ast.Scalar:
		return scalar(d.Name, v), true
	case ast.Enum:
		return v, true
	case ast.Interface, ast.Union:
		name, _ := member(v, "__typename", nil).(string)
		concrete := e.schema.Types[name]
		if concrete == nil || !e.applies(concrete, d.Name) {
			e.fail(path, g.fields[0], "", fmt.Sprintf(
				"Could not resolve the type of %s: a value of an abstract type needs a __typename.", pathString(path)))
			return nil, true
		}

		d = concrete
	}

	var groups []group
	for _, f := range g.fields {
		groups = e.collect(d, f.SelectionSet, groups)
	}

	o, ok := e.selectionSet(ctx, d, groups, v, path)
	if !ok {
		return nil, false
	}

	return o, true
}

// scalar serializes a resolved scalar as AppSync does.
func scalar(name string, v any) any {
	switch name {
	case "AWSJSON":
		if _, ok := v.(string); !ok {
			b, _ := json.Marshal(v)
			return string(b)
		}
	case "Int", "AWSTimestamp":
		if f, ok := v.(float64); ok && f == math.Trunc(f) {
			return int64(f)
		}
	case "String", "ID":
		if f, ok := v.(float64); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}

	return v
}
