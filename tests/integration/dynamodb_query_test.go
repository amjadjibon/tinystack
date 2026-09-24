package integration_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// orders creates a table keyed by customer and a numeric order number.
func orders(t *testing.T, c *sdk.Client) {
	t.Helper()
	_, err := c.CreateTable(context.Background(), &sdk.CreateTableInput{
		TableName:   aws.String("orders"),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("customer"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("order"), AttributeType: types.ScalarAttributeTypeN},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("customer"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("order"), KeyType: types.KeyTypeRange},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v string) types.AttributeValue { return &types.AttributeValueMemberN{Value: v} }

func orderNumbers(items []map[string]types.AttributeValue) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it["order"].(*types.AttributeValueMemberN).Value)
	}

	return out
}

func TestQuery(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	orders(t, c)
	// Numeric sort keys order by value, not text: 9 comes before 10.
	for _, tc := range []struct{ customer, order, status string }{
		{"ada", "10", "shipped"},
		{"ada", "9", "open"},
		{"ada", "100", "open"},
		{"ada", "2.5", "shipped"},
		{"bob", "1", "open"},
	} {
		_, err := c.PutItem(
			ctx,
			&sdk.PutItemInput{TableName: aws.String("orders"), Item: map[string]types.AttributeValue{
				"customer": s(tc.customer), "order": n(tc.order), "status": s(tc.status), "note": s("n" + tc.order),
			}},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	query := func(in *sdk.QueryInput) *sdk.QueryOutput {
		t.Helper()
		in.TableName = aws.String("orders")
		out, err := c.Query(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		return out
	}

	all := query(&sdk.QueryInput{
		KeyConditionExpression:    aws.String("customer = :c"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": s("ada")},
	})
	if got := orderNumbers(all.Items); !reflect.DeepEqual(got, []string{"2.5", "9", "10", "100"}) || all.Count != 4 {
		t.Fatalf("partition order: %v", got)
	}

	for _, tc := range []struct {
		values  map[string]types.AttributeValue
		name    string
		cond    string
		want    []string
		forward bool
	}{
		{
			name: "range", cond: "customer = :c AND #o BETWEEN :lo AND :hi", forward: true, want: []string{"9", "10"},
			values: map[string]types.AttributeValue{":c": s("ada"), ":lo": n("5"), ":hi": n("10")},
		},
		{
			name: "reverse", cond: "customer = :c AND #o >= :lo", want: []string{"100", "10", "9"},
			values: map[string]types.AttributeValue{":c": s("ada"), ":lo": n("9")},
		},
		{
			name: "other partition", cond: "(customer = :c) AND #o < :hi", forward: true, want: []string{"1"},
			values: map[string]types.AttributeValue{":c": s("bob"), ":hi": n("5")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := query(&sdk.QueryInput{
				KeyConditionExpression:    &tc.cond,
				ExpressionAttributeNames:  map[string]string{"#o": "order"},
				ExpressionAttributeValues: tc.values,
				ScanIndexForward:          &tc.forward,
			})
			if got := orderNumbers(out.Items); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	// Limit counts items read before the filter, so pages can come back short.
	pages := sdk.NewQueryPaginator(c, &sdk.QueryInput{
		TableName:                 aws.String("orders"),
		KeyConditionExpression:    aws.String("customer = :c"),
		FilterExpression:          aws.String("#s = :open"),
		ProjectionExpression:      aws.String("#o, note"),
		ExpressionAttributeNames:  map[string]string{"#s": "status", "#o": "order"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": s("ada"), ":open": s("open")},
		Limit:                     aws.Int32(2),
	})
	var open []map[string]types.AttributeValue
	var scanned, count int32
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}

		open = append(open, page.Items...)
		scanned += page.ScannedCount
		count += page.Count
	}

	want := []map[string]types.AttributeValue{
		{"order": n("9"), "note": s("n9")},
		{"order": n("100"), "note": s("n100")},
	}

	if !reflect.DeepEqual(open, want) || scanned != 4 || count != 2 {
		t.Fatalf("filtered pages: %v scanned=%d count=%d", open, scanned, count)
	}

	counted := query(&sdk.QueryInput{
		KeyConditionExpression:    aws.String("customer = :c"),
		FilterExpression:          aws.String("begins_with(note, :p)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":c": s("ada"), ":p": s("n1")},
		Select:                    types.SelectCount,
	})
	if counted.Count != 2 || counted.ScannedCount != 4 || len(counted.Items) != 0 {
		t.Fatalf("count: %d of %d, %v", counted.Count, counted.ScannedCount, counted.Items)
	}
}

func TestQueryValidation(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	orders(t, c)
	for _, tc := range []struct {
		values map[string]types.AttributeValue
		cond   string
	}{
		{cond: "#o = :n", values: map[string]types.AttributeValue{":n": n("1")}},
		{cond: "customer = :c OR #o = :n", values: map[string]types.AttributeValue{":c": s("a"), ":n": n("1")}},
		{cond: "customer <> :c", values: map[string]types.AttributeValue{":c": s("a")}},
		{cond: "customer = :n", values: map[string]types.AttributeValue{":n": n("1")}},
		{cond: "customer = :c AND begins_with(#o, :n)", values: map[string]types.AttributeValue{":c": s("a"), ":n": n("1")}},
		{cond: "customer = :c AND note = :c", values: map[string]types.AttributeValue{":c": s("a")}},
	} {
		t.Run(tc.cond, func(t *testing.T) {
			in := &sdk.QueryInput{
				TableName:                 aws.String("orders"),
				KeyConditionExpression:    &tc.cond,
				ExpressionAttributeValues: tc.values,
			}

			if strings.Contains(tc.cond, "#o") {
				in.ExpressionAttributeNames = map[string]string{"#o": "order"}
			}

			_, err := c.Query(ctx, in)
			code(t, err, "ValidationException")
		})
	}
}

func TestUpdateItem(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	if _, err := create(ctx, c, "users"); err != nil {
		t.Fatal(err)
	}

	type attrs = map[string]types.AttributeValue
	update := func(expr string, values attrs, ret types.ReturnValue) attrs {
		t.Helper()
		out, err := c.UpdateItem(ctx, &sdk.UpdateItemInput{
			TableName:                 aws.String("users"),
			Key:                       key("ada"),
			UpdateExpression:          &expr,
			ExpressionAttributeNames:  map[string]string{"#n": "name"},
			ExpressionAttributeValues: values,
			ReturnValues:              ret,
		})
		if err != nil {
			t.Fatal(err)
		}

		return out.Attributes
	}

	// UpdateItem creates the item from its key.
	got := update("SET #n = :n, visits = :one, tags = :tags", map[string]types.AttributeValue{
		":n": s("Ada"), ":one": n("1"), ":tags": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
	}, types.ReturnValueAllNew)
	want := map[string]types.AttributeValue{
		"PK": s("ada"), "name": s("Ada"), "visits": n("1"),
		"tags": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ALL_NEW: %v", got)
	}

	got = update("ADD visits :one DELETE tags :a REMOVE #n", map[string]types.AttributeValue{
		":one": n("1"), ":a": &types.AttributeValueMemberSS{Value: []string{"a"}},
	}, types.ReturnValueUpdatedNew)
	want = map[string]types.AttributeValue{
		"visits": n("2"),
		"tags":   &types.AttributeValueMemberSS{Value: []string{"b"}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UPDATED_NEW: %v", got)
	}

	for _, tc := range []struct {
		expr, cond, code string
	}{
		{"SET visits = visits * :one", "", "ValidationException"},
		{"SET PK = :one", "", "ValidationException"},
		{"SET visits = :one", "visits > :one", "ConditionalCheckFailedException"},
	} {
		in := &sdk.UpdateItemInput{
			TableName: aws.String("users"), Key: key("ada"), UpdateExpression: &tc.expr,
			ExpressionAttributeValues: map[string]types.AttributeValue{":one": n("5")},
		}

		if tc.cond != "" {
			in.ConditionExpression = &tc.cond
		}

		_, err := c.UpdateItem(ctx, in)
		code(t, err, tc.code)
	}

	item, err := c.GetItem(ctx, &sdk.GetItemInput{TableName: aws.String("users"), Key: key("ada")})
	if err != nil || !reflect.DeepEqual(item.Item["visits"], n("2")) {
		t.Fatalf("rejected updates changed the item: %v %v", item, err)
	}
}

func TestConditionalWrites(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	if _, err := create(ctx, c, "users"); err != nil {
		t.Fatal(err)
	}

	put := func(version string, cond *string, values map[string]types.AttributeValue) error {
		item := key("ada")
		item["version"] = n(version)
		_, err := c.PutItem(ctx, &sdk.PutItemInput{
			TableName: aws.String("users"), Item: item, ConditionExpression: cond, ExpressionAttributeValues: values,
		})
		return err
	}

	notExists := aws.String("attribute_not_exists(PK)")
	if err := put("1", notExists, nil); err != nil {
		t.Fatal(err)
	}

	code(t, put("1", notExists, nil), "ConditionalCheckFailedException")
	code(t, put("2", aws.String("version = :v"), map[string]types.AttributeValue{":v": n("5")}),
		"ConditionalCheckFailedException")
	if err := put("2", aws.String("version = :v"), map[string]types.AttributeValue{":v": n("1")}); err != nil {
		t.Fatal(err)
	}

	_, err := c.DeleteItem(ctx, &sdk.DeleteItemInput{
		TableName: aws.String("users"), Key: key("ada"), ConditionExpression: aws.String("version < :v"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": n("2")},
	})
	code(t, err, "ConditionalCheckFailedException")

	// Optimistic locking: of many concurrent updates from the same version,
	// exactly one wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range 20 {
		wg.Go(func() {
			_, updateErr := c.UpdateItem(ctx, &sdk.UpdateItemInput{
				TableName: aws.String("users"), Key: key("ada"),
				UpdateExpression:    aws.String("SET version = version + :one, writer = :w"),
				ConditionExpression: aws.String("version = :expected"),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":one": n("1"), ":expected": n("2"), ":w": n(fmt.Sprint(i)),
				},
			})
			if updateErr == nil {
				mu.Lock()
				won++
				mu.Unlock()
				return
			}

			code(t, updateErr, "ConditionalCheckFailedException")
		})
	}

	wg.Wait()
	if won != 1 {
		t.Fatalf("%d conditional updates won", won)
	}

	got, err := c.GetItem(ctx, &sdk.GetItemInput{
		TableName: aws.String("users"), Key: key("ada"), ProjectionExpression: aws.String("version"),
	})
	if err != nil || !reflect.DeepEqual(got.Item, map[string]types.AttributeValue{"version": n("3")}) {
		t.Fatalf("after locking: %v %v", got, err)
	}

	desc, err := c.DescribeTable(ctx, &sdk.DescribeTableInput{TableName: aws.String("users")})
	if err != nil || *desc.Table.ItemCount != 1 || *desc.Table.TableSizeBytes == 0 {
		t.Fatalf("table stats: %v %v", desc, err)
	}
}
