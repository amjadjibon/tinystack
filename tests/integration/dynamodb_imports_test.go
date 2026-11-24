package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"
)

func TestDynamoDBImports(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	objects := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	bucket := "import-source"
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}

	var zipped bytes.Buffer
	gz := gzip.NewWriter(&zipped)
	if _, err := gz.Write([]byte("one|123|hello\ntwo|456|world\n")); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	var compressed bytes.Buffer
	zw, zipErr := zstd.NewWriter(&compressed)
	if zipErr != nil {
		t.Fatal(zipErr)
	}

	if _, err := zw.Write([]byte("{\"Item\":{\"PK\":{\"S\":\"one\"},\"Row\":{\"N\":\"123\"}}}\n" +
		"{\"Item\":{\"Row\":{\"N\":\"456\"}}}\n")); err != nil {
		t.Fatal(err)
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		status types.ImportStatus
		input  sdk.ImportTableInput
		data   []byte
		count  int64
	}{
		{
			name: "import-csv", data: zipped.Bytes(), status: types.ImportStatusCompleted, count: 2,
			input: sdk.ImportTableInput{
				InputFormat: types.InputFormatCsv, InputCompressionType: types.InputCompressionTypeGzip,
				InputFormatOptions: &types.InputFormatOptions{Csv: &types.CsvOptions{
					Delimiter: new("|"), HeaderList: []string{"PK", "Row", "Comment"},
				}},
			},
		},
		{
			name: "import-json", data: compressed.Bytes(), status: types.ImportStatusFailed, count: 1,
			input: sdk.ImportTableInput{
				InputFormat: types.InputFormatDynamodbJson, InputCompressionType: types.InputCompressionTypeZstd,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := objects.PutObject(ctx, &s3.PutObjectInput{
				Bucket: &bucket, Key: &tc.name, Body: bytes.NewReader(tc.data),
			}); err != nil {
				t.Fatal(err)
			}

			in := tc.input
			in.ClientToken = &tc.name
			in.S3BucketSource = &types.S3BucketSource{S3Bucket: &bucket, S3KeyPrefix: &tc.name}
			in.TableCreationParameters = &types.TableCreationParameters{
				TableName: &tc.name, BillingMode: types.BillingModePayPerRequest,
				AttributeDefinitions: []types.AttributeDefinition{
					{AttributeName: new("PK"), AttributeType: types.ScalarAttributeTypeS},
					{AttributeName: new("Row"), AttributeType: types.ScalarAttributeTypeN},
				},
				KeySchema: []types.KeySchemaElement{{AttributeName: new("PK"), KeyType: types.KeyTypeHash}},
				GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
					IndexName: new("row-index"), Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
					KeySchema: []types.KeySchemaElement{{AttributeName: new("Row"), KeyType: types.KeyTypeHash}},
				}},
			}
			started, err := c.ImportTable(ctx, &in)
			if err != nil {
				t.Fatal(err)
			}

			arn := started.ImportTableDescription.ImportArn
			waitFor(t, "import completion", func() bool {
				out, describeErr := c.DescribeImport(ctx, &sdk.DescribeImportInput{ImportArn: arn})
				if describeErr != nil {
					t.Fatal(describeErr)
				}

				d := out.ImportTableDescription
				if d.ImportStatus == types.ImportStatusInProgress {
					return false
				}

				if d.ImportStatus != tc.status || d.ProcessedItemCount != 2 || d.ImportedItemCount != tc.count ||
					d.ErrorCount != 2-tc.count || *d.ProcessedSizeBytes != int64(len(tc.data)) {
					t.Fatalf("import status or counts: %+v", d)
				}

				return true
			})

			replay, err := c.ImportTable(ctx, &in)
			if err != nil || *replay.ImportTableDescription.ImportArn != *arn {
				t.Fatalf("import replay: %+v %v", replay, err)
			}

			out, err := c.Query(ctx, &sdk.QueryInput{
				TableName:                &tc.name,
				IndexName:                new("row-index"),
				KeyConditionExpression:   new("#r = :r"),
				ExpressionAttributeNames: map[string]string{"#r": "Row"},
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":r": &types.AttributeValueMemberN{Value: "123"},
				},
			})
			if err != nil || out.Count != 1 {
				t.Fatalf("imported index query: %+v %v", out, err)
			}
		})
	}

	page, err := c.ListImports(ctx, &sdk.ListImportsInput{PageSize: new(int32(1))})
	if err != nil || len(page.ImportSummaryList) != 1 || page.NextToken == nil {
		t.Fatalf("list imports first page: %+v %v", page, err)
	}

	last, err := c.ListImports(ctx, &sdk.ListImportsInput{NextToken: page.NextToken})
	if err != nil || len(last.ImportSummaryList) != 1 || last.NextToken != nil {
		t.Fatalf("list imports second page: %+v %v", last, err)
	}
}
