package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	sdk "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	sdktypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestConsoleDynamoDBItemsAndIndexes(t *testing.T) {
	srv := server(t)
	const account, region, name = "000000000000", "eu-west-1", "console-table"
	write := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, "dynamodb", operation,
			account, region, input), http.StatusOK)["output"].(map[string]any)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		input["TableName"] = name
		return write(operation, input)
	}
	var caps map[string]any
	if internal(t, srv, http.MethodGet, consoleActionsPath, &caps) != http.StatusOK || !consolePage(caps, "dynamodb") {
		t.Fatalf("capabilities = %+v", caps)
	}
	created := call("CreateTable", map[string]any{
		"AttributeDefinitions": []any{
			map[string]any{"AttributeName": "pk", "AttributeType": "S"},
			map[string]any{"AttributeName": "sk", "AttributeType": "N"},
			map[string]any{"AttributeName": "team", "AttributeType": "S"},
			map[string]any{"AttributeName": "rank", "AttributeType": "N"},
		},
		"KeySchema": []any{
			map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
			map[string]any{"AttributeName": "sk", "KeyType": "RANGE"},
		},
		"BillingMode": "PAY_PER_REQUEST", "Tags": []any{map[string]any{"Key": "team", "Value": "console"}},
		"GlobalSecondaryIndexes": []any{map[string]any{
			"IndexName": "by-team", "KeySchema": []any{map[string]any{"AttributeName": "team", "KeyType": "HASH"}},
			"Projection": map[string]any{"ProjectionType": "KEYS_ONLY"},
		}},
		"LocalSecondaryIndexes": []any{map[string]any{
			"IndexName": "by-rank", "KeySchema": []any{
				map[string]any{"AttributeName": "pk", "KeyType": "HASH"},
				map[string]any{"AttributeName": "rank", "KeyType": "RANGE"},
			},
			"Projection": map[string]any{"ProjectionType": "ALL"},
		}},
	})["TableDescription"].(map[string]any)
	arn := created["TableArn"].(string)
	item := map[string]any{
		"pk": map[string]any{"S": "user#世界"}, "sk": map[string]any{"N": "1"},
		"team": map[string]any{"S": "red"}, "rank": map[string]any{"N": "2"},
		"precise": map[string]any{"N": "900719925474099312345"}, "binary": map[string]any{"B": "AP+A"},
		"boolean": map[string]any{"BOOL": false}, "nothing": map[string]any{"NULL": true},
		"strings": map[string]any{"SS": []any{"a", "b"}}, "numbers": map[string]any{"NS": []any{"1", "2"}},
		"binaries": map[string]any{"BS": []any{"AP+A", "aGVsbG8="}},
		"nested": map[string]any{"M": map[string]any{"list": map[string]any{"L": []any{
			map[string]any{"S": "nested"}, map[string]any{"BOOL": false},
		}}}}, "note": map[string]any{"S": "before"},
	}
	call("PutItem", map[string]any{
		"Item": item, "ConditionExpression": "attribute_not_exists(#pk)",
		"ExpressionAttributeNames": map[string]any{"#pk": "pk"},
	})
	duplicate := consoleResponse(t, srv, consoleLambdaRequest(t, srv, "dynamodb", "PutItem", account, region,
		map[string]any{
			"TableName": name, "Item": item, "ConditionExpression": "attribute_not_exists(#pk)",
			"ExpressionAttributeNames": map[string]any{"#pk": "pk"},
		}), http.StatusBadRequest)
	if duplicate["code"] != "ConditionalCheckFailedException" {
		t.Fatal(duplicate)
	}
	key := map[string]any{"pk": item["pk"], "sk": item["sk"]}
	got := call("GetItem", map[string]any{"Key": key, "ConsistentRead": true})["Item"].(map[string]any)
	if got["binary"].(map[string]any)["B"] != "AP+A" ||
		got["precise"].(map[string]any)["N"] != "900719925474099312345" ||
		got["boolean"].(map[string]any)["BOOL"] != false {
		t.Fatalf("item = %+v", got)
	}
	for _, number := range []string{"2", "3"} {
		call("PutItem", map[string]any{"Item": map[string]any{
			"pk": item["pk"], "sk": map[string]any{"N": number},
			"team": map[string]any{"S": "red"}, "rank": map[string]any{"N": number},
		}})
	}
	filtered := call("Scan", map[string]any{
		"Limit": 1, "FilterExpression": "#team = :t",
		"ExpressionAttributeNames":  map[string]any{"#team": "team"},
		"ExpressionAttributeValues": map[string]any{":t": map[string]any{"S": "absent"}},
	})
	if filtered["Count"] != float64(0) || filtered["ScannedCount"] != float64(1) ||
		len(filtered["LastEvaluatedKey"].(map[string]any)) == 0 {
		t.Fatalf("empty filtered page = %+v", filtered)
	}
	query := map[string]any{
		"KeyConditionExpression": "#pk = :pk", "Limit": 1, "ScanIndexForward": true,
		"ExpressionAttributeNames":  map[string]any{"#pk": "pk"},
		"ExpressionAttributeValues": map[string]any{":pk": item["pk"]},
	}
	first := call("Query", query)
	query["ExclusiveStartKey"] = first["LastEvaluatedKey"]
	second := call("Query", query)
	if first["Items"].([]any)[0].(map[string]any)["sk"].(map[string]any)["N"] != "1" ||
		second["Items"].([]any)[0].(map[string]any)["sk"].(map[string]any)["N"] != "2" {
		t.Fatalf("query pages = %+v, %+v", first, second)
	}
	projected := call("Query", map[string]any{
		"IndexName": "by-team", "KeyConditionExpression": "#team = :t",
		"ExpressionAttributeNames":  map[string]any{"#team": "team"},
		"ExpressionAttributeValues": map[string]any{":t": map[string]any{"S": "red"}},
	})
	if len(projected["Items"].([]any)) != 3 || projected["Items"].([]any)[0].(map[string]any)["note"] != nil {
		t.Fatal(projected)
	}
	// The item editor fetches the full base-table item before editing an index result.
	call("UpdateItem", map[string]any{
		"Key": key, "UpdateExpression": "SET #other = :other",
		"ExpressionAttributeNames":  map[string]any{"#other": "concurrent"},
		"ExpressionAttributeValues": map[string]any{":other": map[string]any{"S": "preserve me"}},
	})
	edit := map[string]any{
		"Key": key, "UpdateExpression": "SET #note = :new REMOVE #nothing",
		"ConditionExpression":      "attribute_exists(#pk) AND #note = :old AND #nothing = :nothing",
		"ExpressionAttributeNames": map[string]any{"#pk": "pk", "#note": "note", "#nothing": "nothing"},
		"ExpressionAttributeValues": map[string]any{
			":new": map[string]any{"S": "after"},
			":old": map[string]any{"S": "before"}, ":nothing": map[string]any{"NULL": true},
		},
	}
	call("UpdateItem", edit)
	conflict := consoleResponse(t, srv, consoleLambdaRequest(t, srv, "dynamodb", "UpdateItem", account, region, edit),
		http.StatusBadRequest)
	if conflict["code"] != "ConditionalCheckFailedException" {
		t.Fatal(conflict)
	}
	read, err := client(srv, "test", region).GetItem(context.Background(), &sdk.GetItemInput{
		TableName: new(name), Key: map[string]sdktypes.AttributeValue{"pk": s("user#世界"), "sk": n("1")},
	})
	if err != nil || read.Item["nothing"] != nil || read.Item["concurrent"] == nil || read.Item["binary"] == nil {
		t.Fatalf("SDK item = %+v, %v", read, err)
	}
	for _, scope := range []struct{ account, region string }{{"111111111111", region}, {account, "us-east-1"}} {
		consoleResponse(t, srv, consoleLambdaRequest(t, srv, "dynamodb", "DescribeTable", scope.account, scope.region,
			map[string]any{"TableName": name}), http.StatusBadRequest)
	}
	write(
		"TagResource",
		map[string]any{"ResourceArn": arn, "Tags": []any{map[string]any{"Key": "env", "Value": "test"}}},
	)
	write("UntagResource", map[string]any{"ResourceArn": arn, "TagKeys": []any{"team"}})
	tags := write("ListTagsOfResource", map[string]any{"ResourceArn": arn})["Tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["Key"] != "env" {
		t.Fatal(tags)
	}
	call("UpdateTable", map[string]any{"BillingMode": "PROVISIONED", "ProvisionedThroughput": map[string]any{
		"ReadCapacityUnits": 5, "WriteCapacityUnits": 3,
	}, "GlobalSecondaryIndexUpdates": []any{map[string]any{"Update": map[string]any{
		"IndexName":             "by-team",
		"ProvisionedThroughput": map[string]any{"ReadCapacityUnits": 2, "WriteCapacityUnits": 1},
	}}}})
	call("UpdateTable", map[string]any{"GlobalSecondaryIndexUpdates": []any{map[string]any{
		"Delete": map[string]any{"IndexName": "by-team"},
	}}})
	call("UpdateTable", map[string]any{"BillingMode": "PAY_PER_REQUEST"})
	call("UpdateTable", map[string]any{"AttributeDefinitions": []any{map[string]any{
		"AttributeName": "team", "AttributeType": "S",
	}}, "GlobalSecondaryIndexUpdates": []any{map[string]any{"Create": map[string]any{
		"IndexName": "team-all", "KeySchema": []any{map[string]any{"AttributeName": "team", "KeyType": "HASH"}},
		"Projection": map[string]any{"ProjectionType": "ALL"},
	}}}})
	call("DeleteItem", map[string]any{
		"Key": key, "ConditionExpression": "attribute_exists(#pk)",
		"ExpressionAttributeNames": map[string]any{"#pk": "pk"},
	})
	call("DeleteTable", map[string]any{})
}

func TestConsoleDynamoDBTTLAndLambdaStream(t *testing.T) {
	runtime := &fakeRuntime{calls: make(chan invocation, 10)}
	srv := serverWith(t, runtime)
	write := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation,
			"000000000000", "us-east-1", input), http.StatusOK)["output"].(map[string]any)
	}
	dynamo := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		input["TableName"] = "stream-console"
		return write("dynamodb", operation, input)
	}
	created := dynamo("CreateTable", map[string]any{
		"KeySchema":            []any{map[string]any{"AttributeName": "id", "KeyType": "HASH"}},
		"AttributeDefinitions": []any{map[string]any{"AttributeName": "id", "AttributeType": "S"}},
		"BillingMode":          "PAY_PER_REQUEST", "StreamSpecification": map[string]any{
			"StreamEnabled": true, "StreamViewType": "NEW_AND_OLD_IMAGES",
		},
	})["TableDescription"].(map[string]any)
	stream := created["LatestStreamArn"].(string)
	fn := write("lambda", "CreateFunction", map[string]any{
		"FunctionName": "dynamo-console", "Runtime": "python3.12", "Handler": "app.echo",
		"Role": "arn:aws:iam::000000000000:role/lambda", "Code": map[string]any{
			"ZipFile": base64.StdEncoding.EncodeToString(zipOf(t, "app.py", "def echo(e,c): return e")),
		},
	})["FunctionArn"].(string)
	mp := write("lambda", "CreateEventSourceMapping", map[string]any{
		"FunctionName": fn, "EventSourceArn": stream, "StartingPosition": "LATEST", "Enabled": true,
		"BatchSize": 100, "MaximumRetryAttempts": 2, "FunctionResponseTypes": []any{"ReportBatchItemFailures"},
	})
	dynamo("PutItem", map[string]any{"Item": map[string]any{"id": map[string]any{"S": "triggered"}}})
	select {
	case call := <-runtime.calls:
		if call.fn.ARN != fn || !bytes.Contains(call.payload, []byte("triggered")) ||
			!bytes.Contains(call.payload, []byte(stream)) {
			t.Fatalf("stream invocation = %+v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DynamoDB stream did not invoke Lambda")
	}
	write("lambda", "UpdateEventSourceMapping", map[string]any{"UUID": mp["UUID"], "Enabled": false})
	write("lambda", "DeleteEventSourceMapping", map[string]any{"UUID": mp["UUID"]})
	dynamo("UpdateTimeToLive", map[string]any{"TimeToLiveSpecification": map[string]any{
		"Enabled": true, "AttributeName": "expiresAt",
	}})
	ttl := dynamo("DescribeTimeToLive", map[string]any{})["TimeToLiveDescription"].(map[string]any)
	if ttl["AttributeName"] != "expiresAt" {
		t.Fatal("missing TTL setting")
	}
	dynamo("PutItem", map[string]any{"Item": map[string]any{
		"id":        map[string]any{"S": "expired"},
		"expiresAt": map[string]any{"N": strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)},
	}})
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := dynamo("GetItem", map[string]any{"Key": map[string]any{"id": map[string]any{"S": "expired"}}})
		if got["Item"] == nil || len(got["Item"].(map[string]any)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TTL did not remove expired item")
		}
		time.Sleep(25 * time.Millisecond)
	}
	dynamo(
		"UpdateTimeToLive",
		map[string]any{"TimeToLiveSpecification": map[string]any{"Enabled": false, "AttributeName": "expiresAt"}},
	)
	dynamo(
		"UpdateTimeToLive",
		map[string]any{"TimeToLiveSpecification": map[string]any{"Enabled": true, "AttributeName": "expiry"}},
	)
	dynamo("UpdateTable", map[string]any{"StreamSpecification": map[string]any{"StreamEnabled": false}})
	next := dynamo("UpdateTable", map[string]any{"StreamSpecification": map[string]any{
		"StreamEnabled": true, "StreamViewType": "NEW_IMAGE",
	}})["TableDescription"].(map[string]any)
	if next["LatestStreamArn"] == stream || next["LatestStreamArn"] == nil {
		t.Fatal("stream ARN did not change")
	}
	dynamo("DeleteTable", map[string]any{})
	write("lambda", "DeleteFunction", map[string]any{"FunctionName": "dynamo-console"})
}

func TestConsoleDynamoDBValidationAndSignatures(t *testing.T) {
	srv := server(t)
	for _, tc := range []struct {
		input             map[string]any
		operation, region string
	}{
		{map[string]any{}, "ListTables", ""},
		{map[string]any{}, "ListTables", "../us-east-1"},
		{map[string]any{"Unknown": true}, "ListTables", "us-east-1"},
		{map[string]any{}, "ExecuteStatement", "us-east-1"},
		{map[string]any{"TableName": "not-here", "Item": map[string]any{
			"id": map[string]any{"B": "not base64"},
		}}, "PutItem", "us-east-1"},
	} {
		consoleResponse(t, srv, consoleLambdaRequest(t, srv, "dynamodb", tc.operation,
			"000000000000", tc.region, tc.input), http.StatusBadRequest)
	}
	r := consoleLambdaRequest(t, srv, "dynamodb", "ListTables", "000000000000", "us-east-1", map[string]any{})
	r.Header.Set("Origin", "https://evil.example")
	consoleResponse(t, srv, r, http.StatusForbidden)
	verified := verifyingServer(t)
	for _, tc := range []struct {
		service, region, account string
		signed                   bool
		status                   int
	}{
		{"dynamodb", "us-east-1", "111111111111", false, http.StatusForbidden},
		{"lambda", "us-east-1", "111111111111", true, http.StatusForbidden},
		{"dynamodb", "eu-west-1", "111111111111", true, http.StatusForbidden},
		{"dynamodb", "us-east-1", "000000000000", true, http.StatusForbidden},
		{"dynamodb", "us-east-1", "111111111111", true, http.StatusOK},
	} {
		r := consoleLambdaRequest(t, verified, "dynamodb", "ListTables", tc.account, "us-east-1", map[string]any{})
		if tc.signed {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			sum := sha256.Sum256(body)
			err = v4.NewSigner().
				SignHTTP(context.Background(), aws.Credentials{AccessKeyID: "team-b", SecretAccessKey: "test"},
					r, hex.EncodeToString(sum[:]), tc.service, tc.region, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		}
		consoleResponse(t, verified, r, tc.status)
	}
}

func TestConsoleDynamoDBBackupsExportsAndPartiQL(t *testing.T) {
	srv := server(t)
	const account, region, name = "000000000000", "us-east-1", "console-recovery"
	write := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv, consoleLambdaRequest(t, srv, service, operation,
			account, region, input), http.StatusOK)["output"].(map[string]any)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return write("dynamodb", operation, input)
	}
	created := call("CreateTable", map[string]any{
		"TableName": name, "BillingMode": "PAY_PER_REQUEST",
		"KeySchema":            []any{map[string]any{"AttributeName": "pk", "KeyType": "HASH"}},
		"AttributeDefinitions": []any{map[string]any{"AttributeName": "pk", "AttributeType": "S"}},
	})["TableDescription"].(map[string]any)
	for _, id := range []string{"one", "two"} {
		call("ExecuteStatement", map[string]any{
			"Statement":  "INSERT INTO \"console-recovery\" VALUE {'pk': ?}",
			"Parameters": []any{map[string]any{"S": id}},
		})
	}
	statement := "SELECT * FROM \"console-recovery\""
	first := call("ExecuteStatement", map[string]any{"Statement": statement, "Limit": 1})
	second := call("ExecuteStatement", map[string]any{
		"Statement": statement, "Limit": 1, "NextToken": first["NextToken"],
	})
	if len(first["Items"].([]any)) != 1 || len(second["Items"].([]any)) != 1 {
		t.Fatalf("statement pages = %+v, %+v", first, second)
	}
	backup := call("CreateBackup", map[string]any{"TableName": name, "BackupName": "console-backup"})
	backupARN := backup["BackupDetails"].(map[string]any)["BackupArn"]
	if len(call("ListBackups", map[string]any{"TableName": name})["BackupSummaries"].([]any)) != 1 {
		t.Fatal("backup not listed")
	}
	call("DescribeBackup", map[string]any{"BackupArn": backupARN})
	call("RestoreTableFromBackup", map[string]any{"BackupArn": backupARN, "TargetTableName": "backup-restored"})
	call("UpdateContinuousBackups", map[string]any{
		"TableName": name, "PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": true},
	})
	pitr := call("DescribeContinuousBackups", map[string]any{"TableName": name})
	recovery := pitr["ContinuousBackupsDescription"].(map[string]any)["PointInTimeRecoveryDescription"].(map[string]any)
	if recovery["PointInTimeRecoveryStatus"] != "ENABLED" {
		t.Fatal(pitr)
	}
	call("RestoreTableToPointInTime", map[string]any{
		"SourceTableName": name, "TargetTableName": "pitr-restored", "UseLatestRestorableTime": true,
	})
	for _, restored := range []string{"backup-restored", "pitr-restored"} {
		if len(call("Scan", map[string]any{"TableName": restored})["Items"].([]any)) != 2 {
			t.Fatalf("restored items missing from %s", restored)
		}
	}
	write("s3", "CreateBucket", map[string]any{"Bucket": "console-dynamo-export"})
	exported := call("ExportTableToPointInTime", map[string]any{
		"TableArn": created["TableArn"], "S3Bucket": "console-dynamo-export", "S3Prefix": "snapshots",
		"ExportFormat": "DYNAMODB_JSON",
	})["ExportDescription"].(map[string]any)
	if exported["ExportStatus"] != "COMPLETED" || exported["ItemCount"] != float64(2) {
		t.Fatal(exported)
	}
	call("DescribeExport", map[string]any{"ExportArn": exported["ExportArn"]})
	if len(call("ListExports", map[string]any{"TableArn": created["TableArn"]})["ExportSummaries"].([]any)) != 1 {
		t.Fatal("export not listed")
	}
	stream := feed(t, kinesisClient(srv), "console-dynamo-destination")
	call("EnableKinesisStreamingDestination", map[string]any{
		"TableName": name, "StreamArn": stream,
		"EnableKinesisStreamingConfiguration": map[string]any{"ApproximateCreationDateTimePrecision": "MILLISECOND"},
	})
	call("UpdateKinesisStreamingDestination", map[string]any{
		"TableName": name, "StreamArn": stream,
		"UpdateKinesisStreamingConfiguration": map[string]any{"ApproximateCreationDateTimePrecision": "MICROSECOND"},
	})
	destinations := call("DescribeKinesisStreamingDestination", map[string]any{"TableName": name})
	destination := destinations["KinesisDataStreamDestinations"].([]any)[0].(map[string]any)
	if destination["DestinationStatus"] != "ACTIVE" ||
		destination["ApproximateCreationDateTimePrecision"] != "MICROSECOND" {
		t.Fatal(destinations)
	}
	call("DisableKinesisStreamingDestination", map[string]any{"TableName": name, "StreamArn": stream})
	call("DeleteBackup", map[string]any{"BackupArn": backupARN})
	call("UpdateContinuousBackups", map[string]any{
		"TableName": name, "PointInTimeRecoverySpecification": map[string]any{"PointInTimeRecoveryEnabled": false},
	})
	call("BatchWriteItem", map[string]any{"RequestItems": map[string]any{name: []any{
		map[string]any{"DeleteRequest": map[string]any{"Key": map[string]any{"pk": map[string]any{"S": "one"}}}},
	}}})
	rejected := consoleResponse(t, srv, consoleLambdaRequest(t, srv, "dynamodb", "ExecuteTransaction",
		account, region, map[string]any{}), http.StatusBadRequest)
	if rejected["code"] != "UnknownOperationException" {
		t.Fatal(rejected)
	}
}
