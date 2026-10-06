package appsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/common"
)

// outcome is a field a batch resolved.
type outcome struct {
	value    any
	err      error
	appended []*fieldError
}

// pending is an item of a batch: the payload its request built, and what
// turns the batch's result for it into the field's value.
type pending struct {
	finish  func(result any, failure *sourceError) (any, error)
	payload any
	res     *resolution
}

// prefetch resolves a field of every object of a list together when its
// resolver batches: a unit resolver on a Lambda data source with a
// maxBatchSize, called with BatchInvoke (always, for a direct Lambda
// resolver). Each item's request runs, then the function gets up to
// maxBatchSize payloads at a time, and then each item's response runs.
func (r *session) prefetch(ctx context.Context, parent *ast.Definition, def *ast.FieldDefinition, g group,
	sources []any, paths [][]any,
) {
	resolver := r.graph.resolvers[parent.Name+"."+def.Name]
	if resolver == nil || common.Value(resolver.MaxBatchSize) <= 0 || common.Value(resolver.Kind) == kindPipeline {
		return
	}

	d := r.graph.sources[common.Value(resolver.DataSourceName)]
	if d == nil || common.Value(d.Type) != sourceLambda {
		return
	}

	if r.batched == nil {
		r.batched = map[string]*outcome{}
	}

	var waiting []*pending
	for i, source := range sources {
		res := &resolution{
			source: source, args: g.fields[0].ArgumentMap(r.vars), parent: parent, def: def, group: g, path: paths[i],
		}
		key := pathString(paths[i])
		r.resolved++
		if limit := common.Value(r.graph.api.ResolverCountLimit); limit > 0 && r.resolved > int(limit) {
			r.batched[key] = &outcome{err: &fieldError{Type: "ResolverCountLimitExceeded", Message: fmt.Sprintf(
				"The request ran more than the API's limit of %d resolvers.", limit)}}
			continue
		}

		p, done, err := r.prepareBatch(res, resolver)
		switch {
		case err != nil || done != nil:
			r.batched[key] = &outcome{value: done, err: err, appended: res.appended}
		case p == nil:
			// Its request is not a BatchInvoke, so resolve runs it alone.
			r.resolved--
		default:
			waiting = append(waiting, p)
		}
	}

	size := int(*resolver.MaxBatchSize)
	for start := 0; start < len(waiting); start += size {
		chunk := waiting[start:min(start+size, len(waiting))]
		payloads := make([]any, len(chunk))
		for i, p := range chunk {
			payloads[i] = p.payload
		}

		results, err := r.invokeBatch(ctx, *d.LambdaConfig.LambdaFunctionArn, payloads)
		var failure *sourceError
		if fe, ok := errors.AsType[*fieldError](err); ok {
			failure = &sourceError{Type: fe.Type, Message: fe.Message}
		} else if err != nil {
			failure = &sourceError{Type: typeLambdaUnhandled, Message: err.Error()}
		}

		for i, p := range chunk {
			var result any
			if failure == nil {
				result = results[i]
			}

			value, finishErr := p.finish(result, failure)
			r.batched[pathString(p.res.path)] = &outcome{value: value, err: finishErr, appended: p.res.appended}
		}
	}
}

// prepareBatch runs an item's request, returning the pending item, or the
// field's value when the request settled it, or neither when its request
// does not batch.
func (r *session) prepareBatch(res *resolution, resolver *api.Resolver) (*pending, any, error) {
	event := r.event(res)
	raise := func(result any, failure *sourceError) (any, error) {
		if failure != nil {
			return nil, &fieldError{Type: failure.Type, Message: failure.Message}
		}

		return result, nil
	}
	switch {
	case resolver.Runtime != nil:
		return r.prepareJS(res, resolver, event)
	case resolver.RequestMappingTemplate == nil && resolver.ResponseMappingTemplate == nil:
		return &pending{res: res, payload: event, finish: raise}, nil, nil
	}

	event["prev"], event[keyResult], event["error"] = map[string]any{keyResult: nil}, nil, nil
	v := r.newVTL(event, &res.appended)
	req := map[string]any{"operation": opBatchInvoke, "payload": event}
	if resolver.RequestMappingTemplate != nil {
		rendered, returned, err := v.render(*resolver.RequestMappingTemplate)
		if err != nil || returned {
			return nil, rendered, err
		}

		req, _ = rendered.(map[string]any)
	}

	if op, _ := req["operation"].(string); op != opBatchInvoke {
		return nil, nil, nil
	}

	version, _ := req["version"].(string)
	return &pending{res: res, payload: req["payload"], finish: func(result any, failure *sourceError) (any, error) {
		return v.respond(result, failure, version, resolver.ResponseMappingTemplate)
	}}, nil, nil
}

func (r *session) prepareJS(res *resolution, resolver *api.Resolver, event map[string]any) (*pending, any, error) {
	event["prev"], event[keyResult], event["error"] = map[string]any{keyResult: nil}, nil, nil
	j, err := newJS(event, &res.appended)
	if err != nil {
		return nil, nil, err
	}

	h, err := j.load(*resolver.Code)
	if err != nil {
		j.stop()
		return nil, nil, err
	}

	before, err := j.call(h.request)
	if errors.Is(err, errThrown) {
		j.stop()
		value, exportErr := j.export(j.early)
		return nil, value, exportErr
	}

	if err != nil {
		j.stop()
		return nil, nil, err
	}

	req, err := j.export(before)
	m, _ := req.(map[string]any)
	if op, _ := m["operation"].(string); err != nil || op != opBatchInvoke {
		j.stop()
		return nil, nil, err
	}

	return &pending{res: res, payload: m["payload"], finish: func(result any, failure *sourceError) (any, error) {
		defer j.stop()
		if setErr := j.set(keyResult, result); setErr != nil {
			return nil, setErr
		}

		if setErr := j.set("error", failure); setErr != nil {
			return nil, setErr
		}

		out, callErr := j.call(h.response)
		if errors.Is(callErr, errThrown) {
			out, callErr = j.early, nil
		}

		if callErr != nil {
			return nil, callErr
		}

		return j.export(out)
	}}, nil, nil
}
