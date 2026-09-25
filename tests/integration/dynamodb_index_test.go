package integration_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// indexedOrders creates orders keyed by customer and order number, with a
// sparse global index on status and date that projects only the total, and a
// local index on each customer's order dates.
func indexedOrders(t *testing.T, c *sdk.Client) {
	t.Helper()
	_, err := c.CreateTable(context.Background(), &sdk.CreateTableInput{
		TableName:   aws.String("orders"),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("customer"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("order"), AttributeType: types.ScalarAttributeTypeN},
			{AttributeName: aws.String("status"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("date"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("customer"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("order"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName: aws.String("by-status"),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("status"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("date"), KeyType: types.KeyTypeRange},
			},
			Projection: &types.Projection{
				ProjectionType:   types.ProjectionTypeInclude,
				NonKeyAttributes: []string{"total"},
			},
		}},
		LocalSecondaryIndexes: []types.LocalSecondaryIndex{{
			IndexName: aws.String("by-date"),
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String("customer"), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String("date"), KeyType: types.KeyTypeRange},
			},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeKeysOnly},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSecondaryIndexes(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	indexedOrders(t, c)

	d, err := c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("orders")})
	if err != nil {
		t.Fatal(err)
	}

	gsi := d.Table.GlobalSecondaryIndexes
	if len(gsi) != 1 || *gsi[0].IndexName != "by-status" || gsi[0].IndexStatus != types.IndexStatusActive ||
		*gsi[0].IndexArn != *d.Table.TableArn+"/index/by-status" || len(d.Table.LocalSecondaryIndexes) != 1 {
		t.Fatalf("indexes = %+v %+v", gsi, d.Table.LocalSecondaryIndexes)
	}

	for _, tc := range []struct{ customer, order, status, date string }{
		{"ada", "1", "open", "2026-01-03"},
		{"ada", "2", "shipped", "2026-01-01"},
		{"bob", "3", "open", "2026-01-02"},
		{"bob", "4", "", "2026-01-04"}, // no status: absent from the sparse global index
		{"cy", "5", "open", "2026-01-02"},
	} {
		it := map[string]types.AttributeValue{
			"customer": s(tc.customer), "order": n(tc.order), "date": s(tc.date),
			"total": n("10"), "note": s("hidden"),
		}
		if tc.status != "" {
			it["status"] = s(tc.status)
		}

		if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("orders"), Item: it}); err != nil {
			t.Fatal(err)
		}
	}

	// A global index query sorts by the index's sort key and returns only its
	// projection; pages follow each other through LastEvaluatedKey.
	var got []string
	var start map[string]types.AttributeValue
	for {
		out, queryErr := c.Query(ctx, &sdk.QueryInput{
			TableName:                 aws.String("orders"),
			IndexName:                 aws.String("by-status"),
			KeyConditionExpression:    aws.String("#s = :s"),
			ExpressionAttributeNames:  map[string]string{"#s": "status"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":s": s("open")},
			Limit:                     aws.Int32(2),
			ExclusiveStartKey:         start,
		})
		if queryErr != nil {
			t.Fatal(queryErr)
		}

		for _, it := range out.Items {
			if _, ok := it["note"]; ok || it["total"] == nil {
				t.Fatalf("projection = %v", it)
			}

			got = append(got, it["order"].(*types.AttributeValueMemberN).Value)
		}

		if out.LastEvaluatedKey == nil {
			break
		}

		if len(out.LastEvaluatedKey) != 4 {
			t.Fatalf("LastEvaluatedKey = %v", out.LastEvaluatedKey)
		}

		start = out.LastEvaluatedKey
	}

	// Orders 3 and 5 share a date, so their order is unspecified, but paging
	// through the tie returns each once.
	if len(got) != 3 || got[2] != "1" || !reflect.DeepEqual(slices.Sorted(slices.Values(got[:2])), []string{"3", "5"}) {
		t.Fatalf("by-status = %v", got)
	}

	// A local index reads one customer by date, descending.
	out, err := c.Query(ctx, &sdk.QueryInput{
		TableName:                 aws.String("orders"),
		IndexName:                 aws.String("by-date"),
		KeyConditionExpression:    aws.String("customer = :c AND #d > :d"),
		ExpressionAttributeNames:  map[string]string{"#d": "date"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": s("ada"), ":d": s("2026-01-00")},
		ScanIndexForward:          aws.Bool(false),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(orderNumbers(out.Items), []string{"1", "2"}) || len(out.Items[0]) != 3 {
		t.Fatalf("by-date = %v", out.Items)
	}

	// Scanning the sparse index skips the item without a status.
	scan, err := c.Scan(ctx, &sdk.ScanInput{TableName: aws.String("orders"), IndexName: aws.String("by-status")})
	if err != nil {
		t.Fatal(err)
	}

	if scan.Count != 4 {
		t.Fatalf("index scan count = %d", scan.Count)
	}

	// Updates are visible through the index at once.
	_, err = c.UpdateItem(ctx, &sdk.UpdateItemInput{
		TableName:                 aws.String("orders"),
		Key:                       map[string]types.AttributeValue{"customer": s("ada"), "order": n("1")},
		UpdateExpression:          aws.String("SET #s = :s"),
		ExpressionAttributeNames:  map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":s": s("shipped")},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err = c.Query(ctx, &sdk.QueryInput{
		TableName:                 aws.String("orders"),
		IndexName:                 aws.String("by-status"),
		KeyConditionExpression:    aws.String("#s = :s"),
		ExpressionAttributeNames:  map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":s": s("shipped")},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(orderNumbers(out.Items), []string{"2", "1"}) {
		t.Fatalf("shipped = %v", orderNumbers(out.Items))
	}

	// Index keys must have their defined type, and projections limit Select.
	_, err = c.PutItem(ctx, &sdk.PutItemInput{
		TableName: aws.String("orders"),
		Item:      map[string]types.AttributeValue{"customer": s("dee"), "order": n("6"), "status": n("1")},
	})
	code(t, err, "ValidationException")

	_, err = c.Query(ctx, &sdk.QueryInput{
		TableName:                 aws.String("orders"),
		IndexName:                 aws.String("by-status"),
		KeyConditionExpression:    aws.String("#s = :s"),
		ExpressionAttributeNames:  map[string]string{"#s": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":s": s("open")},
		Select:                    types.SelectAllAttributes,
	})
	code(t, err, "ValidationException")

	_, err = c.Query(ctx, &sdk.QueryInput{
		TableName:                 aws.String("orders"),
		IndexName:                 aws.String("missing"),
		KeyConditionExpression:    aws.String("customer = :c"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": s("ada")},
	})
	code(t, err, "ValidationException")
}

func TestSecondaryIndexValidation(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	hash := []types.KeySchemaElement{{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash}}
	for name, in := range map[string]*sdk.CreateTableInput{
		"unused definition": {
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("GSK"), AttributeType: types.ScalarAttributeTypeS},
			},
		},
		"undefined index key": {
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
			},
			GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
				IndexName:  aws.String("gsi"),
				KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("GSK"), KeyType: types.KeyTypeHash}},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			}},
		},
		"local index without a table sort key": {
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String("LSK"), AttributeType: types.ScalarAttributeTypeS},
			},
			LocalSecondaryIndexes: []types.LocalSecondaryIndex{{
				IndexName: aws.String("lsi"),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash},
					{AttributeName: aws.String("LSK"), KeyType: types.KeyTypeRange},
				},
				Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
			}},
		},
	} {
		in.TableName = aws.String("bad")
		in.BillingMode = types.BillingModePayPerRequest
		in.KeySchema = hash
		_, err := c.CreateTable(ctx, in)
		if err == nil {
			t.Fatalf("%s: created", name)
		}

		code(t, err, "ValidationException")
	}
}
