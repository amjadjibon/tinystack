package dynamodb

import (
	"context"
	"hash/fnv"
	"slices"
	"sort"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	selectAllAttributes = "ALL_ATTRIBUTES"
	selectAllProjected  = "ALL_PROJECTED_ATTRIBUTES"
)

// readRequest holds what Scan and Query share: the filter, projection, and
// selection, and the index the read uses.
type readRequest struct {
	ex        *expressions
	filter    *condition
	ix        *index
	key       string
	selection string
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

	req := &readRequest{key: key, selection: common.Value(selection)}
	if req.ex, err = newExpressions(names, values); err != nil {
		return nil, err
	}

	if req.filter, err = req.ex.condition("FilterExpression", filter); err != nil {
		return nil, err
	}

	if req.pr, err = req.ex.projection(projection); err != nil {
		return nil, err
	}

	switch req.selection {
	case "", selectAllProjected:
	case selectAllAttributes, selectCount:
		if len(req.pr) > 0 {
			return nil, awserror.Validation(
				"Cannot specify the ProjectionExpression when choosing to get %s", req.selection)
		}

		req.countOnly = req.selection == selectCount
	case "SPECIFIC_ATTRIBUTES":
		if len(req.pr) == 0 {
			return nil, awserror.Validation("SPECIFIC_ATTRIBUTES requires a ProjectionExpression")
		}
	default:
		return nil, awserror.Validation(
			"Select must be ALL_ATTRIBUTES, ALL_PROJECTED_ATTRIBUTES, SPECIFIC_ATTRIBUTES, or COUNT")
	}

	return req, nil
}

// use selects the index named name (the table itself when name is nil) and
// checks that the selection is one it can serve.
func (r *readRequest) use(t *table, name *string) error {
	ix, err := findIndex(t, name)
	if err != nil {
		return err
	}

	switch {
	case r.selection == selectAllProjected && ix.name == "":
		return awserror.Validation("ALL_PROJECTED_ATTRIBUTES can be used only when Querying using an IndexName")
	case r.selection == selectAllAttributes && ix.global && !ix.projectsAll():
		return awserror.Validation("One or more parameter values were invalid: Select type ALL_ATTRIBUTES "+
			"is not supported for global secondary index %s because its projection type is not ALL", ix.name)
	}

	r.ix = ix
	return nil
}

// visible returns the attributes of it a read can see: a global index holds
// only its projection, while a local index reads the rest from the table.
func (r *readRequest) visible(t *table, it item) item {
	if r.ix.global {
		return r.ix.project(t, it)
	}

	return it
}

// output returns what the read returns for it.
func (r *readRequest) output(t *table, it item) item {
	switch {
	case len(r.pr) > 0:
		return r.pr.apply(it)
	case r.selection == selectAllAttributes:
		return it
	default:
		return r.ix.project(t, it)
	}
}

type page struct {
	last    item
	items   []item
	count   int64
	scanned int64
}

// read evaluates n items in order, as Scan and Query do: Limit and the 1 MB
// bound count items read, before the filter drops any. at returns a nil item
// for one the index does not hold.
func (r *readRequest) read(t *table, n int, at func(int) (item, error), limit *int64) (page, error) {
	var out page
	var size int64
	var prev item
	for i := range n {
		it, err := at(i)
		if err != nil {
			return page{}, err
		}

		if it == nil {
			continue
		}

		it = r.visible(t, it)
		bytes, err := itemSize(it)
		if err != nil {
			return page{}, err
		}

		if prev != nil && size+bytes > maxPageSize {
			out.last = r.ix.lastKey(t, prev)
			break
		}

		size += bytes
		out.scanned++
		if r.filter.match(it) {
			out.count++
			if !r.countOnly {
				out.items = append(out.items, r.output(t, it))
			}
		}

		if i < n-1 && ((limit != nil && out.scanned >= *limit) || size >= maxPageSize) {
			out.last = r.ix.lastKey(t, it)
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

// Scan returns a deterministic page in table key order; ordering is a local
// implementation detail.
func (s *Service) Scan(ctx context.Context, in *api.ScanInput) (*api.ScanOutput, error) {
	req, err := newReadRequest(ctx, in.TableName, in.Limit, in.Select, in.FilterExpression,
		in.ProjectionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	if finishErr := req.ex.finish(); finishErr != nil {
		return nil, finishErr
	}

	inSegment, err := segment(in.Segment, in.TotalSegments)
	if err != nil {
		return nil, err
	}

	var p page
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, e := load(tx, req.key)
		if e != nil {
			return e
		}

		if guardErr := req.use(t, in.IndexName); guardErr != nil {
			return guardErr
		}

		records := slices.DeleteFunc(itemRecords(tx, req.key, ""), func(r string) bool { return !inSegment(r) })
		if len(in.ExclusiveStartKey) > 0 {
			id, keyErr := req.ix.startID(t, in.ExclusiveStartKey)
			if keyErr != nil {
				return keyErr
			}

			start := itemRecord(req.key, id)
			records = records[sort.Search(len(records), func(i int) bool { return records[i] > start }):]
		}

		p, e = req.read(t, len(records), func(i int) (item, error) {
			it, _, loadErr := loadItem(tx, records[i])
			if loadErr != nil || !req.ix.holds(it) {
				return nil, loadErr
			}

			return it, nil
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

// segment returns whether an item record belongs to a parallel scan's
// segment: each item is in exactly one of TotalSegments segments, chosen by
// a hash of its key. Without a segment, every item does.
func segment(seg, total *int64) (func(record string) bool, error) {
	switch {
	case seg == nil && total == nil:
		return func(string) bool { return true }, nil
	case seg == nil:
		return nil, awserror.Validation("The Segment parameter is required but was not present in the request " +
			"when parameter TotalSegments is present")
	case total == nil:
		return nil, awserror.Validation("The TotalSegments parameter is required but was not present in the " +
			"request when Segment parameter is present")
	case *total < 1 || *total > 1000000:
		return nil, awserror.Validation("1 validation error detected: Value '%d' at 'totalSegments' failed to "+
			"satisfy constraint: Member must have value between 1 and 1000000", *total)
	case *seg < 0 || *seg >= *total:
		return nil, awserror.Validation("The Segment parameter is zero-based and must be less than parameter "+
			"TotalSegments: Segment: %d is not less than TotalSegments: %d", *seg, *total)
	}

	return func(record string) bool {
		h := fnv.New64a()
		_, _ = h.Write([]byte(record))
		return h.Sum64()%uint64(*total) == uint64(*seg) // #nosec G115 -- both are checked to be positive.
	}, nil
}

// Query reads one partition of the table or an index in sort key order.
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

		if guardErr := req.use(t, in.IndexName); guardErr != nil {
			return guardErr
		}

		items, e := partition(
			tx,
			t,
			req.key,
			req.ix,
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

// entry is an item and its storage record, whose order breaks ties between
// items with equal index keys.
type entry struct {
	it     item
	record string
}

// partition returns the items of ix that match a key condition, in sort key
// order (reversed unless forward), starting after start when it is set. A
// table query reads only its partition's records; an index query reads the
// whole table, which keeps every index consistent with its items.
func partition(
	tx storage.ReadTx,
	t *table,
	key string,
	ix *index,
	keyCond *condition,
	start item,
	forward bool,
) ([]item, error) {
	hash, err := keyCondition(t, ix.schema, keyCond)
	if err != nil {
		return nil, err
	}

	var prefix string
	if ix.name == "" {
		prefix = hash
		if len(ix.schema) == 2 {
			prefix += ":"
		}
	}

	var entries []entry
	for _, record := range itemRecords(tx, key, prefix) {
		if ix.name == "" && len(ix.schema) == 1 && record != itemRecord(key, hash) {
			continue
		}

		it, _, loadErr := loadItem(tx, record)
		if loadErr != nil {
			return nil, loadErr
		}

		if ix.holds(it) && keyCond.match(it) {
			entries = append(entries, entry{it: it, record: record})
		}
	}

	direction := 1
	if !forward {
		direction = -1
	}

	var sortKey path
	if len(ix.schema) == 2 {
		sortKey = path{{name: common.Value(ix.schema[1].AttributeName)}}
	}

	compare := func(a, b entry) int {
		var n int
		if sortKey != nil {
			n, _ = order(get(a.it, sortKey), get(b.it, sortKey))
		}

		if n == 0 {
			n = strings.Compare(a.record, b.record)
		}

		return n * direction
	}

	slices.SortFunc(entries, compare)
	if len(start) > 0 {
		id, startErr := ix.startID(t, start)
		if startErr != nil {
			return nil, startErr
		}

		startHash, startErr := keyPart(t, &ix.schema[0], new(start[common.Value(ix.schema[0].AttributeName)]))
		if startErr != nil {
			return nil, startErr
		}

		if startHash != hash {
			return nil, awserror.Validation("The provided starting key is outside the queried partition")
		}

		from := entry{it: start, record: itemRecord(key, id)}
		entries = slices.DeleteFunc(entries, func(e entry) bool { return compare(e, from) <= 0 })
	}

	items := make([]item, len(entries))
	for i, e := range entries {
		items[i] = e.it
	}

	return items, nil
}

// keyCondition checks that c is a key condition for schema, an equality on the
// partition key and at most one sort key comparison, and returns the encoded
// partition key.
func keyCondition(t *table, schema []api.KeySchemaElement, c *condition) (string, error) {
	parts := []*condition{c}
	if c.op == condAnd {
		parts = c.sub
	}

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
		case attr == common.Value(schema[0].AttributeName):
			if part.op != condCompare || part.cmp != "=" {
				return "", awserror.Validation("Query key condition not supported")
			}

			hash = part.args[1].value
		case len(schema) == 2 && attr == common.Value(schema[1].AttributeName):
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
			common.Value(schema[0].AttributeName))
	}

	return keyPart(t, &schema[0], hash)
}
