package integration_test

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

func TestDynamoDBLegacyParameters(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	ctx := context.Background()
	const table = "legacy-items"
	if _, err := create(ctx, c, table); err != nil {
		t.Fatal(err)
	}

	it := key("one")
	it["a.b"] = &types.AttributeValueMemberS{Value: "literal"}
	it["counter"] = &types.AttributeValueMemberN{Value: "2"}
	insert := &sdk.PutItemInput{
		TableName: new(table), Item: it,
		Expected: map[string]types.ExpectedAttributeValue{"PK": {Exists: new(false)}},
	}
	if _, err := c.PutItem(ctx, insert); err != nil {
		t.Fatal(err)
	}

	_, err := c.PutItem(ctx, insert)
	code(t, err, "ConditionalCheckFailedException")

	updated, err := c.UpdateItem(ctx, &sdk.UpdateItemInput{
		TableName: new(table), Key: key("one"), ReturnValues: types.ReturnValueAllNew,
		AttributeUpdates: map[string]types.AttributeValueUpdate{
			"counter": {Action: types.AttributeActionAdd, Value: &types.AttributeValueMemberN{Value: "3"}},
			"tags":    {Value: &types.AttributeValueMemberSS{Value: []string{"urgent", "later"}}},
		},
		Expected: map[string]types.ExpectedAttributeValue{
			"counter": {Value: &types.AttributeValueMemberN{Value: "2"}},
			"a.b":     {Value: &types.AttributeValueMemberS{Value: "other"}},
		},
		ConditionalOperator: types.ConditionalOperatorOr,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := updated.Attributes["counter"].(*types.AttributeValueMemberN).Value; got != "5" {
		t.Fatalf("counter: got %s, want 5", got)
	}

	batch, err := c.BatchGetItem(ctx, &sdk.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		table: {Keys: []map[string]types.AttributeValue{key("one")}, AttributesToGet: []string{"a.b"}},
	}})
	if err != nil || len(batch.Responses[table]) != 1 || len(batch.Responses[table][0]) != 1 {
		t.Fatalf("BatchGetItem: %+v %v", batch, err)
	}

	get, err := c.GetItem(
		ctx,
		&sdk.GetItemInput{TableName: new(table), Key: key("one"), AttributesToGet: []string{"a.b"}},
	)
	if err != nil || len(get.Item) != 1 || get.Item["a.b"].(*types.AttributeValueMemberS).Value != "literal" {
		t.Fatalf("GetItem: %+v %v", get, err)
	}

	filter := map[string]types.Condition{
		"counter": {
			ComparisonOperator: types.ComparisonOperatorGt,
			AttributeValueList: []types.AttributeValue{&types.AttributeValueMemberN{Value: "3"}},
		},
		"tags": {
			ComparisonOperator: types.ComparisonOperatorContains,
			AttributeValueList: []types.AttributeValue{&types.AttributeValueMemberS{Value: "urgent"}},
		},
	}
	q, err := c.Query(ctx, &sdk.QueryInput{
		TableName: new(table), QueryFilter: filter, AttributesToGet: []string{"a.b"},
		KeyConditions: map[string]types.Condition{
			"PK": {
				ComparisonOperator: types.ComparisonOperatorEq,
				AttributeValueList: []types.AttributeValue{&types.AttributeValueMemberS{Value: "one"}},
			},
		},
	})
	if err != nil || q.Count != 1 || len(q.Items[0]) != 1 {
		t.Fatalf("Query: %+v %v", q, err)
	}

	scanned, err := c.Scan(ctx, &sdk.ScanInput{TableName: new(table), ScanFilter: filter, Select: types.SelectCount})
	if err != nil || scanned.Count != 1 {
		t.Fatalf("Scan: %+v %v", scanned, err)
	}

	_, err = c.Scan(ctx, &sdk.ScanInput{
		TableName: new(table), AttributesToGet: []string{"a.b"}, FilterExpression: new("attribute_exists(PK)"),
	})
	code(t, err, "ValidationException")
	var apiErr smithy.APIError
	const mixed = "Can not use both expression and non-expression parameters in the same request: " +
		"Non-expression parameters: {AttributesToGet} Expression parameters: {FilterExpression}"
	if !errors.As(err, &apiErr) || apiErr.ErrorMessage() != mixed {
		t.Fatalf("unexpected mixing error: %v", err)
	}

	if _, err := c.DeleteItem(ctx, &sdk.DeleteItemInput{
		TableName: new(table), Key: key("one"),
		Expected: map[string]types.ExpectedAttributeValue{
			"counter": {ComparisonOperator: types.ComparisonOperatorNotNull},
		},
	}); err != nil {
		t.Fatal(err)
	}
}
