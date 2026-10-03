package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDynamoDBUpdateTable(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := create(ctx, c, "users"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"a", "b"} {
		item := key(id)
		item["Team"] = &types.AttributeValueMemberS{Value: "red"}
		if _, err := c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("users"), Item: item}); err != nil {
			t.Fatal(err)
		}
	}

	team := []types.AttributeDefinition{{AttributeName: aws.String("Team"), AttributeType: types.ScalarAttributeTypeS}}
	createIndex := func(name string) types.GlobalSecondaryIndexUpdate {
		return types.GlobalSecondaryIndexUpdate{Create: &types.CreateGlobalSecondaryIndexAction{
			IndexName:  aws.String(name),
			KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("Team"), KeyType: types.KeyTypeHash}},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}}
	}

	_, err := c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"), AttributeDefinitions: team,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{createIndex("one"), createIndex("two")},
	})
	code(t, err, "LimitExceededException")

	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName:                   aws.String("users"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{createIndex("by-team")},
	})
	code(t, err, "ValidationException")

	// A new index serves the items already in the table.
	updated, err := c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"), AttributeDefinitions: team,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{createIndex("by-team")},
	})
	if err != nil || len(updated.TableDescription.GlobalSecondaryIndexes) != 1 ||
		updated.TableDescription.GlobalSecondaryIndexes[0].IndexStatus != types.IndexStatusActive ||
		len(updated.TableDescription.AttributeDefinitions) != 2 {
		t.Fatalf("created %+v, %v", updated, err)
	}

	queried, err := c.Query(ctx, &sdk.QueryInput{
		TableName: aws.String("users"), IndexName: aws.String("by-team"),
		KeyConditionExpression:    aws.String("Team = :t"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": &types.AttributeValueMemberS{Value: "red"}},
	})
	if err != nil || queried.Count != 2 {
		t.Fatalf("queried %+v, %v", queried, err)
	}

	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"), AttributeDefinitions: team,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{createIndex("by-team")},
	})
	code(t, err, "ValidationException")

	// Switching to PROVISIONED needs throughput for the table and each index.
	capacity := &types.ProvisionedThroughput{ReadCapacityUnits: aws.Int64(5), WriteCapacityUnits: aws.Int64(3)}
	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"), BillingMode: types.BillingModeProvisioned, ProvisionedThroughput: capacity,
	})
	code(t, err, "ValidationException")

	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"), ProvisionedThroughput: capacity,
	})
	code(t, err, "ValidationException")

	updated, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName:             aws.String("users"),
		BillingMode:           types.BillingModeProvisioned,
		ProvisionedThroughput: capacity,
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{Update: &types.UpdateGlobalSecondaryIndexAction{
				IndexName: aws.String("by-team"), ProvisionedThroughput: capacity,
			}},
		},
	})
	d := updated.TableDescription
	if err != nil || d.BillingModeSummary.BillingMode != types.BillingModeProvisioned ||
		aws.ToInt64(d.ProvisionedThroughput.ReadCapacityUnits) != 5 ||
		aws.ToInt64(d.GlobalSecondaryIndexes[0].ProvisionedThroughput.WriteCapacityUnits) != 3 {
		t.Fatalf("provisioned %+v, %v", d, err)
	}

	_, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{
			{Update: &types.UpdateGlobalSecondaryIndexAction{
				IndexName: aws.String("missing"), ProvisionedThroughput: capacity,
			}},
		},
	})
	code(t, err, "ResourceNotFoundException")

	updated, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"), BillingMode: types.BillingModePayPerRequest,
	})
	d = updated.TableDescription
	if err != nil || d.BillingModeSummary.BillingMode != types.BillingModePayPerRequest ||
		aws.ToInt64(d.GlobalSecondaryIndexes[0].ProvisionedThroughput.ReadCapacityUnits) != 0 {
		t.Fatalf("on demand %+v, %v", d, err)
	}

	// Deleting the index drops the attribute definition only it used.
	updated, err = c.UpdateTable(ctx, &sdk.UpdateTableInput{
		TableName: aws.String("users"),
		GlobalSecondaryIndexUpdates: []types.GlobalSecondaryIndexUpdate{{
			Delete: &types.DeleteGlobalSecondaryIndexAction{IndexName: aws.String("by-team")},
		}},
	})
	if err != nil || len(updated.TableDescription.GlobalSecondaryIndexes) != 0 ||
		len(updated.TableDescription.AttributeDefinitions) != 1 {
		t.Fatalf("deleted %+v, %v", updated, err)
	}

	_, err = c.Query(ctx, &sdk.QueryInput{
		TableName: aws.String("users"), IndexName: aws.String("by-team"),
		KeyConditionExpression:    aws.String("Team = :t"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": &types.AttributeValueMemberS{Value: "red"}},
	})
	code(t, err, "ValidationException")
}
