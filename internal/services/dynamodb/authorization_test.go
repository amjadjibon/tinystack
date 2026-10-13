package dynamodb

import (
	"context"
	"slices"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestAuthorizationTransactionAndPartiQLTargets(t *testing.T) {
	s := New(&memory.Store{})
	defer s.Stop()
	ctx := request.WithMetadata(context.Background(), request.Metadata{Scope: request.Scope{
		AccountID: "000000000000", Region: "us-east-1",
	}})
	tests := []struct {
		input any
		name  string
		want  []string
	}{
		{
			name: "transaction per-item actions",
			input: &api.TransactWriteItemsInput{TransactItems: []api.TransactWriteItem{
				{Put: &api.Put{TableName: new("first")}}, {Delete: &api.Delete{TableName: new("second")}},
			}},
			want: []string{
				"dynamodb:PutItem arn:aws:dynamodb:us-east-1:000000000000:table/first",
				"dynamodb:DeleteItem arn:aws:dynamodb:us-east-1:000000000000:table/second",
			},
		},
		{
			name: "PartiQL distinct indexes on same table",
			input: &api.BatchExecuteStatementInput{Statements: []api.BatchStatementRequest{
				{Statement: new(`SELECT * FROM "example"."one"`)},
				{Statement: new(`SELECT * FROM "example"."two"`)},
			}},
			want: []string{
				"dynamodb:PartiQLSelect arn:aws:dynamodb:us-east-1:000000000000:table/example/index/one",
				"dynamodb:PartiQLSelect arn:aws:dynamodb:us-east-1:000000000000:table/example/index/two",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targets, err := s.AuthorizationResources(ctx, "", test.input)
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, target := range targets {
				got = append(got, target.Action+" "+target.ARN)
			}

			for _, want := range test.want {
				if !slices.Contains(got, want) {
					t.Fatalf("missing %s: %v", want, got)
				}
			}
		})
	}
}
