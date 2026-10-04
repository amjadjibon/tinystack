package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDynamoDBParallelScan(t *testing.T) {
	ctx := context.Background()
	c := client(server(t), "test", "us-east-1")
	if _, err := create(ctx, c, "segments"); err != nil {
		t.Fatal(err)
	}

	for i := range 50 {
		if _, err := c.PutItem(ctx, &sdk.PutItemInput{
			TableName: aws.String("segments"), Item: key(fmt.Sprintf("item-%02d", i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Every item is in exactly one segment, also when read a page at a time.
	seen := map[string]int{}
	for seg := range int32(4) {
		var start map[string]types.AttributeValue
		for {
			out, err := c.Scan(ctx, &sdk.ScanInput{
				TableName: aws.String("segments"), Segment: aws.Int32(seg), TotalSegments: aws.Int32(4),
				Limit: aws.Int32(5), ExclusiveStartKey: start,
			})
			if err != nil {
				t.Fatal(err)
			}

			for _, it := range out.Items {
				seen[it["PK"].(*types.AttributeValueMemberS).Value]++
			}

			if start = out.LastEvaluatedKey; start == nil {
				break
			}
		}
	}

	if len(seen) != 50 {
		t.Fatalf("saw %d items, want 50", len(seen))
	}

	for pk, n := range seen {
		if n != 1 {
			t.Fatalf("%s was in %d segments", pk, n)
		}
	}

	for _, in := range []*sdk.ScanInput{
		{Segment: aws.Int32(0)},
		{TotalSegments: aws.Int32(2)},
		{Segment: aws.Int32(2), TotalSegments: aws.Int32(2)},
		{Segment: aws.Int32(0), TotalSegments: aws.Int32(1000001)},
	} {
		in.TableName = aws.String("segments")
		_, err := c.Scan(ctx, in)
		code(t, err, "ValidationException")
	}
}
