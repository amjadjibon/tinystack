package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDynamoDBExecuteTransaction(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	if _, err := create(ctx, c, "ledger"); err != nil {
		t.Fatal(err)
	}

	str := func(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
	stmt := func(text string, params ...types.AttributeValue) types.ParameterizedStatement {
		return types.ParameterizedStatement{Statement: aws.String(text), Parameters: params}
	}

	if _, err := c.ExecuteTransaction(ctx, &sdk.ExecuteTransactionInput{
		TransactStatements: []types.ParameterizedStatement{
			stmt(`INSERT INTO "ledger" VALUE {'PK': ?, 'balance': 10}`, str("a")),
			stmt(`INSERT INTO "ledger" VALUE {'PK': ?, 'balance': 5}`, str("b")),
		},
		ClientRequestToken: aws.String("first"),
	}); err != nil {
		t.Fatal(err)
	}

	// An INSERT of an existing item cancels the whole transaction.
	_, err := c.ExecuteTransaction(ctx, &sdk.ExecuteTransactionInput{
		TransactStatements: []types.ParameterizedStatement{
			stmt(`INSERT INTO "ledger" VALUE {'PK': 'c', 'balance': 1}`),
			stmt(`INSERT INTO "ledger" VALUE {'PK': 'a', 'balance': 1}`),
		},
	})
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) || len(canceled.CancellationReasons) != 2 ||
		aws.ToString(canceled.CancellationReasons[0].Code) != "None" ||
		aws.ToString(canceled.CancellationReasons[1].Code) != "DuplicateItem" {
		t.Fatalf("%v", err)
	}

	if _, err = c.ExecuteTransaction(ctx, &sdk.ExecuteTransactionInput{
		TransactStatements: []types.ParameterizedStatement{
			stmt(`UPDATE "ledger" SET balance = balance - 3 WHERE PK = 'a' AND balance >= 3`),
			stmt(`UPDATE "ledger" SET balance = balance + 3 WHERE PK = 'b'`),
			stmt(`DELETE FROM "ledger" WHERE PK = 'c'`),
		},
	}); err != nil {
		t.Fatal(err)
	}

	read, err := c.ExecuteTransaction(ctx, &sdk.ExecuteTransactionInput{
		TransactStatements: []types.ParameterizedStatement{
			stmt(`SELECT balance FROM "ledger" WHERE PK = 'a'`),
			stmt(`SELECT * FROM "ledger" WHERE PK = ?`, str("b")),
			stmt(`SELECT * FROM "ledger" WHERE PK = 'c'`),
		},
	})
	if err != nil || len(read.Responses) != 3 {
		t.Fatalf("%+v %v", read, err)
	}

	balance := func(item map[string]types.AttributeValue) string {
		n, _ := item["balance"].(*types.AttributeValueMemberN)
		if n == nil {
			return ""
		}

		return n.Value
	}

	if balance(read.Responses[0].Item) != "7" || read.Responses[0].Item["PK"] != nil ||
		balance(read.Responses[1].Item) != "8" || read.Responses[2].Item != nil {
		t.Fatalf("%+v", read.Responses)
	}

	for _, statements := range [][]types.ParameterizedStatement{
		{stmt(`SELECT * FROM "ledger" WHERE PK = 'a'`), stmt(`DELETE FROM "ledger" WHERE PK = 'a'`)},
		{stmt(`SELECT * FROM "ledger"`)},
		{stmt(`DELETE FROM "ledger" WHERE PK = 'a' RETURNING ALL OLD *`)},
	} {
		_, err = c.ExecuteTransaction(ctx, &sdk.ExecuteTransactionInput{TransactStatements: statements})
		code(t, err, "ValidationException")
	}
}
