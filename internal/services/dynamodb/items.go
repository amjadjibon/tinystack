package dynamodb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	streamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	returnAllNew     = "ALL_NEW"
	returnUpdatedOld = "UPDATED_OLD"
	returnUpdatedNew = "UPDATED_NEW"
	maxItemSize      = 400 * 1024
	maxPageSize      = 1 << 20
)

// itemRecord is the storage key of item id in the table stored at key.
func itemRecord(key, id string) string { return key + "/item/" + id }

// itemRecords returns, in order, the storage keys of the table's items whose
// ids start with prefix.
func itemRecords(tx storage.ReadTx, key, prefix string) []string {
	return tx.Keys(itemRecord(key, prefix))
}

func loadItem(tx storage.ReadTx, record string) (it item, found bool, err error) {
	data, ok := tx.Get(record)
	if !ok {
		return nil, false, nil
	}

	if guardErr := json.Unmarshal(data, &it); guardErr != nil {
		return nil, false, fmt.Errorf("decode item: %w", guardErr)
	}

	return it, true, nil
}

// writeItem stores it as record in place of old (either may be nil), keeps
// the table's item count and size current, and writes the change to the
// table's stream. The change is also queued in out for the table's Kinesis
// destinations, which the caller delivers after the transaction commits.
func writeItem(tx storage.WriteTx, key string, t *table, record string, old, it item, out *outbox) error {
	return writeItemBy(tx, key, t, record, old, it, nil, out)
}

// writeItemBy is writeItem for a change a service made, such as a TTL
// deletion, which its stream record names as by.
func writeItemBy(
	tx storage.WriteTx,
	key string,
	t *table,
	record string,
	old, it item,
	by *streamsapi.Identity,
	out *outbox,
) error {
	oldSize, err := itemSize(old)
	if err != nil {
		return err
	}

	newSize, err := itemSize(it)
	if err != nil {
		return err
	}

	count := common.Value(t.Description.ItemCount)
	if old != nil {
		count--
	}

	if it == nil {
		tx.Delete(record)
	} else {
		data, encodeErr := json.Marshal(it)
		if encodeErr != nil {
			return fmt.Errorf("encode item: %w", encodeErr)
		}

		tx.Put(record, data)
		count++
	}

	t.Description.ItemCount = &count
	t.Description.TableSizeBytes = new(common.Value(t.Description.TableSizeBytes) - oldSize + newSize)
	if guardErr := appendRecord(tx, key, t, old, it, by); guardErr != nil {
		return guardErr
	}

	if guardErr := out.add(key, record, t, old, it, by); guardErr != nil {
		return guardErr
	}

	return save(tx, key, t)
}

// itemSize is DynamoDB's item size: every attribute name plus its value.
func itemSize(it item) (int64, error) {
	var size int64
	for name, a := range it {
		if name == "" {
			return 0, awserror.Validation("Attribute names must not be empty")
		}

		n, err := attributeSize(a, 0)
		if err != nil {
			return 0, err
		}

		size += int64(len(name) + n)
	}

	return size, nil
}

// itemKey encodes the key attributes of it as the item's id. With exact, it
// must hold nothing else, as a Key parameter must.
func itemKey(t *table, it item, exact bool) (string, error) {
	if exact && len(it) != len(t.Description.KeySchema) {
		return "", awserror.Validation("Key must contain exactly the table key attributes")
	}

	parts := make([]string, 0, len(t.Description.KeySchema))
	for _, k := range t.Description.KeySchema {
		name := common.Value(k.AttributeName)
		a, ok := it[name]
		if !ok {
			return "", awserror.Validation("Missing key attribute %s", name)
		}

		part, err := keyPart(t, &k, &a)
		if err != nil {
			return "", err
		}

		parts = append(parts, part)
	}

	return strings.Join(parts, ":"), nil
}

// attributeKind returns the type the table's attribute definitions give name.
func attributeKind(t *table, name string) string {
	for _, d := range t.Description.AttributeDefinitions {
		if common.Value(d.AttributeName) == name {
			return common.Value(d.AttributeType)
		}
	}

	return ""
}

// keyPart encodes one key attribute in URL-safe base64, which never contains
// the ':' between parts or the '/' of storage keys.
func keyPart(t *table, k *api.KeySchemaElement, a *api.AttributeValue) (string, error) {
	name := common.Value(k.AttributeName)
	if _, err := attributeSize(*a, 0); err != nil {
		return "", err
	}

	var v string
	switch kind := attributeKind(t, name); {
	case kind == "S" && a.S != nil:
		v = *a.S
	case kind == "N" && a.N != nil:
		v = rat(*a.N).RatString()
	case kind == "B" && a.B != nil:
		v = string(*a.B)
	default:
		return "", awserror.Validation("Key type mismatch for %s", name)
	}

	maxLength := 2048
	if common.Value(k.KeyType) == "RANGE" {
		maxLength = 1024
	}

	if v == "" || len(v) > maxLength {
		return "", awserror.Validation("Invalid key length for %s", name)
	}

	return base64.RawURLEncoding.EncodeToString([]byte(v)), nil
}

func returnValues(p *string, allowed ...string) error {
	if p != nil && *p != "NONE" && !slices.Contains(allowed, *p) {
		return awserror.Validation("ReturnValues must be NONE or %s", strings.Join(allowed, ", "))
	}

	return nil
}

func conditionFailed() error {
	return awserror.New("ConditionalCheckFailedException", "The conditional request failed")
}

// writeRequest holds a table key and the parsed condition every item write
// accepts.
type writeRequest struct {
	ex   *expressions
	cond *condition
	key  string
}

func newWriteRequest(
	ctx context.Context,
	name, condition *string,
	names map[string]string,
	values map[string]api.AttributeValue,
) (*writeRequest, error) {
	key, err := tableKey(ctx, name)
	if err != nil {
		return nil, err
	}

	ex, err := newExpressions(names, values)
	if err != nil {
		return nil, err
	}

	cond, err := ex.condition("ConditionExpression", condition)
	if err != nil {
		return nil, err
	}

	return &writeRequest{ex: ex, cond: cond, key: key}, nil
}

func (s *Service) PutItem(ctx context.Context, in *api.PutItemInput) (*api.PutItemOutput, error) {
	if err := returnValues(in.ReturnValues, returnAllOld); err != nil {
		return nil, err
	}

	req, err := newWriteRequest(ctx, in.TableName, in.ConditionExpression, in.ExpressionAttributeNames,
		in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	if finishErr := req.ex.finish(); finishErr != nil {
		return nil, finishErr
	}

	out := &api.PutItemOutput{}
	var puts outbox
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		if guardErr := checkItem(t, in.Item, "Item size has exceeded the maximum allowed size"); guardErr != nil {
			return guardErr
		}

		record, old, _, e := target(tx, req.key, t, in.Item, false)
		if e != nil {
			return e
		}

		if !req.cond.match(old) {
			return conditionFailed()
		}

		if common.Value(in.ReturnValues) == returnAllOld {
			out.Attributes = old
		}

		return writeItem(tx, req.key, t, record, old, in.Item, &puts)
	})
	if err == nil {
		s.deliver(ctx, &puts)
	}

	return out, err
}

// GetItem returns the item, or an empty output when the key is absent.
func (s *Service) GetItem(ctx context.Context, in *api.GetItemInput) (*api.GetItemOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	ex, err := newExpressions(in.ExpressionAttributeNames, nil)
	if err != nil {
		return nil, err
	}

	pr, err := ex.projection(in.ProjectionExpression)
	if err != nil {
		return nil, err
	}

	if finishErr := ex.finish(); finishErr != nil {
		return nil, finishErr
	}

	out := &api.GetItemOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, e := load(tx, key)
		if e != nil {
			return e
		}

		id, e := itemKey(t, in.Key, true)
		if e != nil {
			return e
		}

		it, _, e := loadItem(tx, itemRecord(key, id))
		out.Item = pr.apply(it)
		return e
	})
	return out, err
}

func (s *Service) DeleteItem(ctx context.Context, in *api.DeleteItemInput) (*api.DeleteItemOutput, error) {
	if err := returnValues(in.ReturnValues, returnAllOld); err != nil {
		return nil, err
	}

	req, err := newWriteRequest(ctx, in.TableName, in.ConditionExpression, in.ExpressionAttributeNames,
		in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	if finishErr := req.ex.finish(); finishErr != nil {
		return nil, finishErr
	}

	out := &api.DeleteItemOutput{}
	var puts outbox
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		record, old, found, e := target(tx, req.key, t, in.Key, true)
		if e != nil {
			return e
		}

		if !req.cond.match(old) {
			return conditionFailed()
		}

		if !found {
			return nil
		}

		if common.Value(in.ReturnValues) == returnAllOld {
			out.Attributes = old
		}

		return writeItem(tx, req.key, t, record, old, nil, &puts)
	})
	if err == nil {
		s.deliver(ctx, &puts)
	}

	return out, err
}

// UpdateItem applies an update expression, creating the item from its key
// when it does not exist.
func (s *Service) UpdateItem(ctx context.Context, in *api.UpdateItemInput) (*api.UpdateItemOutput, error) {
	err := returnValues(in.ReturnValues, returnAllOld, returnUpdatedOld, returnAllNew, returnUpdatedNew)
	if err != nil {
		return nil, err
	}

	req, err := newWriteRequest(ctx, in.TableName, in.ConditionExpression, in.ExpressionAttributeNames,
		in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	upd, err := req.ex.update(in.UpdateExpression)
	if err != nil {
		return nil, err
	}

	if finishErr := req.ex.finish(); finishErr != nil {
		return nil, finishErr
	}

	out := &api.UpdateItemOutput{}
	var puts outbox
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		record, old, found, e := target(tx, req.key, t, in.Key, true)
		if e != nil {
			return e
		}

		if guardErr := updatesKey(t, upd); guardErr != nil {
			return guardErr
		}

		if !req.cond.match(old) {
			return conditionFailed()
		}

		next, e := updated(t, upd, in.Key, old, found)
		if e != nil {
			return e
		}

		switch common.Value(in.ReturnValues) {
		case returnAllOld:
			out.Attributes = old
		case returnAllNew:
			out.Attributes = next
		case returnUpdatedOld:
			out.Attributes = projection(upd.paths()).apply(old)
		case returnUpdatedNew:
			out.Attributes = projection(upd.paths()).apply(next)
		}

		return writeItem(tx, req.key, t, record, old, next, &puts)
	})
	if err == nil {
		s.deliver(ctx, &puts)
	}

	return out, err
}

// target returns the record of the item keyed by k in the table stored at key,
// and the item stored there. With exact, k must hold only key attributes.
func target(
	tx storage.ReadTx,
	key string,
	t *table,
	k item,
	exact bool,
) (record string, old item, found bool, err error) {
	id, err := itemKey(t, k, exact)
	if err != nil {
		return "", nil, false, err
	}

	record = itemRecord(key, id)
	old, found, err = loadItem(tx, record)
	return record, old, found, err
}

// checkItem rejects an item over the size limit, reporting message, or with
// an index key attribute of the wrong type.
func checkItem(t *table, it item, message string) error {
	size, err := itemSize(it)
	if err != nil {
		return err
	}

	if size > maxItemSize {
		return awserror.Validation("%s", message)
	}

	for _, ix := range secondaryIndexes(t) {
		for _, k := range ix.schema {
			if a, ok := it[common.Value(k.AttributeName)]; ok {
				if _, keyErr := keyPart(t, &k, &a); keyErr != nil {
					return awserror.Validation(
						"One or more parameter values were invalid: Type mismatch for Index Key %s Index: %s",
						common.Value(k.AttributeName), ix.name)
				}
			}
		}
	}

	return nil
}

// updatesKey rejects an update expression that changes a key attribute.
func updatesKey(t *table, upd *update) error {
	for _, p := range upd.paths() {
		if isKey(t, p[0].name) {
			return awserror.Validation("Cannot update attribute %s. This attribute is part of the key", p[0].name)
		}
	}

	return nil
}

// updated returns the item upd makes from old, or from the key k when the
// item was not found.
func updated(t *table, upd *update, k, old item, found bool) (item, error) {
	base := old
	if !found {
		base = cloneItem(k)
	}

	next, err := upd.apply(base)
	if err != nil {
		return nil, err
	}

	if guardErr := checkItem(t, next, "Item size to update has exceeded the maximum allowed size"); guardErr != nil {
		return nil, guardErr
	}

	return next, nil
}

func isKey(t *table, name string) bool {
	return slices.ContainsFunc(t.Description.KeySchema, func(k api.KeySchemaElement) bool {
		return common.Value(k.AttributeName) == name
	})
}
