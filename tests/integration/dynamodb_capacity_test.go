package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDynamoDBCapacityAccounting(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	ctx := context.Background()
	in := &sdk.CreateTableInput{
		TableName: new("capacity-units"), BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: new("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: new("SK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: new("Index"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: new("PK"), KeyType: types.KeyTypeHash},
			{AttributeName: new("SK"), KeyType: types.KeyTypeRange},
		},
		LocalSecondaryIndexes: []types.LocalSecondaryIndex{{
			IndexName: new("capacity-lsi"), KeySchema: []types.KeySchemaElement{
				{AttributeName: new("PK"), KeyType: types.KeyTypeHash},
				{AttributeName: new("Index"), KeyType: types.KeyTypeRange},
			},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
		}},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName: new("capacity-gsi"), KeySchema: []types.KeySchemaElement{
				{AttributeName: new("Index"), KeyType: types.KeyTypeHash},
			},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
		}},
	}
	if _, err := c.CreateTable(ctx, in); err != nil {
		t.Fatal(err)
	}

	k := key("one")
	k["SK"] = &types.AttributeValueMemberS{Value: "sort"}
	it := key("one")
	it["SK"], it["Index"] = k["SK"], &types.AttributeValueMemberS{Value: "by-index"}
	it["payload"] = &types.AttributeValueMemberS{Value: strings.Repeat("a", 4096)}
	wrote, err := c.PutItem(ctx, &sdk.PutItemInput{
		TableName: in.TableName, Item: it, ReturnConsumedCapacity: types.ReturnConsumedCapacityIndexes,
		ReturnItemCollectionMetrics: types.ReturnItemCollectionMetricsSize,
	})
	if err != nil {
		t.Fatal(err)
	}

	usage := wrote.ConsumedCapacity
	if aws.ToFloat64(usage.CapacityUnits) != 7 || aws.ToFloat64(usage.Table.WriteCapacityUnits) != 5 ||
		aws.ToFloat64(usage.GlobalSecondaryIndexes["capacity-gsi"].WriteCapacityUnits) != 1 ||
		aws.ToFloat64(usage.LocalSecondaryIndexes["capacity-lsi"].WriteCapacityUnits) != 1 {
		t.Fatalf("write capacity: %+v", usage)
	}

	metrics := wrote.ItemCollectionMetrics
	if metrics == nil || metrics.ItemCollectionKey["PK"].(*types.AttributeValueMemberS).Value != "one" ||
		len(metrics.SizeEstimateRangeGB) != 2 || metrics.SizeEstimateRangeGB[1] <= 0 {
		t.Fatalf("collection metrics: %+v", metrics)
	}

	got, err := c.GetItem(ctx, &sdk.GetItemInput{
		TableName: in.TableName, Key: k, ReturnConsumedCapacity: types.ReturnConsumedCapacityTotal,
	})
	if err != nil || aws.ToFloat64(got.ConsumedCapacity.CapacityUnits) != 1 {
		t.Fatalf("eventual read: %+v %v", got, err)
	}

	tx, err := c.TransactGetItems(ctx, &sdk.TransactGetItemsInput{
		TransactItems:          []types.TransactGetItem{{Get: &types.Get{TableName: in.TableName, Key: k}}},
		ReturnConsumedCapacity: types.ReturnConsumedCapacityTotal,
	})
	if err != nil || len(tx.ConsumedCapacity) != 1 || aws.ToFloat64(tx.ConsumedCapacity[0].CapacityUnits) != 4 {
		t.Fatalf("transactional read: %+v %v", tx, err)
	}

	q, err := c.Query(ctx, &sdk.QueryInput{
		TableName: in.TableName, IndexName: new("capacity-gsi"), KeyConditionExpression: new("#i = :i"),
		ExpressionAttributeNames:  map[string]string{"#i": "Index"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":i": it["Index"]},
		ReturnConsumedCapacity:    types.ReturnConsumedCapacityIndexes,
	})
	if err != nil || q.ConsumedCapacity.Table != nil ||
		aws.ToFloat64(q.ConsumedCapacity.GlobalSecondaryIndexes["capacity-gsi"].ReadCapacityUnits) != 0.5 {
		t.Fatalf("index read: %+v %v", q, err)
	}
}

func TestDynamoDBProvisionedThrottle(t *testing.T) {
	cfg := testConfig()
	cfg.DynamoDB.EnforceProvisionedThroughput = true
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := client(s, "test", "us-east-1")
	ctx := context.Background()

	const table = "provisioned-burst"
	const index = "limited-index"
	if _, err := c.CreateTable(ctx, &sdk.CreateTableInput{
		TableName:   new(table),
		BillingMode: types.BillingModeProvisioned,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: new("PK"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: new("Index"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{{AttributeName: new("PK"), KeyType: types.KeyTypeHash}},
		ProvisionedThroughput: &types.ProvisionedThroughput{
			ReadCapacityUnits:  new(int64(1)),
			WriteCapacityUnits: new(int64(2)),
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName:  new(index),
			KeySchema:  []types.KeySchemaElement{{AttributeName: new("Index"), KeyType: types.KeyTypeHash}},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			ProvisionedThroughput: &types.ProvisionedThroughput{
				ReadCapacityUnits: new(int64(1)), WriteCapacityUnits: new(int64(1)),
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	it := key("large")
	it["Index"] = &types.AttributeValueMemberS{Value: "indexed"}
	it["data"] = &types.AttributeValueMemberS{Value: strings.Repeat("x", 300*1024)}
	_, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: new(table), Item: it})
	code(t, err, "ProvisionedThroughputExceededException")
	expectBuiltin(t, cloudwatchClient(s), "AWS/DynamoDB", "ThrottledRequests", "Count",
		dimensions("TableName", table, "Operation", "PutItem", "GlobalSecondaryIndexName", index), 1)

	got, err := c.GetItem(ctx, &sdk.GetItemInput{TableName: new(table), Key: key("large")})
	if err != nil || len(got.Item) != 0 {
		t.Fatalf("rejected write leaked: %+v %v", got, err)
	}

	if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: new(table), Item: key("small")}); err != nil {
		t.Fatal(err)
	}
}
