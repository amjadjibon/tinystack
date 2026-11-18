package keyspaces

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/keyspaces"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type retryRuntime struct {
	statements []string
}

func (*retryRuntime) Ensure(context.Context, request.Scope) (string, error) {
	return "127.0.0.1:12345", nil
}
func (*retryRuntime) Cleanup(context.Context, bool) error { return nil }

func (r *retryRuntime) Apply(_ context.Context, _ request.Scope, statement string) error {
	r.statements = append(r.statements, statement)
	if len(r.statements) == 2 {
		return errors.New("temporary CQL failure")
	}

	return nil
}

func TestCassandraRuntimeSeedsAndRetriesInOrder(t *testing.T) {
	scope := request.Scope{AccountID: "000000000000", Region: "us-east-1"}
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: scope})
	store := &memory.Store{}
	s := New(store)
	const ks = "app"
	if _, err := s.CreateKeyspace(ctx, &api.CreateKeyspaceRequest{KeyspaceName: new(ks)}); err != nil {
		t.Fatal(err)
	}

	for _, def := range []struct{ name, fieldType string }{{"address", "text"}, {"a_parent", "frozen<address>"}} {
		_, err := s.CreateType(ctx, &api.CreateTypeRequest{
			KeyspaceName: new(ks), TypeName: new(def.name),
			FieldDefinitions: []api.FieldDefinition{{Name: new("field"), Type: new(def.fieldType)}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	table := &api.GetTableRequest{KeyspaceName: new(ks), TableName: new("events")}
	_, err := s.CreateTable(ctx, &api.CreateTableRequest{
		KeyspaceName: table.KeyspaceName, TableName: table.TableName,
		SchemaDefinition: &api.SchemaDefinition{
			AllColumns:    []api.ColumnDefinition{{Name: new("id"), Type: new("text")}},
			PartitionKeys: []api.PartitionKey{{Name: new("id")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	engine := &retryRuntime{}
	s.runtime = engine
	if err = store.Update(ctx, s.seedRuntime); err != nil {
		t.Fatal(err)
	}

	if err = s.reconcile(ctx, scope); err == nil || err.Error() != "temporary CQL failure" {
		t.Fatalf("first CQL apply error: %v", err)
	}

	out, err := s.GetTable(ctx, table)
	if err != nil || common.Value(out.Status) != creating {
		t.Fatalf("status while CQL is blocked: %v %v", out, err)
	}

	if err = s.reconcile(ctx, scope); err != nil {
		t.Fatal(err)
	}

	out, err = s.GetTable(ctx, table)
	if err != nil || common.Value(out.Status) != active {
		t.Fatalf("status after retry: %v %v", out, err)
	}

	objects := make([]string, 0, len(engine.statements))
	for _, statement := range engine.statements {
		_, rest, ok := strings.Cut(statement, "IF NOT EXISTS ")
		if !ok {
			t.Fatalf("unexpected statement: %s", statement)
		}

		name, _, _ := strings.Cut(rest, " ")
		objects = append(objects, name)
	}

	if !slices.Equal(
		objects,
		[]string{`"app"`, `"app"."address"`, `"app"."address"`, `"app"."a_parent"`, `"app"."events"`},
	) {
		t.Fatalf("engine applied changes in wrong order: %v", objects)
	}
}
