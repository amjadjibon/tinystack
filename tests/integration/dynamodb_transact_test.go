package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestBatchItems(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	for _, name := range []string{"users", "teams"} {
		if _, err := create(ctx, c, name); err != nil {
			t.Fatal(err)
		}
	}

	var writes []types.WriteRequest
	for i := range 20 {
		it := key(fmt.Sprint("u", i))
		it["n"] = n(fmt.Sprint(i))
		writes = append(writes, types.WriteRequest{PutRequest: &types.PutRequest{Item: it}})
	}

	_, err := c.BatchWriteItem(ctx, &sdk.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"users": writes,
		"teams": {{PutRequest: &types.PutRequest{Item: key("t1")}}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// Deletes and puts mix; deleting an absent item is not an error.
	out, err := c.BatchWriteItem(ctx, &sdk.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"users": {
			{DeleteRequest: &types.DeleteRequest{Key: key("u0")}},
			{DeleteRequest: &types.DeleteRequest{Key: key("missing")}},
		},
	}})
	if err != nil || len(out.UnprocessedItems) != 0 {
		t.Fatalf("delete: %v %v", out, err)
	}

	got, err := c.BatchGetItem(ctx, &sdk.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"users": {
			Keys:                 []map[string]types.AttributeValue{key("u0"), key("u1"), key("u19")},
			ProjectionExpression: aws.String("PK"),
		},
		"teams": {Keys: []map[string]types.AttributeValue{key("t1")}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Responses["users"]) != 2 || len(got.Responses["teams"]) != 1 || len(got.UnprocessedKeys) != 0 {
		t.Fatalf("responses = %v", got.Responses)
	}

	if _, ok := got.Responses["users"][0]["n"]; ok {
		t.Fatalf("projection ignored: %v", got.Responses["users"][0])
	}

	d, err := c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("users")})
	if err != nil || *d.Table.ItemCount != 19 {
		t.Fatalf("item count = %v %v", d, err)
	}

	// Duplicate keys, too many writes, and a missing table reject the whole batch.
	_, err = c.BatchWriteItem(ctx, &sdk.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"users": {
			{PutRequest: &types.PutRequest{Item: key("dup")}},
			{DeleteRequest: &types.DeleteRequest{Key: key("dup")}},
		},
	}})
	code(t, err, "ValidationException")

	_, err = c.BatchWriteItem(ctx, &sdk.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{
		"users": append(writes, writes[:6]...),
	}})
	code(t, err, "ValidationException")

	_, err = c.BatchGetItem(ctx, &sdk.BatchGetItemInput{RequestItems: map[string]types.KeysAndAttributes{
		"missing": {Keys: []map[string]types.AttributeValue{key("x")}},
	}})
	code(t, err, "ResourceNotFoundException")

	g, err := c.GetItem(ctx, &sdk.GetItemInput{TableName: aws.String("users"), Key: key("dup")})
	if err != nil || g.Item != nil {
		t.Fatalf("rejected batch wrote: %v %v", g, err)
	}
}

func TestTransactions(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	if _, err := create(ctx, c, "accounts"); err != nil {
		t.Fatal(err)
	}

	balance := func(id, amount string) map[string]types.AttributeValue {
		it := key(id)
		it["balance"] = n(amount)
		return it
	}

	_, err := c.TransactWriteItems(ctx, &sdk.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: aws.String("accounts"), Item: balance("a", "100")}},
		{Put: &types.Put{TableName: aws.String("accounts"), Item: balance("b", "0")}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	transfer := func(amount string) *sdk.TransactWriteItemsInput {
		values := map[string]types.AttributeValue{":v": n(amount)}
		return &sdk.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
			{Update: &types.Update{
				TableName:                           aws.String("accounts"),
				Key:                                 key("a"),
				UpdateExpression:                    aws.String("SET balance = balance - :v"),
				ConditionExpression:                 aws.String("balance >= :v"),
				ExpressionAttributeValues:           values,
				ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
			}},
			{Update: &types.Update{
				TableName:                 aws.String("accounts"),
				Key:                       key("b"),
				UpdateExpression:          aws.String("SET balance = balance + :v"),
				ExpressionAttributeValues: values,
			}},
			{ConditionCheck: &types.ConditionCheck{
				TableName:           aws.String("accounts"),
				Key:                 key("b"),
				ConditionExpression: aws.String("attribute_exists(PK)"),
			}},
		}}
	}

	if _, err = c.TransactWriteItems(ctx, transfer("30")); err == nil {
		t.Fatal("an update and a condition check on one item were accepted")
	}

	code(t, err, "ValidationException")

	in := transfer("30")
	in.TransactItems = in.TransactItems[:2]
	if _, err = c.TransactWriteItems(ctx, in); err != nil {
		t.Fatal(err)
	}

	// An overdraft cancels the whole transaction, with a reason per item.
	in = transfer("500")
	in.TransactItems = in.TransactItems[:2]
	_, err = c.TransactWriteItems(ctx, in)
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		t.Fatalf("wanted TransactionCanceledException, got %v", err)
	}

	reasons := canceled.CancellationReasons
	if len(reasons) != 2 || *reasons[0].Code != "ConditionalCheckFailed" || *reasons[1].Code != "None" ||
		reasons[0].Item["balance"].(*types.AttributeValueMemberN).Value != "70" {
		t.Fatalf("reasons = %+v", reasons)
	}

	got, err := c.TransactGetItems(ctx, &sdk.TransactGetItemsInput{TransactItems: []types.TransactGetItem{
		{Get: &types.Get{TableName: aws.String("accounts"), Key: key("a")}},
		{
			Get: &types.Get{
				TableName:            aws.String("accounts"),
				Key:                  key("b"),
				ProjectionExpression: aws.String("balance"),
			},
		},
		{Get: &types.Get{TableName: aws.String("accounts"), Key: key("nobody")}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	r := got.Responses
	if len(r) != 3 || r[0].Item["balance"].(*types.AttributeValueMemberN).Value != "70" ||
		r[1].Item["balance"].(*types.AttributeValueMemberN).Value != "30" || len(r[1].Item) != 1 ||
		r[2].Item != nil {
		t.Fatalf("responses = %+v", r)
	}

	// A repeated ClientRequestToken succeeds without applying the writes again;
	// reusing it for other writes is a mismatch.
	in = transfer("10")
	in.TransactItems = in.TransactItems[:2]
	in.ClientRequestToken = aws.String("token-1")
	for range 2 {
		if _, err = c.TransactWriteItems(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	g, err := c.GetItem(ctx, &sdk.GetItemInput{TableName: aws.String("accounts"), Key: key("a")})
	if err != nil || g.Item["balance"].(*types.AttributeValueMemberN).Value != "60" {
		t.Fatalf("after idempotent retry: %v %v", g, err)
	}

	other := transfer("5")
	other.TransactItems = other.TransactItems[:2]
	other.ClientRequestToken = in.ClientRequestToken
	_, err = c.TransactWriteItems(ctx, other)
	code(t, err, "IdempotentParameterMismatchException")
}
