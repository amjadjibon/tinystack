package integration_test

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestConsoleStreamingPages(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	call := func(service, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, operation, account, region, input),
			http.StatusOK)["output"].(map[string]any)
	}

	stream := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		if input == nil {
			input = map[string]any{}
		}

		input["StreamName"] = "console-kinesis"
		return call("kinesis", operation, input)
	}

	stream("CreateStream", map[string]any{
		"ShardCount": 1, "StreamModeDetails": map[string]any{"StreamMode": "PROVISIONED"},
	})
	description := stream("DescribeStream", nil)["StreamDescription"].(map[string]any)
	arn := description["StreamARN"].(string)
	shards := stream("ListShards", nil)["Shards"].([]any)
	shard := shards[0].(map[string]any)["ShardId"]
	latest := stream("GetShardIterator", map[string]any{
		"ShardId": shard, "ShardIteratorType": "LATEST",
	})["ShardIterator"]
	payload := base64.StdEncoding.EncodeToString([]byte("hello 世界"))
	stream("PutRecord", map[string]any{"Data": payload, "PartitionKey": "console"})
	iterator := stream("GetShardIterator", map[string]any{
		"ShardId": shard, "ShardIteratorType": "TRIM_HORIZON",
	})["ShardIterator"]
	records := call("kinesis", "GetRecords", map[string]any{"ShardIterator": iterator})["Records"].([]any)
	if len(records) != 1 || records[0].(map[string]any)["Data"] != payload {
		t.Fatal(records)
	}

	latestRecords := call("kinesis", "GetRecords", map[string]any{"ShardIterator": latest})["Records"].([]any)
	if len(latestRecords) != 1 {
		t.Fatal(latestRecords)
	}

	stream("UpdateShardCount", map[string]any{"TargetShardCount": 2, "ScalingType": "UNIFORM_SCALING"})
	stream("IncreaseStreamRetentionPeriod", map[string]any{"RetentionPeriodHours": 48})
	stream("DecreaseStreamRetentionPeriod", map[string]any{"RetentionPeriodHours": 24})
	stream("StartStreamEncryption", map[string]any{"EncryptionType": "KMS", "KeyId": "alias/aws/kinesis"})
	stream("StopStreamEncryption", map[string]any{"EncryptionType": "KMS", "KeyId": "alias/aws/kinesis"})
	stream("EnableEnhancedMonitoring", map[string]any{"ShardLevelMetrics": []string{"IncomingBytes"}})
	stream("DisableEnhancedMonitoring", map[string]any{"ShardLevelMetrics": []string{"IncomingBytes"}})
	stream("AddTagsToStream", map[string]any{"Tags": map[string]string{"team": "console"}})
	if len(stream("ListTagsForStream", nil)["Tags"].([]any)) != 1 {
		t.Fatal("missing stream tags")
	}

	stream("RemoveTagsFromStream", map[string]any{"TagKeys": []string{"team"}})
	call("kinesis", "ListStreams", map[string]any{})
	call("kinesis", "CreateStream", map[string]any{
		"StreamName": "console-on-demand", "StreamModeDetails": map[string]any{"StreamMode": "ON_DEMAND"},
	})
	call("kinesis", "DeleteStream", map[string]any{"StreamName": "console-on-demand"})
	role := "arn:aws:iam::000000000000:role/firehose"
	destination := map[string]any{
		"BucketARN": "arn:aws:s3:::console-destination", "RoleARN": role, "Prefix": "events/",
		"BufferingHints": map[string]any{"SizeInMBs": 5, "IntervalInSeconds": 300},
	}

	for _, source := range []string{"DirectPut", "KinesisStreamAsSource"} {
		name := "console-" + source
		input := map[string]any{
			"DeliveryStreamName": name, "DeliveryStreamType": source, "S3DestinationConfiguration": destination,
		}

		if source == "KinesisStreamAsSource" {
			input["KinesisStreamSourceConfiguration"] = map[string]any{"KinesisStreamARN": arn, "RoleARN": role}
		}

		call("firehose", "CreateDeliveryStream", input)
		fire := func(operation string, input map[string]any) map[string]any {
			t.Helper()
			if input == nil {
				input = map[string]any{}
			}

			input["DeliveryStreamName"] = name
			return call("firehose", operation, input)
		}

		d := fire("DescribeDeliveryStream", nil)["DeliveryStreamDescription"].(map[string]any)
		dest := d["Destinations"].([]any)[0].(map[string]any)
		fire("UpdateDestination", map[string]any{
			"CurrentDeliveryStreamVersionId": d["VersionId"], "DestinationId": dest["DestinationId"],
			"S3DestinationUpdate": map[string]any{
				"Prefix": "updated/", "BufferingHints": map[string]any{"SizeInMBs": 1, "IntervalInSeconds": 900},
			},
		})
		updated := fire("DescribeDeliveryStream", nil)["DeliveryStreamDescription"].(map[string]any)
		target := updated["Destinations"].([]any)[0].(map[string]any)["S3DestinationDescription"].(map[string]any)
		if target["Prefix"] != "updated/" || updated["VersionId"] == d["VersionId"] {
			t.Fatal(updated)
		}

		fire("TagDeliveryStream", map[string]any{"Tags": []any{map[string]any{"Key": "team", "Value": "console"}}})
		if len(fire("ListTagsForDeliveryStream", nil)["Tags"].([]any)) != 1 {
			t.Fatal("missing delivery tags")
		}

		fire("UntagDeliveryStream", map[string]any{"TagKeys": []string{"team"}})
		if source == "DirectPut" {
			fire("PutRecord", map[string]any{"Record": map[string]any{"Data": payload}})
		}

		call("firehose", "ListDeliveryStreams", map[string]any{})
		fire("DeleteDeliveryStream", nil)
	}

	stream("DeleteStream", nil)
	for _, service := range []string{"kinesis", "firehose"} {
		consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, service, "NotAllowed", account, region, map[string]any{}),
			http.StatusBadRequest)
	}
}
