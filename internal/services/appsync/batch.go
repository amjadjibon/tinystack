package appsync

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
)

// The DynamoDB operations that span several items, and tables.
const (
	opBatchGet      = "BatchGetItem"
	opBatchPut      = "BatchPutItem"
	opBatchDelete   = "BatchDeleteItem"
	opTransactGet   = "TransactGetItems"
	opTransactWrite = "TransactWriteItems"
)

// invokeDynamoDBMany runs a batch or transaction request, reporting false
// for any other operation.
func (r *session) invokeDynamoDBMany(ctx context.Context, d *api.DataSource, op string, req map[string]any) (
	result any, handled bool, err error,
) {
	region := *d.DynamodbConfig.AwsRegion
	tables, _ := req["tables"].(map[string]any)
	switch op {
	case opBatchGet:
		result, err = r.batchGet(ctx, region, tables)
	case opBatchPut, opBatchDelete:
		result, err = r.batchWrite(ctx, region, op, tables)
	case opTransactGet:
		result, err = r.transactGet(ctx, region, req)
	case opTransactWrite:
		result, err = r.transactWrite(ctx, region, req)
	default:
		return nil, false, nil
	}

	return result, true, err
}

func (r *session) batchGet(ctx context.Context, region string, tables map[string]any) (any, error) {
	items := map[string]any{}
	keys := map[string][]any{}
	for table, t := range tables {
		spec, _ := t.(map[string]any)
		keys[table], _ = spec["keys"].([]any)
		request := map[string]any{"Keys": keys[table]}
		if consistent, ok := spec["consistentRead"].(bool); ok {
			request["ConsistentRead"] = consistent
		}

		items[table] = request
	}

	out, err := r.call(ctx, region, "dynamodb", opBatchGet, map[string]any{"RequestItems": items})
	if err != nil {
		return nil, err
	}

	responses, _ := out["Responses"].(map[string]any)
	data := map[string]any{}
	for table, wanted := range keys {
		found, _ := responses[table].([]any)
		list := make([]any, 0, len(wanted))
		for _, key := range wanted {
			i := slices.IndexFunc(found, func(item any) bool { return hasKey(item, key) })
			if i < 0 {
				list = append(list, nil)
				continue
			}

			list = append(list, plainItem(found[i]))
		}

		data[table] = list
	}

	return map[string]any{keyData: data, "unprocessedKeys": unprocessed(out["UnprocessedKeys"], tables)}, nil
}

// hasKey reports whether an item has the attributes of key.
func hasKey(item, key any) bool {
	m, _ := item.(map[string]any)
	k, _ := key.(map[string]any)
	for name, v := range k {
		if !reflect.DeepEqual(normalize(m[name]), normalize(v)) {
			return false
		}
	}

	return len(k) > 0
}

// unprocessed lists each table's unprocessed keys or items, empty for
// tables without any.
func unprocessed(v any, tables map[string]any) map[string]any {
	out := map[string]any{}
	left, _ := v.(map[string]any)
	for table := range tables {
		out[table] = []any{}
		switch t := left[table].(type) {
		case map[string]any:
			if keys, isList := t["Keys"].([]any); isList {
				out[table] = keys
			}
		case []any:
			out[table] = t
		}
	}

	return out
}

func (r *session) batchWrite(ctx context.Context, region, op string, tables map[string]any) (any, error) {
	requests := map[string]any{}
	data := map[string]any{}
	for table, t := range tables {
		entries, _ := t.([]any)
		writes := make([]any, 0, len(entries))
		plain := make([]any, 0, len(entries))
		for _, e := range entries {
			if op == opBatchPut {
				writes = append(writes, map[string]any{"PutRequest": map[string]any{"Item": e}})
			} else {
				writes = append(writes, map[string]any{"DeleteRequest": map[string]any{attrKey: e}})
			}

			plain = append(plain, plainItem(e))
		}

		requests[table], data[table] = writes, plain
	}

	out, err := r.call(ctx, region, "dynamodb", "BatchWriteItem", map[string]any{"RequestItems": requests})
	if err != nil {
		return nil, err
	}

	left := map[string]any{}
	for table, writes := range unprocessed(out["UnprocessedItems"], tables) {
		list := []any{}
		for _, w := range writes.([]any) {
			m, _ := w.(map[string]any)
			put, _ := m["PutRequest"].(map[string]any)
			del, _ := m["DeleteRequest"].(map[string]any)
			list = append(list, plainItem(map[bool]any{true: put["Item"], false: del[attrKey]}[put != nil]))
		}

		left[table] = list
	}

	unprocessedName := map[string]string{opBatchPut: "unprocessedItems", opBatchDelete: "unprocessedKeys"}[op]
	return map[string]any{keyData: data, unprocessedName: left}, nil
}

func (r *session) transactGet(ctx context.Context, region string, req map[string]any) (any, error) {
	entries, _ := req["transactItems"].([]any)
	items := make([]any, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]any)
		get := map[string]any{attrTableName: m["table"], attrKey: m["key"]}
		expressionParams(get, "ProjectionExpression", m["projection"])
		items = append(items, map[string]any{"Get": get})
	}

	out, err := r.call(ctx, region, "dynamodb", opTransactGet, map[string]any{"TransactItems": items})
	if err != nil {
		return nil, err
	}

	responses, _ := out["Responses"].([]any)
	result := make([]any, 0, len(responses))
	for _, resp := range responses {
		m, _ := resp.(map[string]any)
		result = append(result, plainItem(m["Item"]))
	}

	return map[string]any{"items": result, "cancellationReasons": nil}, nil
}

func (r *session) transactWrite(ctx context.Context, region string, req map[string]any) (any, error) {
	entries, _ := req["transactItems"].([]any)
	items := make([]any, 0, len(entries))
	keys := make([]any, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]any)
		key, _ := m["key"].(map[string]any)
		params := map[string]any{attrTableName: m["table"], attrKey: key}
		expressionParams(params, "ConditionExpression", m["condition"])
		var kind string
		switch op, _ := m["operation"].(string); op {
		case opPutItem:
			item := maps.Clone(key)
			attributes, _ := m["attributeValues"].(map[string]any)
			maps.Copy(item, attributes)
			delete(params, attrKey)
			params["Item"], kind = item, "Put"
		case opUpdateItem:
			expressionParams(params, "UpdateExpression", m["update"])
			kind = "Update"
		case opDeleteItem:
			kind = "Delete"
		case "ConditionCheck":
			kind = "ConditionCheck"
		default:
			return nil, &sourceError{Type: "DynamoDB:ValidationException", Message: fmt.Sprintf(
				"Unknown transactItems operation %q.", op)}
		}

		items = append(items, map[string]any{kind: params})
		keys = append(keys, plainItem(key))
	}

	if _, err := r.call(ctx, region, "dynamodb", opTransactWrite, map[string]any{"TransactItems": items}); err != nil {
		return nil, err
	}

	return map[string]any{"keys": keys, "cancellationReasons": nil}, nil
}
