package dynamodb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
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

	if err = json.Unmarshal(data, &it); err != nil {
		return nil, false, fmt.Errorf("decode item: %w", err)
	}

	return it, true, nil
}

// writeItem stores it as record in place of old (either may be nil) and keeps
// the table's item count and size current.
func writeItem(tx storage.WriteTx, key string, t *table, record string, old, it item) error {
	oldSize, err := itemSize(old)
	if err != nil {
		return err
	}

	newSize, err := itemSize(it)
	if err != nil {
		return err
	}

	count := value(t.Description.ItemCount)
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
	t.Description.TableSizeBytes = new(value(t.Description.TableSizeBytes) - oldSize + newSize)
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
		name := value(k.AttributeName)
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
		if value(d.AttributeName) == name {
			return value(d.AttributeType)
		}
	}

	return ""
}

// keyPart encodes one key attribute in URL-safe base64, which never contains
// the ':' between parts or the '/' of storage keys.
func keyPart(t *table, k *api.KeySchemaElement, a *api.AttributeValue) (string, error) {
	name := value(k.AttributeName)
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
	if value(k.KeyType) == "RANGE" {
		maxLength = 1024
	}

	if v == "" || len(v) > maxLength {
		return "", awserror.Validation("Invalid key length for %s", name)
	}

	return base64.RawURLEncoding.EncodeToString([]byte(v)), nil
}

func primaryKey(t *table, it item) item {
	key := item{}
	for _, field := range t.Description.KeySchema {
		name := value(field.AttributeName)
		key[name] = it[name]
	}

	return key
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

	size, err := itemSize(in.Item)
	if err != nil {
		return nil, err
	}

	if size > maxItemSize {
		return nil, awserror.Validation("Item size has exceeded the maximum allowed size")
	}

	out := &api.PutItemOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		id, e := itemKey(t, in.Item, false)
		if e != nil {
			return e
		}

		record := itemRecord(req.key, id)
		old, _, e := loadItem(tx, record)
		if e != nil {
			return e
		}

		if !req.cond.match(old) {
			return conditionFailed()
		}

		if value(in.ReturnValues) == returnAllOld {
			out.Attributes = old
		}

		return writeItem(tx, req.key, t, record, old, in.Item)
	})
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
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		id, e := itemKey(t, in.Key, true)
		if e != nil {
			return e
		}

		record := itemRecord(req.key, id)
		old, found, e := loadItem(tx, record)
		if e != nil {
			return e
		}

		if !req.cond.match(old) {
			return conditionFailed()
		}

		if !found {
			return nil
		}

		if value(in.ReturnValues) == returnAllOld {
			out.Attributes = old
		}

		return writeItem(tx, req.key, t, record, old, nil)
	})
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
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		id, e := itemKey(t, in.Key, true)
		if e != nil {
			return e
		}

		for _, p := range upd.paths() {
			if isKey(t, p[0].name) {
				return awserror.Validation("Cannot update attribute %s. This attribute is part of the key", p[0].name)
			}
		}

		record := itemRecord(req.key, id)
		old, found, e := loadItem(tx, record)
		if e != nil {
			return e
		}

		if !req.cond.match(old) {
			return conditionFailed()
		}

		base := old
		if !found {
			base = cloneItem(in.Key)
		}

		next, e := upd.apply(base)
		if e != nil {
			return e
		}

		size, e := itemSize(next)
		if e != nil {
			return e
		}

		if size > maxItemSize {
			return awserror.Validation("Item size to update has exceeded the maximum allowed size")
		}

		switch value(in.ReturnValues) {
		case returnAllOld:
			out.Attributes = old
		case returnAllNew:
			out.Attributes = next
		case returnUpdatedOld:
			out.Attributes = projection(upd.paths()).apply(old)
		case returnUpdatedNew:
			out.Attributes = projection(upd.paths()).apply(next)
		}

		return writeItem(tx, req.key, t, record, old, next)
	})
	return out, err
}

func isKey(t *table, name string) bool {
	return slices.ContainsFunc(t.Description.KeySchema, func(k api.KeySchemaElement) bool {
		return value(k.AttributeName) == name
	})
}

// readRequest holds what Scan and Query share: the filter, projection, and
// selection.
type readRequest struct {
	ex        *expressions
	filter    *condition
	key       string
	pr        projection
	countOnly bool
}

func newReadRequest(
	ctx context.Context,
	name *string,
	limit *int64,
	selection, filter, projection *string,
	names map[string]string,
	values map[string]api.AttributeValue,
) (*readRequest, error) {
	key, err := tableKey(ctx, name)
	if err != nil {
		return nil, err
	}

	if limit != nil && *limit < 1 {
		return nil, awserror.Validation("Limit must be positive")
	}

	req := &readRequest{key: key}
	if req.ex, err = newExpressions(names, values); err != nil {
		return nil, err
	}

	if req.filter, err = req.ex.condition("FilterExpression", filter); err != nil {
		return nil, err
	}

	if req.pr, err = req.ex.projection(projection); err != nil {
		return nil, err
	}

	switch value(selection) {
	case "":
	case "ALL_ATTRIBUTES", selectCount:
		if len(req.pr) > 0 {
			return nil, awserror.Validation(
				"Cannot specify the ProjectionExpression when choosing to get %s", value(selection))
		}

		req.countOnly = value(selection) == selectCount
	case "SPECIFIC_ATTRIBUTES":
		if len(req.pr) == 0 {
			return nil, awserror.Validation("SPECIFIC_ATTRIBUTES requires a ProjectionExpression")
		}
	default:
		return nil, awserror.Validation("Select must be ALL_ATTRIBUTES, SPECIFIC_ATTRIBUTES, or COUNT")
	}

	return req, nil
}

type page struct {
	last    item
	items   []item
	count   int64
	scanned int64
}

// read evaluates n items in order, as Scan and Query do: Limit and the 1 MB
// bound count items read, before the filter drops any.
func (r *readRequest) read(t *table, n int, at func(int) (item, error), limit *int64) (page, error) {
	var out page
	var size int64
	var prev item
	for i := range n {
		it, err := at(i)
		if err != nil {
			return page{}, err
		}

		bytes, err := itemSize(it)
		if err != nil {
			return page{}, err
		}

		if i > 0 && size+bytes > maxPageSize {
			out.last = primaryKey(t, prev)
			break
		}

		size += bytes
		out.scanned++
		if r.filter.match(it) {
			out.count++
			if !r.countOnly {
				out.items = append(out.items, r.pr.apply(it))
			}
		}

		if i < n-1 && ((limit != nil && out.scanned >= *limit) || size >= maxPageSize) {
			out.last = primaryKey(t, it)
			break
		}

		prev = it
	}

	return out, nil
}

// list returns the page's items, empty rather than absent.
func (p *page) list() *[]item {
	if p.items == nil {
		return &[]item{}
	}

	return &p.items
}

// Scan returns a deterministic page; ordering is a local implementation detail.
func (s *Service) Scan(ctx context.Context, in *api.ScanInput) (*api.ScanOutput, error) {
	req, err := newReadRequest(ctx, in.TableName, in.Limit, in.Select, in.FilterExpression,
		in.ProjectionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	if finishErr := req.ex.finish(); finishErr != nil {
		return nil, finishErr
	}

	var p page
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		records := itemRecords(tx, req.key, "")
		if len(in.ExclusiveStartKey) > 0 {
			id, keyErr := itemKey(t, in.ExclusiveStartKey, true)
			if keyErr != nil {
				return keyErr
			}

			start := itemRecord(req.key, id)
			records = records[sort.Search(len(records), func(i int) bool { return records[i] > start }):]
		}

		p, e = req.read(t, len(records), func(i int) (item, error) {
			it, _, loadErr := loadItem(tx, records[i])
			return it, loadErr
		}, in.Limit)
		return e
	})
	if err != nil {
		return nil, err
	}

	out := &api.ScanOutput{
		Items: p.list(), Count: &p.count, ScannedCount: &p.scanned, LastEvaluatedKey: p.last,
	}

	if req.countOnly {
		out.Items = nil
	}

	return out, nil
}

// Query reads one partition in sort key order.
func (s *Service) Query(ctx context.Context, in *api.QueryInput) (*api.QueryOutput, error) {
	req, err := newReadRequest(ctx, in.TableName, in.Limit, in.Select, in.FilterExpression,
		in.ProjectionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	if in.KeyConditionExpression == nil {
		return nil, awserror.Validation("KeyConditionExpression must be specified in the request")
	}

	keyCond, err := req.ex.condition("KeyConditionExpression", in.KeyConditionExpression)
	if err != nil {
		return nil, err
	}

	if finishErr := req.ex.finish(); finishErr != nil {
		return nil, finishErr
	}

	var p page
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		items, e := partition(
			tx,
			t,
			req.key,
			keyCond,
			in.ExclusiveStartKey,
			in.ScanIndexForward == nil || *in.ScanIndexForward,
		)
		if e != nil {
			return e
		}

		p, e = req.read(t, len(items), func(i int) (item, error) { return items[i], nil }, in.Limit)
		return e
	})
	if err != nil {
		return nil, err
	}

	out := &api.QueryOutput{
		Items: p.list(), Count: &p.count, ScannedCount: &p.scanned, LastEvaluatedKey: p.last,
	}

	if req.countOnly {
		out.Items = nil
	}

	return out, nil
}

// partition returns the items that match a key condition, in sort key order
// (reversed unless forward), starting after start when it is set.
func partition(
	tx storage.ReadTx,
	t *table,
	key string,
	keyCond *condition,
	start item,
	forward bool,
) ([]item, error) {
	hash, err := keyCondition(t, keyCond)
	if err != nil {
		return nil, err
	}

	schema := t.Description.KeySchema
	prefix := hash
	if len(schema) == 2 {
		prefix += ":"
	}

	var items []item
	for _, record := range itemRecords(tx, key, prefix) {
		if len(schema) == 1 && record != itemRecord(key, hash) {
			continue
		}

		it, _, loadErr := loadItem(tx, record)
		if loadErr != nil {
			return nil, loadErr
		}

		if keyCond.match(it) {
			items = append(items, it)
		}
	}

	direction := 1
	if !forward {
		direction = -1
	}

	var sortKey string
	if len(schema) == 2 {
		sortKey = value(schema[1].AttributeName)
		slices.SortStableFunc(items, func(a, b item) int {
			n, _ := order(get(a, path{{name: sortKey}}), get(b, path{{name: sortKey}}))
			return n * direction
		})
	}

	if len(start) == 0 {
		return items, nil
	}

	id, err := itemKey(t, start, true)
	if err != nil {
		return nil, err
	}

	if id != hash && !strings.HasPrefix(id, prefix) {
		return nil, awserror.Validation("The provided starting key is outside the queried partition")
	}

	if sortKey == "" {
		return nil, nil
	}

	after := get(start, path{{name: sortKey}})
	return slices.DeleteFunc(items, func(it item) bool {
		n, _ := order(get(it, path{{name: sortKey}}), after)
		return n*direction <= 0
	}), nil
}

// keyCondition checks that c is a key condition for t, an equality on the
// partition key and at most one sort key comparison, and returns the encoded
// partition key.
func keyCondition(t *table, c *condition) (string, error) {
	parts := []*condition{c}
	if c.op == condAnd {
		parts = c.sub
	}

	schema := t.Description.KeySchema
	var hash *api.AttributeValue
	seen := map[string]bool{}
	for _, part := range parts {
		switch {
		case part.op == condCompare && part.cmp != "<>", part.op == condBetween,
			part.op == condFunc && part.fn == fnBeginsWith:
		default:
			return "", awserror.Validation("Invalid operator used in KeyConditionExpression")
		}

		name := part.args[0].path
		for _, o := range part.args[1:] {
			if o.value == nil {
				return "", awserror.Validation("Invalid KeyConditionExpression: key conditions compare a key " +
					"attribute with values")
			}
		}

		if len(name) != 1 || seen[name[0].name] {
			return "", awserror.Validation("Invalid KeyConditionExpression: each key attribute may be " +
				"compared once, by name")
		}

		attr := name[0].name
		seen[attr] = true
		kind := attributeKind(t, attr)
		switch {
		case attr == value(schema[0].AttributeName):
			if part.op != condCompare || part.cmp != "=" {
				return "", awserror.Validation("Query key condition not supported")
			}

			hash = part.args[1].value
		case len(schema) == 2 && attr == value(schema[1].AttributeName):
			if part.op == condFunc && kind == "N" {
				return "", awserror.Validation("Invalid KeyConditionExpression: begins_with does not support N")
			}
		default:
			return "", awserror.Validation("Query key condition not supported: %s is not a key attribute", attr)
		}

		for _, o := range part.args[1:] {
			if attributeType(o.value) != kind {
				return "", awserror.Validation(
					"One or more parameter values were invalid: Condition parameter type does not match schema type")
			}
		}
	}

	if hash == nil {
		return "", awserror.Validation("Query condition missed key schema element: %s",
			value(schema[0].AttributeName))
	}

	return keyPart(t, &schema[0], hash)
}
