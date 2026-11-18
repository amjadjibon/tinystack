package keyspaces

import (
	"slices"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
)

func exampleSchema() *api.SchemaDefinition {
	return &api.SchemaDefinition{
		AllColumns: []api.ColumnDefinition{
			{Name: new("id"), Type: new("uuid")},
			{Name: new("tenant"), Type: new("text")},
			{Name: new("time"), Type: new("timestamp")},
			{Name: new("note"), Type: new("text")},
			{Name: new("details"), Type: new("map<text, frozen<address>>")},
		},
		PartitionKeys:  []api.PartitionKey{{Name: new("id")}, {Name: new("tenant")}},
		ClusteringKeys: []api.ClusteringKey{{Name: new("time"), OrderBy: new("DESC")}},
		StaticColumns:  []api.StaticColumn{{Name: new("note")}},
	}
}

func TestCassandraSchemaValidation(t *testing.T) {
	defined := map[string]int64{"address": 1}
	types, refs, err := schemaTypes(exampleSchema(), defined)
	if err != nil || types["details"].text != `map<text, frozen<"address">>` ||
		!slices.Equal(refs, []string{"address"}) {
		t.Fatalf("schema validation: %v %v %v", types, refs, err)
	}

	for _, input := range []string{
		"unknown", "list<text", "map<int>", "map<int,text,int>", "int; DROP TABLE foo",
		"list<list<int>>", "frozen<int>", "set<duration>", "tuple<counter>",
	} {
		t.Run(input, func(t *testing.T) {
			if _, e := parseType(input, defined); e == nil {
				t.Fatalf("accepted type %q", input)
			}
		})
	}

	for name, change := range map[string]func(*api.SchemaDefinition){
		"partition required":        func(s *api.SchemaDefinition) { s.PartitionKeys = nil },
		"undefined key":             func(s *api.SchemaDefinition) { s.PartitionKeys[0].Name = new("missing") },
		"duplicate key":             func(s *api.SchemaDefinition) { s.ClusteringKeys[0].Name = new("tenant") },
		"invalid order":             func(s *api.SchemaDefinition) { s.ClusteringKeys[0].OrderBy = new("SIDEWAYS") },
		"duplicate column":          func(s *api.SchemaDefinition) { s.AllColumns = append(s.AllColumns, s.AllColumns[0]) },
		"non-frozen key":            func(s *api.SchemaDefinition) { s.PartitionKeys[0].Name = new("details") },
		"static key":                func(s *api.SchemaDefinition) { s.StaticColumns[0].Name = new("id") },
		"static without clustering": func(s *api.SchemaDefinition) { s.ClusteringKeys = nil },
		"mixed counter":             func(s *api.SchemaDefinition) { s.AllColumns[3].Type = new("counter") },
	} {
		t.Run(name, func(t *testing.T) {
			schema := exampleSchema()
			change(schema)
			if _, _, e := schemaTypes(schema, defined); e == nil {
				t.Fatal("accepted invalid schema")
			}
		})
	}
}

func TestCassandraCQL(t *testing.T) {
	schema := exampleSchema()
	defined := map[string]int64{"address": 1}
	types, _, err := schemaTypes(schema, defined)
	if err != nil {
		t.Fatal(err)
	}

	table := &api.GetTableResponse{
		KeyspaceName: new("CaseSensitive"), TableName: new("events"), SchemaDefinition: schema,
		DefaultTimeToLive: new(int64(60)), Comment: &api.Comment{Message: new("it's a table")},
	}
	expected := `CREATE TABLE IF NOT EXISTS "CaseSensitive"."events" ("id" uuid, "tenant" text, ` +
		`"time" timestamp, "note" text STATIC, "details" map<text, frozen<"address">>, ` +
		`PRIMARY KEY (("id", "tenant"), "time")) WITH CLUSTERING ORDER BY ("time" DESC) ` +
		`AND default_time_to_live = 60 AND comment = 'it''s a table'`
	if got := createTableCQL(table, types); got != expected {
		t.Fatalf("statement: %s", got)
	}

	changes := &api.UpdateTableRequest{
		KeyspaceName: table.KeyspaceName, TableName: table.TableName, DefaultTimeToLive: new(int64(0)),
		AddColumns: []api.ColumnDefinition{{Name: new("details"), Type: new("map<text, frozen<address>>")}},
	}
	if got := alterTableCQL(changes, types); !slices.Equal(got, []string{
		`ALTER TABLE "CaseSensitive"."events" ADD IF NOT EXISTS "details" map<text, frozen<"address">>`,
		`ALTER TABLE "CaseSensitive"."events" WITH default_time_to_live = 0`,
	}) {
		t.Fatalf("statements: %v", got)
	}

	udt := &record{Keyspace: "CaseSensitive", Name: "place", Fields: []api.FieldDefinition{
		{Name: new("street"), Type: new("frozen<address>")},
	}}
	statement, err := createTypeCQL(udt, defined)
	if err != nil || statement != `CREATE TYPE IF NOT EXISTS "CaseSensitive"."place" ("street" frozen<"address">)` {
		t.Fatalf("type statement: %s (%v)", statement, err)
	}

	if got := createKeyspaceCQL(
		"CaseSensitive",
	); !strings.HasPrefix(
		got,
		`CREATE KEYSPACE IF NOT EXISTS "CaseSensitive"`,
	) {
		t.Fatalf("keyspace statement: %s", got)
	}
}
