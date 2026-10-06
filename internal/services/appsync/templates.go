package appsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/appsync/vtl"
)

// versionRaising is the mapping template version whose data source errors
// end a resolver before its response template runs.
const versionRaising = "2017-02-28"

// templates caches parsed mapping templates by their source.
var templates sync.Map

func parseTemplate(src string) (*vtl.Template, error) {
	if t, ok := templates.Load(src); ok {
		tpl, _ := t.(*vtl.Template)
		return tpl, nil
	}

	t, err := vtl.Parse(src)
	if err != nil {
		return nil, err
	}

	templates.Store(src, t)
	return t, nil
}

// vtlRun renders one field's mapping templates, which share $ctx and its
// stash.
type vtlRun struct {
	vars map[string]any
	ctx  map[string]any
}

func (r *session) newVTL(event map[string]any, appended *[]*fieldError) *vtlRun {
	ctx, _ := vtl.FromJSON(normalize(event)).(map[string]any)
	ctx["args"] = ctx["arguments"]
	v := &vtlRun{ctx: ctx}
	v.vars = map[string]any{
		"ctx": ctx, "context": ctx, "util": r.vtlUtil(appended),
		"extensions": map[string]any{
			"setSubscriptionFilter": vtl.Func(func(args []any) (any, error) {
				r.filter = vtl.ToJSON(arg(args, 0))
				return "", nil
			}),
			"setSubscriptionInvalidationFilter": unsupportedFunc("subscription invalidation"),
			"invalidateSubscriptions":           unsupportedFunc("subscription invalidation"),
		},
	}

	return v
}

func unsupportedFunc(what string) vtl.Func {
	return func([]any) (any, error) {
		return nil, &fieldError{Type: errorUnsupported, Message: "TinyStack does not support " + what + "."}
	}
}

// set replaces a member of $ctx with a JSON value.
func (v *vtlRun) set(name string, value any) { v.ctx[name] = vtl.FromJSON(normalize(value)) }

// render renders a template, returning its JSON value, or the value it
// returned with #return.
func (v *vtlRun) render(src string) (value any, returned bool, err error) {
	t, err := parseTemplate(src)
	if err != nil {
		return nil, false, &fieldError{Type: typeMappingTemplate, Message: err.Error()}
	}

	out, early, returned, err := t.Render(v.vars)
	if err != nil {
		if _, ok := errors.AsType[*fieldError](err); ok {
			return nil, false, err
		}

		return nil, false, &fieldError{Type: typeMappingTemplate, Message: err.Error()}
	}

	if returned {
		return vtl.ToJSON(early), true, nil
	}

	text := strings.TrimSpace(out)
	if text == "" {
		return nil, false, nil
	}

	if err = json.Unmarshal([]byte(text), &value); err != nil {
		return nil, false, &fieldError{Type: typeMappingTemplate, Message: fmt.Sprintf(
			"Unable to convert %s to Object.", text)}
	}

	return value, false, nil
}

// step runs one data source call with mapping templates: the request
// template (by default a Lambda Invoke of the field's event), the data
// source, and the response template (by default its result).
func (r *session) vtlStep(ctx context.Context, v *vtlRun, d *api.DataSource, request, response *string) (
	any, error,
) {
	var req any = map[string]any{"operation": "Invoke", "payload": vtl.ToJSON(v.ctx)}
	if request != nil {
		var returned bool
		var err error
		if req, returned, err = v.render(*request); err != nil || returned {
			return req, err
		}
	}

	if d == nil {
		return nil, &fieldError{Type: errorCode, Message: "The data source no longer exists."}
	}

	result, err := r.invokeSource(ctx, d, req)
	failure, isSource := errors.AsType[*sourceError](err)
	if err != nil && !isSource {
		return nil, err
	}

	version, _ := req.(map[string]any)["version"].(string)
	return v.respond(result, failure, version, response)
}

// respond renders a response template with a data source's result or
// failure, or, without one, returns the result.
func (v *vtlRun) respond(result any, failure *sourceError, version string, response *string) (any, error) {
	v.set(keyResult, result)
	v.ctx["error"] = nil
	if failure != nil {
		v.set("error", failure)
	}

	if failure != nil && (response == nil || version == versionRaising) {
		return nil, &fieldError{Type: failure.Type, Message: failure.Message}
	}

	if response == nil {
		return result, nil
	}

	value, _, err := v.render(*response)
	return value, err
}

// runVTL runs a resolver with mapping templates: a unit resolver's request
// and response, or a pipeline's before template, functions, and after
// template.
func (r *session) runVTL(ctx context.Context, res *resolution, resolver *api.Resolver) (any, error) {
	event := r.event(res)
	event["prev"], event[keyResult], event["error"] = map[string]any{keyResult: nil}, nil, nil
	v := r.newVTL(event, &res.appended)
	if resolver.Kind == nil || *resolver.Kind != kindPipeline {
		return r.vtlStep(
			ctx,
			v,
			r.graph.sources[common.Value(resolver.DataSourceName)],
			resolver.RequestMappingTemplate,
			resolver.ResponseMappingTemplate,
		)
	}

	var prev any
	if resolver.RequestMappingTemplate != nil {
		before, returned, err := v.render(*resolver.RequestMappingTemplate)
		if err != nil || returned {
			return before, err
		}

		prev = before
	}

	for _, id := range pipelineFunctions(resolver.PipelineConfig) {
		v.set("prev", map[string]any{keyResult: prev})
		f := r.graph.functions[id]
		switch {
		case f == nil:
			return nil, &fieldError{Type: errorCode, Message: "Function " + id + " no longer exists."}
		case f.Runtime != nil:
			return nil, &fieldError{Type: errorUnsupported, Message: fmt.Sprintf(
				"TinyStack does not run APPSYNC_JS functions, such as %s, in a pipeline with mapping templates.",
				*f.Name)}
		}

		value, err := r.vtlStep(ctx, v, r.graph.sources[common.Value(f.DataSourceName)], f.RequestMappingTemplate,
			f.ResponseMappingTemplate)
		if err != nil {
			return nil, err
		}

		prev = value
	}

	v.set("prev", map[string]any{keyResult: prev})
	if resolver.ResponseMappingTemplate == nil {
		return prev, nil
	}

	value, _, err := v.render(*resolver.ResponseMappingTemplate)
	return value, err
}

func pipelineFunctions(c *api.PipelineConfig) []string {
	if c == nil {
		return nil
	}

	return c.Functions
}

func arg(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}

	return nil
}

func text(args []any, i int) string { return vtl.String(arg(args, i)) }

func numeric(v any) float64 {
	switch t := v.(type) {
	case int64:
		return float64(t)
	case float64:
		return t
	}

	return 0
}

func jsonOf(v any) (any, error) {
	b, err := json.Marshal(vtl.ToJSON(v))
	return string(b), err
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case *vtl.List:
		return len(t.Items) == 0
	case map[string]any:
		return len(t) == 0
	}

	return false
}

// toDynamoDB converts a value to a DynamoDB attribute value.
func toDynamoDB(v any) any {
	switch t := v.(type) {
	case nil:
		return map[string]any{attrNULL: true}
	case string:
		return map[string]any{"S": t}
	case int64, float64:
		return map[string]any{"N": vtl.String(t)}
	case bool:
		return map[string]any{attrBOOL: t}
	case *vtl.List:
		l := &vtl.List{}
		for _, e := range t.Items {
			l.Items = append(l.Items, toDynamoDB(e))
		}

		return map[string]any{"L": l}
	case map[string]any:
		return map[string]any{"M": toMapValues(t)}
	}

	return map[string]any{"S": vtl.String(v)}
}

func toMapValues(v any) any {
	m, _ := v.(map[string]any)
	out := map[string]any{}
	for k, e := range m {
		out[k] = toDynamoDB(e)
	}

	return out
}

// funcs builds a map of host functions.
func funcs(fns map[string]func(args []any) (any, error)) map[string]any {
	out := make(map[string]any, len(fns))
	for k, f := range fns {
		out[k] = vtl.Func(f)
	}

	return out
}

// dynamoFuncs are $util.dynamodb's conversions, each with its Json form.
func dynamoFuncs() map[string]any {
	conversions := map[string]func(v any) any{
		"toDynamoDB": toDynamoDB, "toMapValues": toMapValues,
		"toString": func(v any) any { return map[string]any{"S": vtl.String(v)} },
		"toNumber": func(v any) any { return map[string]any{"N": vtl.String(v)} },
		"toBoolean": func(v any) any {
			return map[string]any{attrBOOL: vtl.Truthy(v)}
		},
		"toNull": func(any) any { return map[string]any{attrNULL: true} },
		"toList": toDynamoDB, "toMap": toDynamoDB,
		"toStringSet": func(v any) any { return map[string]any{"SS": v} },
		"toNumberSet": func(v any) any {
			l, _ := v.(*vtl.List)
			out := &vtl.List{}
			if l != nil {
				for _, e := range l.Items {
					out.Items = append(out.Items, vtl.String(e))
				}
			}

			return map[string]any{"NS": out}
		},
	}
	out := map[string]any{}
	for name, convert := range conversions {
		out[name] = vtl.Func(func(args []any) (any, error) { return convert(arg(args, 0)), nil })
		out[name+"Json"] = vtl.Func(func(args []any) (any, error) { return jsonOf(convert(arg(args, 0))) })
	}

	return out
}

// errorOf is the field error $util.error and $util.appendError describe.
func errorOf(args []any) *fieldError {
	fe := &fieldError{Message: text(args, 0), Data: vtl.ToJSON(arg(args, 2)), Info: vtl.ToJSON(arg(args, 3))}
	if t := arg(args, 1); t != nil {
		fe.Type = vtl.String(t)
	}

	return fe
}

// vtlUtil is $util: AppSync's utilities for mapping templates.
func (r *session) vtlUtil(appended *[]*fieldError) map[string]any {
	keep := func(retain bool) func(args []any) (any, error) {
		return func(args []any) (any, error) {
			m, _ := arg(args, 0).(map[string]any)
			l, _ := arg(args, 1).(*vtl.List)
			out := map[string]any{}
			for k, e := range m {
				listed := l != nil && slices.ContainsFunc(l.Items, func(x any) bool { return vtl.String(x) == k })
				if listed == retain {
					out[k] = e
				}
			}

			return out, nil
		}
	}
	keepItems := func(retain bool) func(args []any) (any, error) {
		return func(args []any) (any, error) {
			l, _ := arg(args, 0).(*vtl.List)
			other, _ := arg(args, 1).(*vtl.List)
			out := &vtl.List{}
			if l == nil {
				return out, nil
			}

			for _, e := range l.Items {
				listed := other != nil && slices.ContainsFunc(other.Items, func(x any) bool {
					return vtl.String(x) == vtl.String(e)
				})
				if listed == retain {
					out.Items = append(out.Items, e)
				}
			}

			return out, nil
		}
	}
	quiet := func([]any) (any, error) { return "", nil }
	util := funcs(map[string]func(args []any) (any, error){
		"qr": quiet, "quiet": quiet,
		"toJson": func(args []any) (any, error) { return jsonOf(arg(args, 0)) },
		"parseJson": func(args []any) (any, error) {
			var v any
			if err := json.Unmarshal([]byte(text(args, 0)), &v); err != nil {
				return nil, &fieldError{Type: typeMappingTemplate, Message: "Unable to parse the JSON document: " +
					err.Error()}
			}

			return vtl.FromJSON(v), nil
		},
		"autoId":   func([]any) (any, error) { return uuid.NewV4().String(), nil },
		"autoUlid": func([]any) (any, error) { return ulid(), nil },
		"error":    func(args []any) (any, error) { return nil, errorOf(args) },
		"appendError": func(args []any) (any, error) {
			*appended = append(*appended, errorOf(args))
			return "", nil
		},
		"unauthorized": func([]any) (any, error) {
			return nil, &fieldError{Type: typeUnauthorized, Message: typeUnauthorized}
		},
		"validate": func(args []any) (any, error) {
			if vtl.Truthy(arg(args, 0)) {
				return "", nil
			}

			fe := &fieldError{Type: "CustomTemplateException", Message: text(args, 1)}
			if t := arg(args, 2); t != nil {
				fe.Type = vtl.String(t)
			}

			return nil, fe
		},
		"isNull":        func(args []any) (any, error) { return arg(args, 0) == nil, nil },
		"isNullOrEmpty": func(args []any) (any, error) { return isEmpty(arg(args, 0)), nil },
		"isNullOrBlank": func(args []any) (any, error) {
			return arg(args, 0) == nil || strings.TrimSpace(text(args, 0)) == "", nil
		},
		"defaultIfNull": func(args []any) (any, error) {
			if arg(args, 0) == nil {
				return arg(args, 1), nil
			}

			return args[0], nil
		},
		"defaultIfNullOrEmpty": func(args []any) (any, error) {
			if isEmpty(arg(args, 0)) {
				return arg(args, 1), nil
			}

			return args[0], nil
		},
		"defaultIfNullOrBlank": func(args []any) (any, error) {
			if arg(args, 0) == nil || strings.TrimSpace(text(args, 0)) == "" {
				return arg(args, 1), nil
			}

			return args[0], nil
		},
		"isString":  func(args []any) (any, error) { _, ok := arg(args, 0).(string); return ok, nil },
		"isBoolean": func(args []any) (any, error) { _, ok := arg(args, 0).(bool); return ok, nil },
		"isList":    func(args []any) (any, error) { _, ok := arg(args, 0).(*vtl.List); return ok, nil },
		"isMap":     func(args []any) (any, error) { _, ok := arg(args, 0).(map[string]any); return ok, nil },
		"isNumber": func(args []any) (any, error) {
			switch arg(args, 0).(type) {
			case int64, float64:
				return true, nil
			}

			return false, nil
		},
		"typeOf": func(args []any) (any, error) {
			switch arg(args, 0).(type) {
			case nil:
				return "Null", nil
			case int64, float64:
				return "Number", nil
			case string:
				return "String", nil
			case bool:
				return "Boolean", nil
			case *vtl.List:
				return "List", nil
			case map[string]any:
				return "Map", nil
			}

			return "Object", nil
		},
		"escapeJavaScript": func(args []any) (any, error) {
			b, err := json.Marshal(text(args, 0))
			return strings.Trim(string(b), `"`), err
		},
		"urlEncode": func(args []any) (any, error) { return url.QueryEscape(text(args, 0)), nil },
		"urlDecode": func(args []any) (any, error) { return url.QueryUnescape(text(args, 0)) },
		"base64Encode": func(args []any) (any, error) {
			return base64.StdEncoding.EncodeToString([]byte(text(args, 0))), nil
		},
		"base64Decode": func(args []any) (any, error) {
			b, err := base64.StdEncoding.DecodeString(text(args, 0))
			return string(b), err
		},
		"matches": func(args []any) (any, error) {
			re, err := regexp.Compile("^(?:" + text(args, 0) + ")$")
			if err != nil {
				return nil, err
			}

			return re.MatchString(text(args, 1)), nil
		},
	})
	util["time"] = funcs(map[string]func(args []any) (any, error){
		"nowISO8601": func([]any) (any, error) {
			return time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), nil
		},
		"nowEpochSeconds":      func([]any) (any, error) { return time.Now().Unix(), nil },
		"nowEpochMilliSeconds": func([]any) (any, error) { return time.Now().UnixMilli(), nil },
		"parseISO8601ToEpochMilliSeconds": func(args []any) (any, error) {
			t, err := time.Parse(time.RFC3339Nano, text(args, 0))
			return t.UnixMilli(), err
		},
		"epochMilliSecondsToSeconds": func(args []any) (any, error) { return int64(numeric(arg(args, 0)) / 1000), nil },
		"epochMilliSecondsToISO8601": func(args []any) (any, error) {
			return time.UnixMilli(int64(numeric(arg(args, 0)))).UTC().Format("2006-01-02T15:04:05.000Z"), nil
		},
	})
	util["dynamodb"] = dynamoFuncs()
	util["map"] = funcs(map[string]func(args []any) (any, error){
		"copyAndRetainAllKeys": keep(true), "copyAndRemoveAllKeys": keep(false),
	})
	util["list"] = funcs(map[string]func(args []any) (any, error){
		"copyAndRetainAll": keepItems(true), "copyAndRemoveAll": keepItems(false),
	})
	util["str"] = funcs(map[string]func(args []any) (any, error){
		"toUpper": func(args []any) (any, error) { return strings.ToUpper(text(args, 0)), nil },
		"toLower": func(args []any) (any, error) { return strings.ToLower(text(args, 0)), nil },
		"toReplace": func(args []any) (any, error) {
			return strings.ReplaceAll(text(args, 0), text(args, 1), text(args, 2)), nil
		},
	})
	util["math"] = funcs(map[string]func(args []any) (any, error){
		"roundNum": func(args []any) (any, error) { return int64(math.Round(numeric(arg(args, 0)))), nil },
		"minVal": func(args []any) (any, error) {
			return math.Min(numeric(arg(args, 0)), numeric(arg(args, 1))), nil
		},
		"maxVal": func(args []any) (any, error) {
			return math.Max(numeric(arg(args, 0)), numeric(arg(args, 1))), nil
		},
		// #nosec G404 -- templates ask for random numbers, not secrets.
		"randomDouble": func([]any) (any, error) { return rand.Float64(), nil },
		"randomWithinRange": func(args []any) (any, error) {
			lo, hi := int64(numeric(arg(args, 0))), int64(numeric(arg(args, 1)))
			if hi <= lo {
				return lo, nil
			}

			// #nosec G404 -- templates ask for random numbers, not secrets.
			return lo + rand.Int64N(hi-lo+1), nil
		},
	})
	util["transform"] = funcs(map[string]func(args []any) (any, error){
		"toDynamoDBFilterExpression":    filterExpressionJSON,
		"toDynamoDBConditionExpression": filterExpressionJSON,
		"toSubscriptionFilter": func(args []any) (any, error) {
			ignored, _ := arg(args, 1).(*vtl.List)
			var skip []string
			if ignored != nil {
				for _, e := range ignored.Items {
					skip = append(skip, vtl.String(e))
				}
			}

			m, _ := vtl.ToJSON(arg(args, 0)).(map[string]any)
			var groups []any
			for _, g := range filterGroupsOf(m, skip) {
				groups = append(groups, map[string]any{"filters": g})
			}

			return vtl.FromJSON(map[string]any{"filterGroup": groups}), nil
		},
		"toElasticsearchQueryDSL": unsupportedFunc("OpenSearch query DSL"),
	})
	return util
}

// filterExpressionJSON is $util.transform.toDynamoDBFilterExpression: an
// Amplify filter object ({field: {operator: value}, and, or, not}) as a
// DynamoDB expression's JSON.
func filterExpressionJSON(args []any) (any, error) {
	m, _ := vtl.ToJSON(arg(args, 0)).(map[string]any)
	if m == nil {
		return nil, nil //nolint:nilnil // no filter is null.
	}

	names, values := map[string]any{}, map[string]any{}
	n := 0
	var build func(o map[string]any) (string, error)
	build = func(o map[string]any) (string, error) {
		var parts []string
		for _, field := range slices.Sorted(maps.Keys(o)) {
			v := o[field]
			switch field {
			case keyAnd, "or":
				list, _ := v.([]any)
				var subs []string
				for _, e := range list {
					sub, _ := e.(map[string]any)
					s, err := build(sub)
					if err != nil {
						return "", err
					}

					subs = append(subs, s)
				}

				parts = append(
					parts,
					"("+strings.Join(subs, map[string]string{keyAnd: " AND ", "or": " OR "}[field])+")",
				)
				continue
			case "not":
				sub, _ := v.(map[string]any)
				s, err := build(sub)
				if err != nil {
					return "", err
				}

				parts = append(parts, "(NOT "+s+")")
				continue
			}

			ops, _ := v.(map[string]any)
			name := "#" + regexp.MustCompile(`[^A-Za-z0-9_]`).ReplaceAllString(field, "_")
			names[name] = field
			value := func(x any) string {
				key := fmt.Sprintf(":v%d", n)
				n++
				values[key] = toDynamoDB(vtl.FromJSON(x))
				return key
			}
			for _, op := range slices.Sorted(maps.Keys(ops)) {
				x := ops[op]
				comparison := map[string]string{"eq": "=", "ne": "<>", "lt": "<", "le": "<=", "gt": ">", "ge": ">="}
				switch {
				case comparison[op] != "":
					parts = append(parts, name+" "+comparison[op]+" "+value(x))
				case op == "between":
					pair, _ := x.([]any)
					if len(pair) != 2 {
						return "", &fieldError{Type: typeMappingTemplate, Message: "between needs two values."}
					}

					parts = append(parts, name+" BETWEEN "+value(pair[0])+" AND "+value(pair[1]))
				case op == opBeginsWith:
					parts = append(parts, "begins_with("+name+", "+value(x)+")")
				case op == opContains:
					parts = append(parts, "contains("+name+", "+value(x)+")")
				case op == opNotContains:
					parts = append(parts, "NOT contains("+name+", "+value(x)+")")
				case op == "attributeExists":
					parts = append(
						parts,
						map[bool]string{true: "attribute_exists(", false: "attribute_not_exists("}[x == true]+
							name+")",
					)
				case op == "attributeType":
					parts = append(parts, "attribute_type("+name+", "+value(x)+")")
				default:
					return "", &fieldError{Type: errorUnsupported, Message: "TinyStack does not support the " + op +
						" filter operator."}
				}
			}
		}

		return strings.Join(parts, " AND "), nil
	}
	expression, err := build(m)
	if err != nil {
		return nil, err
	}

	return jsonOf(map[string]any{"expression": expression, "expressionNames": names, "expressionValues": values})
}

// filterGroupsOf converts a filter object into enhanced subscription filter
// groups, as util.transform.toSubscriptionFilter does in utils.js.
func filterGroupsOf(o map[string]any, ignored []string) [][]any {
	and := func(a, b [][]any) [][]any {
		var out [][]any
		for _, x := range a {
			for _, y := range b {
				out = append(out, append(slices.Clone(x), y...))
			}
		}

		return out
	}
	groups := [][]any{{}}
	for _, key := range slices.Sorted(maps.Keys(o)) {
		if slices.Contains(ignored, key) {
			continue
		}

		switch key {
		case "or":
			var alternatives [][]any
			list, _ := o[key].([]any)
			for _, e := range list {
				sub, _ := e.(map[string]any)
				alternatives = append(alternatives, filterGroupsOf(sub, ignored)...)
			}

			groups = and(groups, alternatives)
		case keyAnd:
			list, _ := o[key].([]any)
			for _, e := range list {
				sub, _ := e.(map[string]any)
				groups = and(groups, filterGroupsOf(sub, ignored))
			}
		default:
			groups = and(groups, [][]any{fieldFilters(key, o[key])})
		}
	}

	return groups
}

func fieldFilters(path string, ops any) []any {
	var out []any
	m, _ := ops.(map[string]any)
	for _, op := range slices.Sorted(maps.Keys(m)) {
		if _, known := filterOperators[op]; known {
			out = append(out, map[string]any{"fieldName": path, "operator": op, "value": m[op]})
		} else {
			out = append(out, fieldFilters(path+"."+op, m[op])...)
		}
	}

	return out
}
