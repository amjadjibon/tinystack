package appsync

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	formatSDL     = "SDL"
	formatJSON    = "JSON"
	statusSuccess = "SUCCESS"
	statusFailed  = "FAILED"
)

// prelude declares the scalars and directives AppSync adds to every schema.
var prelude = &ast.Source{Name: "appsync", BuiltIn: true, Input: `
scalar AWSDate
scalar AWSTime
scalar AWSDateTime
scalar AWSTimestamp
scalar AWSEmail
scalar AWSJSON
scalar AWSURL
scalar AWSPhone
scalar AWSIPAddress
directive @aws_api_key on OBJECT | FIELD_DEFINITION
directive @aws_iam on OBJECT | FIELD_DEFINITION
directive @aws_oidc on OBJECT | FIELD_DEFINITION
directive @aws_lambda on OBJECT | FIELD_DEFINITION
directive @aws_cognito_user_pools(cognito_groups: [String]) on OBJECT | FIELD_DEFINITION
directive @aws_auth(cognito_groups: [String]) on FIELD_DEFINITION
directive @aws_subscribe(mutations: [String]) on FIELD_DEFINITION
directive @canonical on OBJECT
directive @hidden on OBJECT | FIELD_DEFINITION
directive @renamed(to: String) on OBJECT | FIELD_DEFINITION
`}

// schemaRecord is an API's schema: its SDL and how the last
// StartSchemaCreation went.
type schemaRecord struct {
	Definition string
	Status     string
	Details    string
}

func schemaError(format string, args ...any) error {
	return awserror.New("GraphQLSchemaException", fmt.Sprintf(format, args...))
}

// load validates SDL as a complete schema.
func load(sdl string) (*ast.Schema, error) {
	return validator.LoadSchema(validator.Prelude, prelude, &ast.Source{Name: "schema", Input: sdl})
}

// format renders the definitions of doc as SDL.
func format(doc *ast.SchemaDocument) string {
	var b strings.Builder
	formatter.NewFormatter(&b, formatter.WithIndent("  ")).FormatSchemaDocument(doc)
	return b.String()
}

// readSchema reads the schema of the API id names, which has none until a
// StartSchemaCreation succeeds or a type is created.
func readSchema(tx storage.ReadTx, scope request.Scope, id *string) (*schemaRecord, error) {
	if _, err := loadAPI(tx, scope, id); err != nil {
		return nil, err
	}

	r, _, err := storage.Read[schemaRecord](tx, scope.Key(service, kindSchema, *id))
	if r == nil {
		r = &schemaRecord{}
	}

	return r, err
}

// Schema returns the validated schema of an API and whether it has one.
func activeSchema(tx storage.ReadTx, scope request.Scope, id *string) (*ast.Schema, bool, error) {
	r, err := readSchema(tx, scope, id)
	if err != nil || r.Definition == "" {
		return nil, false, err
	}

	schema, err := load(r.Definition)
	return schema, err == nil, err
}

func (s *Service) StartSchemaCreation(
	ctx context.Context,
	in *api.StartSchemaCreationRequest,
) (*api.StartSchemaCreationResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readSchema(tx, scope, in.ApiId)
		if err != nil {
			return err
		}

		// A failed schema leaves the previous one in place, as AppSync does.
		schema, loadErr := load(string(in.Definition))
		if loadErr != nil {
			r.Status, r.Details = statusFailed, loadErr.Error()
		} else {
			r.Definition, r.Status = string(in.Definition), statusSuccess
			r.Details = fmt.Sprintf("Successfully created schema with %d types.", userTypes(schema))
		}

		return storage.Write(tx, scope.Key(service, kindSchema, *in.ApiId), r)
	})
	if err != nil {
		return nil, err
	}

	// The schema is created at once; GetSchemaCreationStatus reports how.
	return &api.StartSchemaCreationResponse{Status: new("PROCESSING")}, nil
}

// userTypes counts the types a schema defines beyond the built-in ones.
func userTypes(schema *ast.Schema) int {
	n := 0
	for _, d := range schema.Types {
		if !d.BuiltIn {
			n++
		}
	}

	return n
}

func (s *Service) GetSchemaCreationStatus(
	ctx context.Context,
	in *api.GetSchemaCreationStatusRequest,
) (*api.GetSchemaCreationStatusResponse, error) {
	out := &api.GetSchemaCreationStatusResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, err := readSchema(tx, scope, in.ApiId)
		if err != nil {
			return err
		}

		if r.Status == "" {
			return notFound("No schema creation has been started for API %s.", *in.ApiId)
		}

		out.Status, out.Details = new(r.Status), new(r.Details)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetIntrospectionSchema(
	ctx context.Context,
	in *api.GetIntrospectionSchemaRequest,
) (*api.GetIntrospectionSchemaResponse, error) {
	out := &api.GetIntrospectionSchemaResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		schema, ok, err := activeSchema(tx, scope, in.ApiId)
		if err != nil || !ok {
			return common.ErrorsOr(err, schemaError("The API %s has no schema.", *in.ApiId))
		}

		switch common.Value(in.Format) {
		case formatSDL:
			out.Schema = []byte(sdl(schema, common.Value(in.IncludeDirectives)))
		case formatJSON:
			err = badRequest("TinyStack supports the SDL format only.")
		default:
			err = badRequest("format must be SDL or JSON.")
		}

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// sdl renders a schema's own types, with or without their directives.
func sdl(schema *ast.Schema, directives bool) string {
	doc := &ast.SchemaDocument{}
	for _, d := range schema.Types {
		if d.BuiltIn {
			continue
		}

		if !directives {
			d = stripDirectives(d)
		}

		doc.Definitions = append(doc.Definitions, d)
	}

	slices.SortFunc(doc.Definitions, func(a, b *ast.Definition) int { return strings.Compare(a.Name, b.Name) })
	doc.Schema = append(doc.Schema, &ast.SchemaDefinition{})
	for op, d := range map[ast.Operation]*ast.Definition{
		ast.Query: schema.Query, ast.Mutation: schema.Mutation, ast.Subscription: schema.Subscription,
	} {
		if d != nil {
			doc.Schema[0].OperationTypes = append(doc.Schema[0].OperationTypes,
				&ast.OperationTypeDefinition{Operation: op, Type: d.Name})
		}
	}

	slices.SortFunc(doc.Schema[0].OperationTypes, func(a, b *ast.OperationTypeDefinition) int {
		return strings.Compare(string(a.Operation), string(b.Operation))
	})

	return format(doc)
}

// stripDirectives copies d without the directives on it and its fields.
func stripDirectives(d *ast.Definition) *ast.Definition {
	c := *d
	c.Directives, c.Fields = nil, make(ast.FieldList, 0, len(d.Fields))
	for _, f := range d.Fields {
		fc := *f
		fc.Directives = nil
		c.Fields = append(c.Fields, &fc)
	}

	return &c
}

// editTypes applies fn to the definitions of an API's schema and stores the
// result, which must still be a valid schema.
func (s *Service) editTypes(
	ctx context.Context,
	apiID *string,
	fn func(doc *ast.SchemaDocument, scope request.Scope) error,
) error {
	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readSchema(tx, scope, apiID)
		if err != nil {
			return err
		}

		doc, err := parser.ParseSchema(&ast.Source{Input: r.Definition})
		if err != nil {
			return badRequest("%s", err.Error())
		}

		if fnErr := fn(doc, scope); fnErr != nil {
			return fnErr
		}

		r.Definition = format(doc)
		if _, err = load(r.Definition); err != nil {
			return badRequest("%s", err.Error())
		}

		return storage.Write(tx, scope.Key(service, kindSchema, *apiID), r)
	})
}

// parseType parses an SDL type definition, the one CreateType or UpdateType
// sends.
func parseType(definition, format *string) (*ast.Definition, error) {
	if common.Value(format) != formatSDL {
		return nil, badRequest("TinyStack supports type definitions in SDL format only.")
	}

	doc, err := parser.ParseSchema(&ast.Source{Input: common.Value(definition)})
	if err != nil {
		return nil, badRequest("%s", err.Error())
	}

	if len(doc.Definitions) != 1 || len(doc.Extensions)+len(doc.Schema)+len(doc.Directives) != 0 {
		return nil, badRequest("The definition must define exactly one type.")
	}

	return doc.Definitions[0], nil
}

func typeOf(scope request.Scope, apiID string, d *ast.Definition) *api.Type {
	t := &api.Type{
		Name: new(d.Name), Arn: new(apiARN(scope, apiID) + "/types/" + d.Name), Format: new(formatSDL),
		Definition: new(format(&ast.SchemaDocument{Definitions: ast.DefinitionList{d}})),
	}
	if d.Description != "" {
		t.Description = new(d.Description)
	}

	return t
}

func (s *Service) CreateType(ctx context.Context, in *api.CreateTypeRequest) (*api.CreateTypeResponse, error) {
	d, err := parseType(in.Definition, in.Format)
	if err != nil {
		return nil, err
	}

	out := &api.CreateTypeResponse{}
	err = s.editTypes(ctx, in.ApiId, func(doc *ast.SchemaDocument, scope request.Scope) error {
		if doc.Definitions.ForName(d.Name) != nil {
			return badRequest("Type %s already exists.", d.Name)
		}

		doc.Definitions = append(doc.Definitions, d)
		out.Type = typeOf(scope, *in.ApiId, d)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateType(ctx context.Context, in *api.UpdateTypeRequest) (*api.UpdateTypeResponse, error) {
	var d *ast.Definition
	if in.Definition != nil {
		var err error
		if d, err = parseType(in.Definition, in.Format); err != nil {
			return nil, err
		}

		if d.Name != common.Value(in.TypeName) {
			return nil, badRequest("The definition must define type %s.", common.Value(in.TypeName))
		}
	}

	out := &api.UpdateTypeResponse{}
	err := s.editTypes(ctx, in.ApiId, func(doc *ast.SchemaDocument, scope request.Scope) error {
		i := slices.IndexFunc(doc.Definitions, func(t *ast.Definition) bool { return t.Name == *in.TypeName })
		if i < 0 {
			return notFound("Type %s not found.", *in.TypeName)
		}

		if d != nil {
			doc.Definitions[i] = d
		}

		out.Type = typeOf(scope, *in.ApiId, doc.Definitions[i])
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteType(ctx context.Context, in *api.DeleteTypeRequest) (*api.DeleteTypeResponse, error) {
	err := s.editTypes(ctx, in.ApiId, func(doc *ast.SchemaDocument, _ request.Scope) error {
		i := slices.IndexFunc(doc.Definitions, func(t *ast.Definition) bool { return t.Name == *in.TypeName })
		if i < 0 {
			return notFound("Type %s not found.", *in.TypeName)
		}

		doc.Definitions = slices.Delete(doc.Definitions, i, i+1)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteTypeResponse{}, nil
}

// types returns the types an API's schema defines, in name order.
func types(tx storage.ReadTx, scope request.Scope, apiID *string) ([]*ast.Definition, error) {
	r, err := readSchema(tx, scope, apiID)
	if err != nil || r.Definition == "" {
		return nil, err
	}

	doc, err := parser.ParseSchema(&ast.Source{Input: r.Definition})
	if err != nil {
		return nil, err
	}

	out := slices.Clone(doc.Definitions)
	slices.SortFunc(out, func(a, b *ast.Definition) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *Service) GetType(ctx context.Context, in *api.GetTypeRequest) (*api.GetTypeResponse, error) {
	if common.Value(in.Format) != formatSDL {
		return nil, badRequest("TinyStack supports type definitions in SDL format only.")
	}

	out := &api.GetTypeResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := types(tx, scope, in.ApiId)
		if err != nil {
			return err
		}

		i := slices.IndexFunc(all, func(t *ast.Definition) bool { return t.Name == common.Value(in.TypeName) })
		if i < 0 {
			return notFound("Type %s not found.", common.Value(in.TypeName))
		}

		out.Type = typeOf(scope, *in.ApiId, all[i])
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) ListTypes(ctx context.Context, in *api.ListTypesRequest) (*api.ListTypesResponse, error) {
	if common.Value(in.Format) != formatSDL {
		return nil, badRequest("TinyStack supports type definitions in SDL format only.")
	}

	out := &api.ListTypesResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		all, err := types(tx, scope, in.ApiId)
		if err != nil {
			return err
		}

		listed := make([]api.Type, 0, len(all))
		for _, d := range all {
			listed = append(listed, *typeOf(scope, *in.ApiId, d))
		}

		out.Types, out.NextToken, err = page(listed, func(t api.Type) string { return *t.Name }, in.NextToken,
			in.MaxResults)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
