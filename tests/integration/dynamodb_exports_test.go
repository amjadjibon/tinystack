package integration_test

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestDynamoDBExports(t *testing.T) {
	s := server(t)
	c := client(s, "test", "us-east-1")
	objects := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := objects.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("exports")}); err != nil {
		t.Fatal(err)
	}

	created, err := create(ctx, c, "orders")
	if err != nil {
		t.Fatal(err)
	}

	tableARN := created.TableDescription.TableArn
	for _, id := range []string{"a", "b", "c"} {
		if _, err = c.PutItem(ctx, &sdk.PutItemInput{TableName: aws.String("orders"), Item: key(id)}); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.ExportTableToPointInTime(ctx, &sdk.ExportTableToPointInTimeInput{
		TableArn: tableARN, S3Bucket: aws.String("exports"),
	})
	code(t, err, "PointInTimeRecoveryUnavailableException")

	if _, err = c.UpdateContinuousBackups(ctx, &sdk.UpdateContinuousBackupsInput{
		TableName: aws.String("orders"),
		PointInTimeRecoverySpecification: &types.PointInTimeRecoverySpecification{
			PointInTimeRecoveryEnabled: aws.Bool(true),
		},
	}); err != nil {
		t.Fatal(err)
	}

	exported, err := c.ExportTableToPointInTime(ctx, &sdk.ExportTableToPointInTimeInput{
		ClientToken: new("export-once"), TableArn: tableARN, S3Bucket: aws.String("exports"), S3Prefix: aws.String("nightly"),
	})
	if err != nil || exported.ExportDescription.ExportStatus != types.ExportStatusCompleted {
		t.Fatalf("exported: %+v %v", exported, err)
	}

	repeated, replayErr := c.ExportTableToPointInTime(ctx, &sdk.ExportTableToPointInTimeInput{
		ClientToken: new("export-once"), TableArn: tableARN, S3Bucket: new("exports"), S3Prefix: new("nightly"),
	})
	if replayErr != nil || aws.ToString(repeated.ExportDescription.ExportArn) !=
		aws.ToString(exported.ExportDescription.ExportArn) {
		t.Fatalf("export replay: %+v %v", repeated, replayErr)
	}

	_, mismatchErr := c.ExportTableToPointInTime(ctx, &sdk.ExportTableToPointInTimeInput{
		ClientToken: new("export-once"), TableArn: tableARN, S3Bucket: new("exports"), S3Prefix: new("changed"),
	})
	code(t, mismatchErr, "ExportConflictException")

	described, err := c.DescribeExport(ctx, &sdk.DescribeExportInput{
		ExportArn: exported.ExportDescription.ExportArn,
	})
	if err != nil || aws.ToInt64(described.ExportDescription.ItemCount) != 3 ||
		!strings.HasPrefix(aws.ToString(described.ExportDescription.ExportManifest), "nightly/AWSDynamoDB/") {
		t.Fatalf("described: %+v %v", described, err)
	}

	files, err := objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("exports"),
		Key: aws.String(strings.Replace(
			aws.ToString(described.ExportDescription.ExportManifest), "manifest-summary", "manifest-files", 1,
		)),
	})
	if err != nil {
		t.Fatal(err)
	}

	var manifest struct {
		DataFileS3Key string `json:"dataFileS3Key"`
		ItemCount     int    `json:"itemCount"`
	}
	if err = json.NewDecoder(files.Body).Decode(&manifest); err != nil || manifest.ItemCount != 3 {
		t.Fatalf("manifest: %+v %v", manifest, err)
	}

	data, err := objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("exports"), Key: aws.String(manifest.DataFileS3Key),
	})
	if err != nil {
		t.Fatal(err)
	}

	unzipped, err := gzip.NewReader(data.Body)
	if err != nil {
		t.Fatal(err)
	}

	lines := 0
	for scanner := bufio.NewScanner(unzipped); scanner.Scan(); lines++ {
		var line struct {
			Item map[string]map[string]string
		}
		if err = json.Unmarshal(scanner.Bytes(), &line); err != nil || line.Item["PK"]["S"] == "" {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
	}

	if lines != 3 {
		t.Fatalf("%d lines", lines)
	}

	failed, err := c.ExportTableToPointInTime(ctx, &sdk.ExportTableToPointInTimeInput{
		TableArn: tableARN, S3Bucket: aws.String("missing"),
	})
	if err != nil || failed.ExportDescription.ExportStatus != types.ExportStatusFailed ||
		aws.ToString(failed.ExportDescription.FailureCode) == "" {
		t.Fatalf("failed: %+v %v", failed, err)
	}

	listed, err := c.ListExports(ctx, &sdk.ListExportsInput{TableArn: tableARN, MaxResults: aws.Int32(1)})
	if err != nil || len(listed.ExportSummaries) != 1 || listed.NextToken == nil {
		t.Fatalf("listed: %+v %v", listed, err)
	}

	listed, err = c.ListExports(ctx, &sdk.ListExportsInput{TableArn: tableARN, NextToken: listed.NextToken})
	if err != nil || len(listed.ExportSummaries) != 1 || listed.NextToken != nil {
		t.Fatalf("second page: %+v %v", listed, err)
	}
}
