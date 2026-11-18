package keyspaces

import (
	"regexp"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/common"
)

var (
	columnName     = regexp.MustCompile(`^[A-Za-z0-9_]{1,48}$`)
	typeIdentifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,47}$`)
	typeToken      = regexp.MustCompile(`^(?:"[A-Za-z][A-Za-z0-9_]*"|[A-Za-z][A-Za-z0-9_]*)`)
)

const (
	frozen    = "frozen"
	mapType   = "map"
	tupleType = "tuple"
)

type cqlType struct {
	text      string
	refs      []string
	multiCell bool
	counter   bool
	duration  bool
	freezable bool
}

func scalar(name string) bool {
	switch name {
	case "ascii", "bigint", "blob", "boolean", "counter", "date", "decimal", "double", "duration", "float",
		"inet", "int", "smallint", "text", "time", "timestamp", "timeuuid", "tinyint", "uuid", "varchar", "varint":
		return true
	default:
		return false
	}
}

func typeName(input string) (string, error) {
	name := strings.ToLower(input)
	if len(input) >= 2 && input[0] == '"' && input[len(input)-1] == '"' {
		name = input[1 : len(input)-1]
	}

	if !typeIdentifier.MatchString(name) || scalar(strings.ToLower(name)) {
		return "", awserror.Validation("Invalid user-defined type name %q", input)
	}

	return name, nil
}

type typeParser struct {
	defined map[string]int64
	rest    string
}

func (p *typeParser) consume(token string) bool {
	p.rest = strings.TrimSpace(p.rest)
	if !strings.HasPrefix(p.rest, token) {
		return false
	}

	p.rest = p.rest[len(token):]
	return true
}

func (p *typeParser) parse(depth int) (cqlType, bool) {
	if depth > 16 {
		return cqlType{}, false
	}

	p.rest = strings.TrimSpace(p.rest)
	token := typeToken.FindString(p.rest)
	p.rest = p.rest[len(token):]
	name := strings.ToLower(token)
	switch name {
	case "list", "set", mapType, tupleType, frozen:
		if p.consume("<") {
			return p.composite(name, depth)
		}
	}

	if scalar(name) {
		return cqlType{text: name, counter: name == "counter", duration: name == "duration"}, true
	}

	ref, err := typeName(token)
	if _, ok := p.defined[ref]; err != nil || !ok {
		return cqlType{}, false
	}

	return cqlType{text: identifier(ref), refs: []string{ref}, multiCell: true, freezable: true}, true
}

func (p *typeParser) composite(name string, depth int) (cqlType, bool) {
	var result cqlType
	var parts []string
	for {
		child, ok := p.parse(depth + 1)
		if !ok || child.counter || (name == frozen && !child.freezable) ||
			(name != frozen && child.multiCell) ||
			(child.duration && (name == "set" || (name == mapType && len(parts) == 0))) {
			return cqlType{}, false
		}

		parts = append(parts, child.text)
		result.refs = append(result.refs, child.refs...)
		result.duration = result.duration || child.duration
		if !p.consume(",") {
			break
		}
	}

	if !p.consume(">") ||
		(name == mapType && len(parts) != 2) || (name != mapType && name != tupleType && len(parts) != 1) {
		return cqlType{}, false
	}

	result.text = name + "<" + strings.Join(parts, ", ") + ">"
	result.freezable = name != frozen
	result.multiCell = name != frozen && name != tupleType
	return result, true
}

func parseType(input string, defined map[string]int64) (cqlType, error) {
	p := typeParser{rest: input, defined: defined}
	value, valid := p.parse(0)
	if !valid || strings.TrimSpace(p.rest) != "" {
		return cqlType{}, awserror.Validation("Unsupported or invalid Cassandra type %q", input)
	}

	return value, nil
}

func schemaTypes(schema *api.SchemaDefinition, defined map[string]int64) (
	columns map[string]cqlType, refs []string, err error,
) {
	if schema == nil || len(schema.AllColumns) == 0 || len(schema.PartitionKeys) == 0 {
		return nil, nil, awserror.Validation("A schema requires columns and at least one partition key")
	}

	columns = make(map[string]cqlType, len(schema.AllColumns))
	for _, col := range schema.AllColumns {
		name := common.Value(col.Name)
		if _, exists := columns[name]; !columnName.MatchString(name) || exists {
			return nil, nil, awserror.Validation("Invalid or duplicate column %q", name)
		}

		ct, err := parseType(common.Value(col.Type), defined)
		if err != nil {
			return nil, nil, err
		}

		columns[name] = ct
		refs = append(refs, ct.refs...)
	}

	keys := map[string]bool{}
	for _, pk := range schema.PartitionKeys {
		if err := checkKey(common.Value(pk.Name), columns, keys); err != nil {
			return nil, nil, err
		}
	}

	for _, ck := range schema.ClusteringKeys {
		if order := common.Value(ck.OrderBy); order != "ASC" && order != "DESC" {
			return nil, nil, awserror.Validation("Clustering order must be ASC or DESC")
		}

		if err := checkKey(common.Value(ck.Name), columns, keys); err != nil {
			return nil, nil, err
		}
	}

	statics := map[string]bool{}
	for _, col := range schema.StaticColumns {
		name := common.Value(col.Name)
		if _, found := columns[name]; !found || keys[name] || statics[name] || len(schema.ClusteringKeys) == 0 {
			return nil, nil, awserror.Validation("Invalid static column %q", name)
		}

		statics[name] = true
	}

	var counters, regular int
	for name, ct := range columns {
		switch {
		case ct.counter:
			counters++
		case !keys[name]:
			regular++
		}
	}

	if counters > 0 && regular > 0 {
		return nil, nil, awserror.Validation("Counter tables require every non-key column to be a counter")
	}

	slices.Sort(refs)
	return columns, slices.Compact(refs), nil
}

func checkKey(name string, columns map[string]cqlType, keys map[string]bool) error {
	ct, found := columns[name]
	if !found || keys[name] || ct.multiCell || ct.counter || ct.duration {
		return awserror.Validation("Invalid or duplicate primary key column %q", name)
	}

	keys[name] = true
	return nil
}
