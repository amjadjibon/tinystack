package keyspaces

import (
	"fmt"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/common"
)

func identifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func literal(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func qualified(keyspace, name string) string {
	return identifier(keyspace) + "." + identifier(name)
}

const localReplication = " WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}"

func createKeyspaceCQL(name string) string {
	return "CREATE KEYSPACE IF NOT EXISTS " + identifier(name) + localReplication
}

func createTypeCQL(r *record, defined map[string]int64) (string, error) {
	fields := make([]string, 0, len(r.Fields))
	for _, field := range r.Fields {
		ct, err := parseType(common.Value(field.Type), defined)
		if err != nil {
			return "", err
		}

		fields = append(fields, identifier(common.Value(field.Name))+" "+ct.text)
	}

	return "CREATE TYPE IF NOT EXISTS " + qualified(r.Keyspace, r.Name) + " (" + strings.Join(fields, ", ") + ")", nil
}

func createTableCQL(table *api.GetTableResponse, types map[string]cqlType) string {
	schema := table.SchemaDefinition
	parts := make([]string, 0, len(schema.AllColumns)+1)
	statics := map[string]bool{}
	for _, col := range schema.StaticColumns {
		statics[common.Value(col.Name)] = true
	}

	for _, col := range schema.AllColumns {
		name := common.Value(col.Name)
		part := identifier(name) + " " + types[name].text
		if statics[name] {
			part += " STATIC"
		}

		parts = append(parts, part)
	}

	partition := make([]string, 0, len(schema.PartitionKeys))
	for _, key := range schema.PartitionKeys {
		partition = append(partition, identifier(common.Value(key.Name)))
	}

	primary := []string{"(" + strings.Join(partition, ", ") + ")"}
	order := make([]string, 0, len(schema.ClusteringKeys))
	for _, key := range schema.ClusteringKeys {
		name := identifier(common.Value(key.Name))
		primary = append(primary, name)
		order = append(order, name+" "+common.Value(key.OrderBy))
	}

	parts = append(parts, "PRIMARY KEY ("+strings.Join(primary, ", ")+")")
	cte := "CREATE TABLE IF NOT EXISTS " + qualified(common.Value(table.KeyspaceName), common.Value(table.TableName))
	statement := cte + " (" + strings.Join(parts, ", ") + ")"
	var options []string
	if len(order) != 0 {
		options = append(options, "CLUSTERING ORDER BY ("+strings.Join(order, ", ")+")")
	}

	if ttl := common.Value(table.DefaultTimeToLive); ttl > 0 {
		options = append(options, fmt.Sprintf("default_time_to_live = %d", ttl))
	}

	if table.Comment != nil {
		options = append(options, "comment = "+literal(common.Value(table.Comment.Message)))
	}

	if len(options) != 0 {
		statement += " WITH " + strings.Join(options, " AND ")
	}

	return statement
}

func alterTableCQL(in *api.UpdateTableRequest, types map[string]cqlType) []string {
	prefix := "ALTER TABLE " + qualified(common.Value(in.KeyspaceName), common.Value(in.TableName))
	var statements []string
	for _, col := range in.AddColumns {
		name := common.Value(col.Name)
		statements = append(statements, prefix+" ADD IF NOT EXISTS "+identifier(name)+" "+types[name].text)
	}

	if in.DefaultTimeToLive != nil {
		statements = append(statements, fmt.Sprintf("%s WITH default_time_to_live = %d", prefix, *in.DefaultTimeToLive))
	}

	return statements
}
