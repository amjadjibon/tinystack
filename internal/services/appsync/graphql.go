package appsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/dop251/goja"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/formatter"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/validator"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// query is the body of a GraphQL request.
type query struct {
	eventOperation string
	eventChannel   string
	Variables      map[string]any `json:"variables"`
	Query          string         `json:"query"`
	OperationName  string         `json:"operationName"`
}

// graph is what running a request needs of an API, read in one transaction:
// resolvers call other services, so they run outside it.
type graph struct {
	api       *api.GraphqlApi
	schema    *ast.Schema
	resolvers map[string]*api.Resolver
	sources   map[string]*api.DataSource
	functions map[string]*api.FunctionConfiguration
	// keyValid reports whether the request's x-api-key is a live key.
	keyValid bool
}

// load reads the API id names in scope, checking apiKey against its keys.
func (s *Service) load(ctx context.Context, scope request.Scope, id, apiKey string) (*graph, error) {
	g := &graph{
		resolvers: map[string]*api.Resolver{}, sources: map[string]*api.DataSource{},
		functions: map[string]*api.FunctionConfiguration{},
	}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		if g.api, err = loadAPI(tx, scope, &id); err != nil {
			return err
		}

		if g.schema, _, err = activeSchema(tx, scope, &id); err != nil {
			return err
		}

		resolvers, err := storage.Records[api.Resolver](tx, owned(scope, kindResolver, id, ""))
		if err != nil {
			return err
		}

		for _, r := range resolvers {
			g.resolvers[*r.TypeName+"."+*r.FieldName] = r
		}

		sources, err := storage.Records[api.DataSource](tx, owned(scope, kindDataSource, id, ""))
		if err != nil {
			return err
		}

		for _, d := range sources {
			g.sources[*d.Name] = d
		}

		functions, err := storage.Records[api.FunctionConfiguration](tx, owned(scope, kindFunction, id, ""))
		if err != nil {
			return err
		}

		for _, f := range functions {
			g.functions[*f.FunctionId] = f
		}

		if apiKey != "" {
			k, found, readErr := storage.Read[api.ApiKey](tx, owned(scope, kindAPIKey, id, apiKey))
			g.keyValid = found && s.now().Unix() < common.Value(k.Expires)
			return readErr
		}

		return nil
	})

	return g, err
}

// locate returns the scope of the API id names, which is empty when there
// is none.
func (s *Service) locate(ctx context.Context, id string) (request.Scope, error) {
	var scope request.Scope
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		l, found, err := storage.Read[location](tx, indexKey(id))
		if found {
			scope = request.Scope{AccountID: l.Account, Region: l.Region}
		}

		return err
	})

	return scope, err
}

// Execute runs a GraphQL request, POST /graphql, against the API id names.
func (s *Service) Execute(ctx context.Context, id string, r *execute.Request) (*execute.Response, bool, error) {
	if r.Path == "/event" {
		return s.executeEvent(ctx, id, r)
	}

	scope, err := s.locate(ctx, id)
	if err != nil || scope.AccountID == "" {
		return nil, false, err
	}

	if r.Method != http.MethodPost || r.Path != graphqlPath {
		return execute.Message(http.StatusNotFound, "Not Found"), true, nil
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
	var q query
	if err = json.Unmarshal(r.Body, &q); err != nil || q.Query == "" {
		return respond(http.StatusBadRequest, nil, []*gqlError{{
			ErrorType: new("MalformedHttpRequestException"), Message: "The request body is not a GraphQL request.",
		}})
	}

	g, err := s.load(ctx, scope, id, r.Header.Get("X-Api-Key"))
	if err != nil {
		return nil, true, err
	}

	c, err := s.authorize(ctx, g, r, &q)
	if err != nil {
		fe, ok := errors.AsType[*fieldError](err)
		if !ok {
			return nil, true, err
		}

		return respond(http.StatusUnauthorized, nil, []*gqlError{{ErrorType: new(fe.Type), Message: fe.Message}})
	}

	if g.schema == nil {
		return respond(http.StatusOK, nil, []*gqlError{{Message: "The API has no schema."}})
	}

	e, errs := s.prepare(g, c, r, &q)
	if errs != nil {
		return respond(http.StatusOK, nil, errs)
	}

	if e.op.Operation == ast.Subscription {
		return respond(http.StatusOK, nil, []*gqlError{{
			ErrorType: new("UnsupportedOperation"),
			Message:   "Subscriptions are served by the API's real-time endpoint, " + g.api.Uris[typeRealtime] + ".",
		}})
	}

	data, errs := e.run(ctx)
	if e.op.Operation == ast.Mutation {
		s.publish(ctx, id, data)
	}

	return respond(http.StatusOK, data, errs)
}

// result is a GraphQL response's data and errors.
type result struct {
	Data   any         `json:"data"`
	Errors []*gqlError `json:"errors,omitempty"`
}

func respond(status int, data any, errs []*gqlError) (*execute.Response, bool, error) {
	body, err := json.Marshal(result{data, errs})
	if err != nil {
		return nil, true, err
	}

	return &execute.Response{
		Status: status, Body: body, Header: http.Header{"Content-Type": {"application/json; charset=UTF-8"}},
	}, true, nil
}

// prepare validates the request's query, picks its operation, and coerces
// its variables, returning the executor that runs it.
func (s *Service) prepare(g *graph, c *caller, r *execute.Request, q *query) (*executor, []*gqlError) {
	doc, list := gqlparser.LoadQueryWithRules(g.schema, q.Query, nil)
	if len(list) > 0 {
		return nil, validationErrors(list)
	}

	op := doc.Operations.ForName(q.OperationName)
	if op == nil {
		return nil, []*gqlError{{ErrorType: new("BadRequestException"), Message: fmt.Sprintf(
			"The query does not have an operation named %q.", q.OperationName)}}
	}

	vars, err := validator.VariableValues(g.schema, op, q.Variables)
	if err != nil {
		return nil, validationErrors(gqlerror.List{gqlerror.WrapIfUnwrapped(err)})
	}

	if limit := common.Value(g.api.QueryDepthLimit); limit > 0 && depth(op.SelectionSet, 0) > int(limit) {
		return nil, []*gqlError{{ErrorType: new("ValidationError"), Message: fmt.Sprintf(
			"The query exceeds the API's depth limit of %d.", limit)}}
	}

	e := &executor{
		schema: g.schema, doc: doc, op: op, vars: vars, allowed: c.allowed(*g.api.AuthenticationType),
		noIntrospection: common.Value(g.api.IntrospectionConfig) == "DISABLED",
	}
	ses := &session{service: s, graph: g, caller: c, request: r, vars: vars}
	e.resolve, e.prefetch = ses.resolve, ses.prefetch
	return e, nil
}

func validationErrors(list gqlerror.List) []*gqlError {
	out := make([]*gqlError, 0, len(list))
	for _, err := range list {
		ge := &gqlError{ErrorType: new("ValidationError"), Message: err.Message, Locations: []sourceLocation{}}
		for _, l := range err.Locations {
			ge.Locations = append(ge.Locations, sourceLocation{Line: l.Line, Column: l.Column})
		}

		out = append(out, ge)
	}

	return out
}

// depth is how deeply a selection set nests fields.
func depth(set ast.SelectionSet, level int) int {
	deepest := level
	for _, sel := range set {
		var inner ast.SelectionSet
		next := level
		switch s := sel.(type) {
		case *ast.Field:
			inner, next = s.SelectionSet, level+1
		case *ast.FragmentSpread:
			if s.Definition != nil {
				inner = s.Definition.SelectionSet
			}
		case *ast.InlineFragment:
			inner = s.SelectionSet
		}

		deepest = max(deepest, depth(inner, next))
	}

	return deepest
}

// session is one request's resolvers.
type session struct {
	service *Service
	graph   *graph
	caller  *caller
	request *execute.Request
	vars    map[string]any
	// batched are the fields prefetch resolved, by path.
	batched map[string]*outcome
	// filter is the enhanced filter a subscription's resolver set.
	filter   any
	resolved int
}

// resolve runs the resolver of a field, if it has one.
func (r *session) resolve(ctx context.Context, res *resolution) (value any, handled bool, err error) {
	resolver := r.graph.resolvers[res.parent.Name+"."+res.def.Name]
	if resolver == nil {
		return nil, false, nil
	}

	if o, ok := r.batched[pathString(res.path)]; ok {
		res.appended = append(res.appended, o.appended...)
		return o.value, true, o.err
	}

	r.resolved++
	if limit := common.Value(r.graph.api.ResolverCountLimit); limit > 0 && r.resolved > int(limit) {
		return nil, true, &fieldError{Type: "ResolverCountLimitExceeded", Message: fmt.Sprintf(
			"The request ran more than the API's limit of %d resolvers.", limit)}
	}

	switch {
	case resolver.Runtime != nil:
		value, err = r.runJS(ctx, res, resolver)
		return value, true, err
	case resolver.RequestMappingTemplate != nil || resolver.ResponseMappingTemplate != nil ||
		common.Value(resolver.Kind) == kindPipeline:
		value, err = r.runVTL(ctx, res, resolver)
		return value, true, err
	}

	source := r.graph.sources[common.Value(resolver.DataSourceName)]
	if source == nil || common.Value(source.Type) != sourceLambda {
		return nil, true, &fieldError{Type: errorUnsupported, Message: fmt.Sprintf(
			"The resolver of %s.%s has neither code nor mapping templates, so it needs a Lambda data source.",
			res.parent.Name, res.def.Name)}
	}

	value, err = r.invoke(ctx, *source.LambdaConfig.LambdaFunctionArn, r.event(res),
		common.Value(resolver.MaxBatchSize) > 0)
	return value, true, err
}

// runJS runs an APPSYNC_JS resolver: its request handler, then its data
// source or, in a pipeline, each function's request handler, data source, and
// response handler, then its response handler.
func (r *session) runJS(ctx context.Context, res *resolution, resolver *api.Resolver) (any, error) {
	event := r.event(res)
	event["prev"], event[keyResult], event["error"] = map[string]any{keyResult: nil}, nil, nil
	j, err := newJS(event, &res.appended)
	if err != nil {
		return nil, err
	}

	defer func() {
		j.stop()
		if j.filter != nil {
			r.filter = j.filter
		}
	}()

	h, err := j.load(*resolver.Code)
	if err != nil {
		return nil, err
	}

	before, err := j.call(h.request)
	if errors.Is(err, errThrown) {
		return j.export(j.early)
	}

	if err != nil {
		return nil, err
	}

	if common.Value(resolver.Kind) == kindPipeline {
		if pipelineErr := r.pipeline(ctx, j, before, resolver.PipelineConfig); pipelineErr != nil {
			return nil, pipelineErr
		}
	} else if stepErr := r.step(ctx, j, r.graph.sources[common.Value(resolver.DataSourceName)], before); stepErr != nil {
		return nil, stepErr
	}

	out, err := j.call(h.response)
	if errors.Is(err, errThrown) {
		out, err = j.early, nil
	}

	if err != nil {
		return nil, err
	}

	return j.export(out)
}

// pipeline runs a pipeline's functions in order, each seeing the one before's
// result as ctx.prev.result, the first the resolver's request handler's.
func (r *session) pipeline(ctx context.Context, j *jsRun, before goja.Value, config *api.PipelineConfig) error {
	prev := before
	for _, id := range common.OrElse(config, &api.PipelineConfig{}).Functions {
		if err := j.setPrev(prev); err != nil {
			return err
		}

		f := r.graph.functions[id]
		switch {
		case f == nil:
			return &fieldError{Type: errorCode, Message: "Function " + id + " no longer exists."}
		case f.Runtime == nil:
			return &fieldError{Type: errorUnsupported, Message: fmt.Sprintf(
				"TinyStack does not run functions with mapping templates, such as %s, in an APPSYNC_JS pipeline.",
				*f.Name)}
		}

		h, err := j.load(*f.Code)
		if err != nil {
			return err
		}

		req, err := j.call(h.request)
		if errors.Is(err, errThrown) {
			prev = j.early
			continue
		}

		if err != nil {
			return err
		}

		if stepErr := r.step(ctx, j, r.graph.sources[*f.DataSourceName], req); stepErr != nil {
			return stepErr
		}

		if prev, err = j.call(h.response); errors.Is(err, errThrown) {
			prev, err = j.early, nil
		}

		if err != nil {
			return err
		}
	}

	return j.setPrev(prev)
}

// step sends a request handler's result to a data source and sets ctx.result
// and ctx.error for the response handler.
func (r *session) step(ctx context.Context, j *jsRun, d *api.DataSource, req goja.Value) error {
	if d == nil {
		return &fieldError{Type: errorCode, Message: "The data source no longer exists."}
	}

	payload, err := j.export(req)
	if err != nil {
		return err
	}

	result, err := r.invokeSource(ctx, d, payload)
	failure, isSource := errors.AsType[*sourceError](err)
	if err != nil && !isSource {
		return err
	}

	if setErr := j.set(keyResult, result); setErr != nil {
		return setErr
	}

	return j.set("error", failure)
}

// event is the payload a direct Lambda resolver receives.
func (r *session) event(res *resolution) map[string]any {
	var fields []string
	for _, f := range res.group.fields {
		fields = selectionList(f.SelectionSet, "", fields)
	}

	var b strings.Builder
	formatter.NewFormatter(&b, formatter.WithIndent("  ")).FormatQueryDocument(&ast.QueryDocument{
		Operations: ast.OperationList{{SelectionSet: res.group.fields[0].SelectionSet}},
	})
	return map[string]any{
		"arguments": res.args, "identity": r.caller.identity, "source": res.source,
		"request": map[string]any{"headers": headers(r.request), "domainName": nil},
		"prev":    nil, "stash": map[string]any{},
		"info": map[string]any{
			"fieldName": res.def.Name, "parentTypeName": res.parent.Name, "variables": r.vars,
			"selectionSetList": fields, "selectionSetGraphQL": strings.TrimSpace(b.String()),
		},
	}
}

// selectionList lists the fields a selection set reads, as paths such as
// "author/name", in the order it names them.
func selectionList(set ast.SelectionSet, prefix string, out []string) []string {
	for _, sel := range set {
		switch s := sel.(type) {
		case *ast.Field:
			if strings.HasPrefix(s.Name, "__") {
				continue
			}

			path := prefix + s.Name
			if !slices.Contains(out, path) {
				out = append(out, path)
			}

			out = selectionList(s.SelectionSet, path+"/", out)
		case *ast.FragmentSpread:
			if s.Definition != nil {
				out = selectionList(s.Definition.SelectionSet, prefix, out)
			}
		case *ast.InlineFragment:
			out = selectionList(s.SelectionSet, prefix, out)
		}
	}

	return out
}

// invoke calls a function with event, as a batch of one when batch is set,
// and returns the field's value.
func (r *session) invoke(ctx context.Context, arn string, event any, batch bool) (any, error) {
	if !batch {
		return r.lambda(ctx, arn, event)
	}

	results, err := r.invokeBatch(ctx, arn, []any{event})
	if err != nil {
		return nil, err
	}

	return results[0], nil
}

// invokeBatch calls a function with a batch of payloads, which must return
// one result for each, in order.
func (r *session) invokeBatch(ctx context.Context, arn string, payloads []any) ([]any, error) {
	value, err := r.lambda(ctx, arn, payloads)
	if err != nil {
		return nil, err
	}

	results, ok := value.([]any)
	if !ok || len(results) != len(payloads) {
		return nil, &fieldError{Type: typeLambdaUnhandled, Message: fmt.Sprintf(
			"The function must return a list of %d results, one for each item of the batch.", len(payloads))}
	}

	return results, nil
}

// lambda calls a function with payload and returns what it returned.
func (r *session) lambda(ctx context.Context, arn string, payload any) (any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	out, err := r.service.functions.Invoke(ctx, &lambdaapi.InvocationRequest{FunctionName: &arn, Payload: body})
	if err != nil {
		return nil, &fieldError{Type: "Lambda:IllegalArgument", Message: err.Error()}
	}

	if out.FunctionError != nil {
		var failure struct {
			ErrorMessage string `json:"errorMessage"`
			ErrorType    string `json:"errorType"`
		}
		_ = json.Unmarshal(out.Payload, &failure)
		return nil, &fieldError{Type: typeLambdaUnhandled, Message: failure.ErrorMessage}
	}

	var value any
	if err = json.Unmarshal(out.Payload, &value); err != nil {
		return nil, &fieldError{Type: typeLambdaUnhandled, Message: "The function returned invalid JSON."}
	}

	return value, nil
}
