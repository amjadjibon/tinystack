package appsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
)

// httpTimeout bounds an HTTP data source's request, as AppSync's 30-second
// limit does.
const httpTimeout = 30 * time.Second

// sourceError is a data source's failure, which code sees as ctx.error.
type sourceError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func (e *sourceError) Error() string { return e.Message }

// invokeSource sends the request code built to a data source, returning the
// result its response handler sees as ctx.result, or the failure it sees as
// ctx.error. Other errors are TinyStack's own.
func (r *session) invokeSource(ctx context.Context, d *api.DataSource, req any) (any, error) {
	m, ok := req.(map[string]any)
	if !ok {
		return nil, &sourceError{Type: errorCode, Message: "The request handler must return an object."}
	}

	switch common.Value(d.Type) {
	case sourceNone:
		return m["payload"], nil
	case sourceLambda:
		return r.invokeLambda(ctx, d, m)
	case sourceDynamoDB:
		return r.invokeDynamoDB(ctx, d, m)
	case sourceHTTP:
		return r.invokeHTTP(ctx, d, m)
	case sourceEventBridge:
		return r.invokeEventBridge(ctx, d, m)
	}

	return nil, &sourceError{Type: errorUnsupported, Message: "TinyStack cannot run data sources of type " + *d.Type}
}

func (r *session) invokeLambda(ctx context.Context, d *api.DataSource, req map[string]any) (any, error) {
	op, _ := req["operation"].(string)
	if op != "Invoke" && op != opBatchInvoke {
		return nil, &sourceError{
			Type:    errorUnsupported,
			Message: "Lambda data sources run the Invoke and BatchInvoke operations, not " + op + ".",
		}
	}

	value, err := r.invoke(ctx, *d.LambdaConfig.LambdaFunctionArn, req["payload"], op == opBatchInvoke)
	if fe, ok := errors.AsType[*fieldError](err); ok {
		return nil, &sourceError{Type: fe.Type, Message: fe.Message}
	}

	return value, err
}

// call runs an operation of another service in the API's account and region.
func (r *session) call(ctx context.Context, region, service, operation string, params map[string]any) (
	map[string]any,
	error,
) {
	if r.service.caller == nil {
		return nil, fmt.Errorf("appsync: no caller to reach %s", service)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	scope.Region = region
	out, err := r.service.caller.Call(request.WithMetadata(ctx, request.Metadata{Scope: scope}), service, operation,
		params)
	if ae, ok := errors.AsType[*awserror.Error](err); ok {
		prefix := map[string]string{"dynamodb": "DynamoDB:", "eventbridge": "EventBridge:"}[service]
		return nil, &sourceError{Type: prefix + ae.Code, Message: ae.Message}
	}

	return out, err
}

// expressionParams adds an expression object ({expression, expressionNames,
// expressionValues}) to DynamoDB parameters under field, merging its names
// and values with those already there.
func expressionParams(params map[string]any, field string, e any) {
	m, _ := e.(map[string]any)
	if s, _ := m["expression"].(string); s != "" {
		params[field] = s
	}

	for from, to := range map[string]string{
		"expressionNames": "ExpressionAttributeNames", "expressionValues": "ExpressionAttributeValues",
	} {
		values, _ := m[from].(map[string]any)
		if len(values) == 0 {
			continue
		}

		merged, _ := params[to].(map[string]any)
		if merged == nil {
			merged = map[string]any{}
		}

		maps.Copy(merged, values)
		params[to] = merged
	}
}

func (r *session) invokeDynamoDB(ctx context.Context, d *api.DataSource, req map[string]any) (any, error) {
	op, _ := req["operation"].(string)
	if result, handled, err := r.invokeDynamoDBMany(ctx, d, op, req); handled {
		return result, err
	}

	params := map[string]any{attrTableName: *d.DynamodbConfig.TableName}
	expressionParams(params, "ConditionExpression", req["condition"])
	for from, to := range map[string]string{
		"key": attrKey, "index": "IndexName", "limit": "Limit", "consistentRead": "ConsistentRead",
		"scanIndexForward": "ScanIndexForward", "select": "Select",
	} {
		if v, ok := req[from]; ok && v != nil {
			params[to] = v
		}
	}

	if token, _ := req["nextToken"].(string); token != "" {
		start, err := decodeToken(token)
		if err != nil {
			return nil, &sourceError{Type: "DynamoDB:ValidationException", Message: "Invalid nextToken."}
		}

		params["ExclusiveStartKey"] = start
	}

	switch op {
	case "GetItem":
	case opPutItem:
		item, _ := req["attributeValues"].(map[string]any)
		key, _ := req["key"].(map[string]any)
		merged := maps.Clone(item)
		if merged == nil {
			merged = map[string]any{}
		}

		maps.Copy(merged, key)
		delete(params, attrKey)
		params["Item"] = merged
	case opUpdateItem:
		expressionParams(params, "UpdateExpression", req["update"])
		params["ReturnValues"] = "ALL_NEW"
	case opDeleteItem:
		params["ReturnValues"] = "ALL_OLD"
	case "Query":
		expressionParams(params, "KeyConditionExpression", req["query"])
		expressionParams(params, "FilterExpression", req["filter"])
	case "Scan":
		expressionParams(params, "FilterExpression", req["filter"])
	default:
		return nil, &sourceError{Type: errorUnsupported, Message: fmt.Sprintf(
			"TinyStack does not run the %s operation of DynamoDB data sources.", op)}
	}

	out, err := r.call(ctx, *d.DynamodbConfig.AwsRegion, "dynamodb", op, params)
	if err != nil {
		return nil, err
	}

	switch op {
	case "GetItem":
		return plainItem(out["Item"]), nil
	case opPutItem:
		return plainItem(params["Item"]), nil
	case opUpdateItem, opDeleteItem:
		return plainItem(out["Attributes"]), nil
	}

	items, _ := out["Items"].([]any)
	list := make([]any, 0, len(items))
	for _, item := range items {
		list = append(list, plainItem(item))
	}

	result := map[string]any{"items": list, "scannedCount": out["ScannedCount"], "nextToken": nil}
	if last, ok := out["LastEvaluatedKey"].(map[string]any); ok && len(last) > 0 {
		b, _ := json.Marshal(last)
		result["nextToken"] = base64.RawURLEncoding.EncodeToString(b)
	}

	return result, nil
}

func decodeToken(token string) (map[string]any, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}

	var key map[string]any
	err = json.Unmarshal(b, &key)
	return key, err
}

// plainItem converts a DynamoDB item into plain JSON, as AppSync hands
// results to code.
func plainItem(item any) any {
	m, ok := item.(map[string]any)
	if !ok {
		return nil
	}

	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = plain(v)
	}

	return out
}

// plain converts a DynamoDB attribute value into plain JSON.
func plain(v any) any {
	av, _ := v.(map[string]any)
	for kind, value := range av {
		switch kind {
		case "S", "B", attrBOOL, "SS", "BS":
			return value
		case "N":
			return number(value)
		case attrNULL:
			return nil
		case "NS":
			list, _ := value.([]any)
			out := make([]any, 0, len(list))
			for _, n := range list {
				out = append(out, number(n))
			}

			return out
		case "L":
			list, _ := value.([]any)
			out := make([]any, 0, len(list))
			for _, e := range list {
				out = append(out, plain(e))
			}

			return out
		case "M":
			return plainItem(value)
		}
	}

	return nil
}

func number(v any) any {
	s, _ := v.(string)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}

	return s
}

func (r *session) invokeHTTP(ctx context.Context, d *api.DataSource, req map[string]any) (any, error) {
	method, _ := req["method"].(string)
	path, _ := req["resourcePath"].(string)
	params, _ := req["params"].(map[string]any)
	target := strings.TrimSuffix(*d.HttpConfig.Endpoint, "/") + path
	if q, _ := params["query"].(map[string]any); len(q) > 0 {
		values := url.Values{}
		for k, v := range q {
			values.Set(k, fmt.Sprint(v))
		}

		target += "?" + values.Encode()
	}

	header := http.Header{}
	if h, _ := params["headers"].(map[string]any); h != nil {
		for k, v := range h {
			header.Set(k, fmt.Sprint(v))
		}
	}

	var body []byte
	switch b := params["body"].(type) {
	case nil:
	case string:
		body = []byte(b)
	default:
		body, _ = json.Marshal(b)
	}

	if method == "" {
		method = http.MethodGet
	}

	resp := proxy.Forward(ctx, strings.ToUpper(method), target, header, body, httpTimeout)
	headers := map[string]any{}
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ",")
	}

	return map[string]any{"statusCode": resp.Status, "headers": headers, "body": string(resp.Body)}, nil
}

func (r *session) invokeEventBridge(ctx context.Context, d *api.DataSource, req map[string]any) (any, error) {
	if op, _ := req["operation"].(string); op != "PutEvents" {
		return nil, &sourceError{
			Type:    "Unsupported",
			Message: "TinyStack runs only the PutEvents operation of EventBridge data sources.",
		}
	}

	bus := *d.EventBridgeConfig.EventBusArn
	region := ""
	if parts := strings.Split(bus, ":"); len(parts) > 3 {
		region = parts[3]
	}

	events, _ := req["events"].([]any)
	entries := make([]any, 0, len(events))
	for _, e := range events {
		event, _ := e.(map[string]any)
		entry := map[string]any{"EventBusName": bus, "Source": event["source"], "DetailType": event["detailType"]}
		if detail, ok := event["detail"]; ok {
			b, _ := json.Marshal(detail)
			entry["Detail"] = string(b)
		}

		if resources, ok := event["resources"]; ok {
			entry["Resources"] = resources
		}

		entries = append(entries, entry)
	}

	return r.call(ctx, region, "eventbridge", "PutEvents", map[string]any{"Entries": entries})
}
