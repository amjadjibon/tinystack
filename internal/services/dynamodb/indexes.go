package dynamodb

import (
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	projectAll      = "ALL"
	projectKeysOnly = "KEYS_ONLY"
	projectInclude  = "INCLUDE"
)

// index is the key schema and projection a read uses: a secondary index, or
// the table itself when name is empty. Indexes keep no records of their own;
// reads derive them from the table's items in the same transaction, so an
// index is always consistent with the table.
type index struct {
	projection *api.Projection
	name       string
	schema     []api.KeySchemaElement
	global     bool
}

// findIndex returns the index named name, or the table's own key when name is nil.
func findIndex(t *table, name *string) (*index, error) {
	if name == nil {
		return &index{schema: t.Description.KeySchema}, nil
	}

	for _, ix := range secondaryIndexes(t) {
		if ix.name == *name {
			return ix, nil
		}
	}

	return nil, awserror.Validation("The table does not have the specified index: %s", *name)
}

// secondaryIndexes returns the table's global and local secondary indexes.
func secondaryIndexes(t *table) []*index {
	var out []*index
	for _, g := range t.Description.GlobalSecondaryIndexes {
		out = append(
			out,
			&index{name: common.Value(g.IndexName), schema: g.KeySchema, projection: g.Projection, global: true},
		)
	}

	for _, l := range t.Description.LocalSecondaryIndexes {
		out = append(out, &index{name: common.Value(l.IndexName), schema: l.KeySchema, projection: l.Projection})
	}

	return out
}

func (ix *index) projectsAll() bool {
	return ix.projection == nil || common.Value(ix.projection.ProjectionType) == projectAll
}

// keyNames returns the table's key attributes, then the index's others.
func (ix *index) keyNames(t *table) []string {
	var names []string
	for _, k := range slices.Concat(t.Description.KeySchema, ix.schema) {
		if name := common.Value(k.AttributeName); !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	return names
}

// holds reports whether it is in the index: a sparse index holds only the
// items that have all of its key attributes.
func (ix *index) holds(it item) bool {
	for _, k := range ix.schema {
		if _, ok := it[common.Value(k.AttributeName)]; !ok {
			return false
		}
	}

	return true
}

// project returns the attributes of it that the index projects.
func (ix *index) project(t *table, it item) item {
	if ix.projectsAll() {
		return it
	}

	names := ix.keyNames(t)
	if common.Value(ix.projection.ProjectionType) == projectInclude {
		names = append(names, ix.projection.NonKeyAttributes...)
	}

	out := item{}
	for _, name := range names {
		if v, ok := it[name]; ok {
			out[name] = v
		}
	}

	return out
}

// lastKey is the LastEvaluatedKey after it: its table and index key attributes.
func (ix *index) lastKey(t *table, it item) item {
	out := item{}
	for _, name := range ix.keyNames(t) {
		out[name] = it[name]
	}

	return out
}

// startID returns the item id of an ExclusiveStartKey, which must hold exactly
// the table and index key attributes.
func (ix *index) startID(t *table, start item) (string, error) {
	if ix.name == "" {
		return itemKey(t, start, true)
	}

	names := ix.keyNames(t)
	invalid := len(start) != len(names)
	for _, name := range names {
		if _, ok := start[name]; !ok {
			invalid = true
		}
	}

	if invalid {
		return "", awserror.Validation("The provided starting key is invalid: " +
			"the key must hold exactly the table and index key attributes")
	}

	return itemKey(t, start, false)
}

// checkKeySchema checks a HASH key and an optional RANGE key on defined
// attributes and marks them used.
func checkKeySchema(schema []api.KeySchemaElement, types map[string]string, used map[string]bool) error {
	if len(schema) < 1 || len(schema) > 2 {
		return awserror.Validation("Invalid key schema: define a HASH key and an optional RANGE key")
	}

	for i, k := range schema {
		expected := "HASH"
		if i == 1 {
			expected = "RANGE"
		}

		name := common.Value(k.AttributeName)
		if types[name] == "" || common.Value(k.KeyType) != expected ||
			(i == 1 && name == common.Value(schema[0].AttributeName)) {
			return awserror.Validation("Invalid key schema: key attributes must be defined, distinct, " +
				"and ordered HASH then RANGE")
		}

		used[name] = true
	}

	return nil
}

func checkIndexName(name *string) error {
	if name == nil || !tableName.MatchString(*name) {
		return awserror.Validation("IndexName must be 3–255 characters using letters, digits, _, -, or .")
	}

	return nil
}

// describeGlobal validates a global secondary index, marks its key attributes
// used, and describes it. Index item counts and sizes stay 0: DynamoDB
// refreshes them only every six hours.
func describeGlobal(
	g *api.GlobalSecondaryIndex,
	types map[string]string,
	used map[string]bool,
	tableArn, mode string,
) (api.GlobalSecondaryIndexDescription, error) {
	if err := checkKeySchema(g.KeySchema, types, used); err != nil {
		return api.GlobalSecondaryIndexDescription{}, err
	}

	if err := checkProjection(g.Projection, *g.IndexName); err != nil {
		return api.GlobalSecondaryIndexDescription{}, err
	}

	pt, err := throughputFor(mode, g.ProvisionedThroughput)
	if err != nil {
		return api.GlobalSecondaryIndexDescription{}, err
	}

	warm := tableWarmThroughput(pt)
	return api.GlobalSecondaryIndexDescription{
		IndexName:             g.IndexName,
		KeySchema:             g.KeySchema,
		Projection:            g.Projection,
		IndexStatus:           new("ACTIVE"),
		ProvisionedThroughput: pt,
		WarmThroughput: &api.GlobalSecondaryIndexWarmThroughputDescription{
			ReadUnitsPerSecond:  warm.ReadUnitsPerSecond,
			WriteUnitsPerSecond: warm.WriteUnitsPerSecond,
			Status:              warm.Status,
		},
		IndexSizeBytes: new(int64(0)),
		ItemCount:      new(int64(0)),
		IndexArn:       new(tableArn + "/index/" + *g.IndexName),
	}, nil
}

// checkProjection checks an index projection, which must name its type.
func checkProjection(p *api.Projection, index string) error {
	if p == nil {
		return awserror.Validation("Projection must be specified for index %s", index)
	}

	switch common.Value(p.ProjectionType) {
	case projectAll, projectKeysOnly:
		if len(p.NonKeyAttributes) > 0 {
			return awserror.Validation("NonKeyAttributes can be specified only with the INCLUDE projection type")
		}
	case projectInclude:
		if len(p.NonKeyAttributes) == 0 || len(p.NonKeyAttributes) > 20 {
			return awserror.Validation("The INCLUDE projection of %s needs 1 to 20 NonKeyAttributes", index)
		}

		seen := map[string]bool{}
		for _, name := range p.NonKeyAttributes {
			if name == "" || seen[name] {
				return awserror.Validation("NonKeyAttributes must be distinct attribute names")
			}

			seen[name] = true
		}
	default:
		return awserror.Validation("ProjectionType must be ALL, KEYS_ONLY, or INCLUDE")
	}

	return nil
}

// secondaryIndexDescriptions validates CreateTable's secondary indexes, marks
// their key attributes used, and describes them.
func secondaryIndexDescriptions(
	in *api.CreateTableInput,
	types map[string]string,
	used map[string]bool,
	tableArn, mode string,
) ([]api.GlobalSecondaryIndexDescription, []api.LocalSecondaryIndexDescription, error) {
	if len(in.GlobalSecondaryIndexes) > maxGlobalIndexes || len(in.LocalSecondaryIndexes) > 5 {
		return nil, nil, awserror.Validation("A table can have at most 20 global and 5 local secondary indexes")
	}

	names := map[string]bool{}
	checkName := func(name *string) error {
		if err := checkIndexName(name); err != nil {
			return err
		}

		if names[*name] {
			return awserror.Validation("Duplicate index name: %s", *name)
		}

		names[*name] = true
		return nil
	}

	var globals []api.GlobalSecondaryIndexDescription
	for _, g := range in.GlobalSecondaryIndexes {
		if err := checkName(g.IndexName); err != nil {
			return nil, nil, err
		}

		d, err := describeGlobal(&g, types, used, tableArn, mode)
		if err != nil {
			return nil, nil, err
		}

		globals = append(globals, d)
	}

	var locals []api.LocalSecondaryIndexDescription
	for _, l := range in.LocalSecondaryIndexes {
		if err := checkName(l.IndexName); err != nil {
			return nil, nil, err
		}

		if err := checkKeySchema(l.KeySchema, types, used); err != nil {
			return nil, nil, err
		}

		if len(in.KeySchema) != 2 || len(l.KeySchema) != 2 ||
			common.Value(l.KeySchema[0].AttributeName) != common.Value(in.KeySchema[0].AttributeName) {
			return nil, nil, awserror.Validation("Local secondary index %s must have the table's HASH key and "+
				"a RANGE key, on a table with a RANGE key", *l.IndexName)
		}

		if err := checkProjection(l.Projection, *l.IndexName); err != nil {
			return nil, nil, err
		}

		locals = append(locals, api.LocalSecondaryIndexDescription{
			IndexName:      l.IndexName,
			KeySchema:      l.KeySchema,
			Projection:     l.Projection,
			IndexSizeBytes: new(int64(0)),
			ItemCount:      new(int64(0)),
			IndexArn:       new(tableArn + "/index/" + *l.IndexName),
		})
	}

	return globals, locals, nil
}
