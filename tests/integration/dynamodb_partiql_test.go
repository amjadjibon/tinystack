package integration_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func statement(
	t *testing.T,
	c *sdk.Client,
	text string,
	params ...types.AttributeValue,
) []map[string]types.AttributeValue {
	t.Helper()
	out, err := c.ExecuteStatement(context.Background(), &sdk.ExecuteStatementInput{
		Statement: aws.String(text), Parameters: params,
	})
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}

	return out.Items
}

func str(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}

	if v, ok := item[name].(*types.AttributeValueMemberN); ok {
		return v.Value
	}

	return ""
}

func TestDynamoDBPartiQL(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateTable(ctx, &sdk.CreateTableInput{
		TableName: aws.String("music"), BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("Artist"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("Track"), AttributeType: types.ScalarAttributeTypeN},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("Artist"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("Track"), KeyType: types.KeyTypeRange},
		},
	}); err != nil {
		t.Fatal(err)
	}

	statement(t, c, `INSERT INTO "music" VALUE {'Artist': 'Nina', 'Track': 1, 'Title': 'Sinnerman', 'Plays': 10, `+
		`'Tags': <<'jazz', 'live'>>, 'Info': {'Year': 1965}}`)
	statement(t, c, `INSERT INTO music VALUE {'Artist': ?, 'Track': ?, 'Title': 'Feeling Good', 'Plays': 7}`,
		&types.AttributeValueMemberS{Value: "Nina"}, &types.AttributeValueMemberN{Value: "2"})
	statement(t, c, `INSERT INTO music VALUE {'Artist': 'Miles', 'Track': 1, 'Title': 'So What', 'Old': true}`)

	_, err := c.ExecuteStatement(ctx, &sdk.ExecuteStatementInput{
		Statement: aws.String(`INSERT INTO music VALUE {'Artist': 'Miles', 'Track': 1}`),
	})
	code(t, err, "DuplicateItemException")

	items := statement(t, c, `SELECT * FROM music WHERE Artist = 'Nina' ORDER BY Track DESC`)
	if len(items) != 2 || str(items[0], "Track") != "2" || str(items[1], "Track") != "1" {
		t.Fatalf("ordered: %+v", items)
	}

	items = statement(t, c, `SELECT Title, Info.Year FROM music WHERE Artist = ? AND Track BETWEEN 1 AND 2 `+
		`AND Plays > 8`, &types.AttributeValueMemberS{Value: "Nina"})
	if len(items) != 1 || str(items[0], "Title") != "Sinnerman" || items[0]["Plays"] != nil {
		t.Fatalf("projected: %+v", items)
	}

	items = statement(t, c, `SELECT Title FROM music WHERE contains(Tags, 'jazz') OR Old IS NOT MISSING`)
	if len(items) != 2 {
		t.Fatalf("scanned: %+v", items)
	}

	page, err := c.ExecuteStatement(ctx, &sdk.ExecuteStatementInput{
		Statement: aws.String(`SELECT * FROM music`), Limit: aws.Int32(2),
	})
	if err != nil || len(page.Items) != 2 || page.NextToken == nil {
		t.Fatalf("page: %+v %v", page, err)
	}

	page, err = c.ExecuteStatement(ctx, &sdk.ExecuteStatementInput{
		Statement: aws.String(`SELECT * FROM music`), Limit: aws.Int32(2), NextToken: page.NextToken,
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("second page: %+v %v", page, err)
	}

	items = statement(t, c, `UPDATE music SET Plays = Plays + 1 SET Tags = set_add(Tags, <<'soul'>>) `+
		`REMOVE Info WHERE Artist = 'Nina' AND Track = 1 RETURNING ALL NEW *`)
	if len(items) != 1 || str(items[0], "Plays") != "11" || items[0]["Info"] != nil ||
		len(items[0]["Tags"].(*types.AttributeValueMemberSS).Value) != 3 {
		t.Fatalf("updated: %+v", items)
	}

	_, err = c.ExecuteStatement(ctx, &sdk.ExecuteStatementInput{
		Statement: aws.String(`UPDATE music SET Plays = 1 WHERE Artist = 'Nobody' AND Track = 1`),
	})
	code(t, err, "ConditionalCheckFailedException")

	_, err = c.ExecuteStatement(ctx, &sdk.ExecuteStatementInput{
		Statement: aws.String(`UPDATE music SET Plays = 1 WHERE Artist = 'Nina'`),
	})
	code(t, err, "ValidationException")

	items = statement(t, c, `DELETE FROM music WHERE Artist = 'Nina' AND Track = 2 RETURNING ALL OLD *`)
	if len(items) != 1 || str(items[0], "Title") != "Feeling Good" {
		t.Fatalf("deleted: %+v", items)
	}

	_, err = c.ExecuteStatement(ctx, &sdk.ExecuteStatementInput{Statement: aws.String(`SELECT * FROM`)})
	code(t, err, "ValidationException")

	batch, err := c.BatchExecuteStatement(ctx, &sdk.BatchExecuteStatementInput{
		Statements: []types.BatchStatementRequest{
			{Statement: aws.String(`SELECT * FROM music WHERE Artist = 'Miles' AND Track = 1`)},
			{Statement: aws.String(`SELECT * FROM music WHERE Artist = 'Miles'`)},
		},
	})
	if err != nil || str(batch.Responses[0].Item, "Title") != "So What" || batch.Responses[1].Error == nil ||
		batch.Responses[1].Error.Code != types.BatchStatementErrorCodeEnumValidationError {
		t.Fatalf("batch reads: %+v %v", batch, err)
	}

	batch, err = c.BatchExecuteStatement(ctx, &sdk.BatchExecuteStatementInput{
		Statements: []types.BatchStatementRequest{
			{Statement: aws.String(`INSERT INTO music VALUE {'Artist': 'Ella', 'Track': 1}`)},
			{Statement: aws.String(`INSERT INTO music VALUE {'Artist': 'Miles', 'Track': 1}`)},
		},
	})
	if err != nil || batch.Responses[0].Error != nil ||
		batch.Responses[1].Error.Code != types.BatchStatementErrorCodeEnumDuplicateItem {
		t.Fatalf("batch writes: %+v %v", batch, err)
	}

	_, err = c.BatchExecuteStatement(ctx, &sdk.BatchExecuteStatementInput{
		Statements: []types.BatchStatementRequest{
			{Statement: aws.String(`SELECT * FROM music WHERE Artist = 'Ella' AND Track = 1`)},
			{Statement: aws.String(`DELETE FROM music WHERE Artist = 'Ella' AND Track = 1`)},
		},
	})
	code(t, err, "ValidationException")

	// The rejected batch deleted nothing.
	if items = statement(t, c, `SELECT * FROM music WHERE Artist = 'Ella'`); len(items) != 1 {
		t.Fatalf("after a mixed batch: %+v", items)
	}
}
