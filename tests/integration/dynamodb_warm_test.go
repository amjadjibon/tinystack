package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDynamoDBDefaultWarmThroughput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	c := client(s, "test", "us-east-1")
	out, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   new("warm-table"),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: new("pk"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{{AttributeName: new("pk"), KeyType: types.KeyTypeHash}},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName:  new("by-key"),
			KeySchema:  []types.KeySchemaElement{{AttributeName: new("pk"), KeyType: types.KeyTypeHash}},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = client(s, "test", "us-east-1")
	desc, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: out.TableDescription.TableName})
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range []*types.TableDescription{out.TableDescription, desc.Table} {
		w := d.WarmThroughput
		if w == nil || w.Status != types.TableStatusActive || aws.ToInt64(w.ReadUnitsPerSecond) != 12000 ||
			aws.ToInt64(w.WriteUnitsPerSecond) != 4000 {
			t.Fatalf("table warm throughput: %+v", w)
		}

		g := d.GlobalSecondaryIndexes[0]
		if g.WarmThroughput == nil || g.WarmThroughput.Status != types.IndexStatusActive ||
			aws.ToInt64(g.WarmThroughput.ReadUnitsPerSecond) != 12000 || len(g.KeySchema) != 1 {
			t.Fatalf("index description: %+v", g)
		}
	}
}
