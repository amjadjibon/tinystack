package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/firehose"
	fhtypes "github.com/aws/aws-sdk-go-v2/service/firehose/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/amjadjibon/tinystack/internal/config"
)

func metricStreamInput(name string) *cloudwatch.PutMetricStreamInput {
	return &cloudwatch.PutMetricStreamInput{
		Name: new(name), OutputFormat: cwtypes.MetricStreamOutputFormatJson,
		FirehoseArn: new("arn:aws:firehose:us-east-1:000000000000:deliverystream/metrics"),
		RoleArn:     new("arn:aws:iam::000000000000:role/metric-stream"),
	}
}

func TestCloudWatchMetricStreamCRUD(t *testing.T) {
	c := cloudwatchClient(server(t))
	ctx := context.Background()
	in := metricStreamInput("metrics")
	in.Tags = []cwtypes.Tag{{Key: new("env"), Value: new("test")}}
	in.IncludeFilters = []cwtypes.MetricStreamFilter{{Namespace: new("App"), MetricNames: []string{"Latency"}}}
	in.StatisticsConfigurations = []cwtypes.MetricStreamStatisticsConfiguration{
		{
			IncludeMetrics: []cwtypes.MetricStreamStatisticsMetric{
				{Namespace: new("App"), MetricName: new("Latency")},
			},
			AdditionalStatistics: []string{"p99"},
		},
	}

	created, err := c.PutMetricStream(ctx, in)
	mustAuthorize(t, err)
	if !strings.HasSuffix(*created.Arn, ":metric-stream/metrics") {
		t.Fatalf("ARN: %s", *created.Arn)
	}

	got, err := c.GetMetricStream(ctx, &cloudwatch.GetMetricStreamInput{Name: in.Name})
	mustAuthorize(t, err)
	if *got.State != "running" || got.CreationDate == nil || got.LastUpdateDate == nil ||
		len(got.IncludeFilters) != 1 || len(got.StatisticsConfigurations) != 1 {
		t.Fatalf("stream: %+v", got)
	}

	creation := *got.CreationDate
	_, err = c.StopMetricStreams(ctx, &cloudwatch.StopMetricStreamsInput{Names: []string{"metrics"}})
	mustAuthorize(t, err)
	in.Tags = []cwtypes.Tag{{Key: new("env"), Value: new("ignored")}}
	_, err = c.PutMetricStream(ctx, in)
	mustAuthorize(t, err)
	got, err = c.GetMetricStream(ctx, &cloudwatch.GetMetricStreamInput{Name: in.Name})
	mustAuthorize(t, err)
	if *got.State != "stopped" || !creation.Equal(*got.CreationDate) {
		t.Fatalf("update: %+v", got)
	}

	tags, err := c.ListTagsForResource(ctx, &cloudwatch.ListTagsForResourceInput{ResourceARN: created.Arn})
	mustAuthorize(t, err)
	if len(tags.Tags) != 1 || *tags.Tags[0].Value != "test" {
		t.Fatalf("tags: %+v", tags)
	}

	_, err = c.TagResource(ctx, &cloudwatch.TagResourceInput{
		ResourceARN: created.Arn,
		Tags:        []cwtypes.Tag{{Key: new("owner"), Value: new("team")}},
	})
	mustAuthorize(t, err)
	_, err = c.UntagResource(ctx, &cloudwatch.UntagResourceInput{ResourceARN: created.Arn, TagKeys: []string{"env"}})
	mustAuthorize(t, err)
	tags, err = c.ListTagsForResource(ctx, &cloudwatch.ListTagsForResourceInput{ResourceARN: created.Arn})
	mustAuthorize(t, err)
	if len(tags.Tags) != 1 || *tags.Tags[0].Key != "owner" {
		t.Fatalf("retag: %+v", tags)
	}

	_, err = c.StartMetricStreams(ctx, &cloudwatch.StartMetricStreamsInput{Names: []string{"metrics"}})
	mustAuthorize(t, err)
	_, err = c.PutMetricStream(ctx, metricStreamInput("second"))
	mustAuthorize(t, err)
	listed, err := c.ListMetricStreams(ctx, &cloudwatch.ListMetricStreamsInput{MaxResults: new(int32(1))})
	mustAuthorize(t, err)
	if len(listed.Entries) != 1 || listed.NextToken == nil {
		t.Fatalf("list: %+v", listed)
	}

	listed, err = c.ListMetricStreams(ctx, &cloudwatch.ListMetricStreamsInput{NextToken: listed.NextToken})
	mustAuthorize(t, err)
	if len(listed.Entries) != 1 || *listed.Entries[0].Name != "second" {
		t.Fatalf("page: %+v", listed)
	}

	_, err = c.DeleteMetricStream(ctx, &cloudwatch.DeleteMetricStreamInput{Name: in.Name})
	mustAuthorize(t, err)
	_, err = c.GetMetricStream(ctx, &cloudwatch.GetMetricStreamInput{Name: in.Name})
	if err == nil || !strings.Contains(err.Error(), "ResourceNotFound") {
		t.Fatalf("get deleted: %v", err)
	}

	_, err = c.DeleteMetricStream(ctx, &cloudwatch.DeleteMetricStreamInput{Name: in.Name})
	mustAuthorize(t, err)
}

func TestCloudWatchMetricStreamValidation(t *testing.T) {
	c := cloudwatchClient(server(t))
	for name, mutate := range map[string]func(*cloudwatch.PutMetricStreamInput){
		"name":         func(in *cloudwatch.PutMetricStreamInput) { in.Name = new("invalid.name") },
		"missing role": func(in *cloudwatch.PutMetricStreamInput) { in.RoleArn = new("") },
		"cross account": func(in *cloudwatch.PutMetricStreamInput) {
			in.FirehoseArn = new("arn:aws:firehose:us-east-1:123456789012:deliverystream/metrics")
		},
		"linked accounts": func(in *cloudwatch.PutMetricStreamInput) { in.IncludeLinkedAccountsMetrics = new(true) },
		"otel1":           func(in *cloudwatch.PutMetricStreamInput) { in.OutputFormat = "opentelemetry1.0" },
		"otel07":          func(in *cloudwatch.PutMetricStreamInput) { in.OutputFormat = "opentelemetry0.7" },
		"both filters": func(in *cloudwatch.PutMetricStreamInput) {
			in.IncludeFilters = []cwtypes.MetricStreamFilter{{Namespace: new("App")}}
			in.ExcludeFilters = in.IncludeFilters
		},
		"filter namespace": func(in *cloudwatch.PutMetricStreamInput) {
			in.IncludeFilters = []cwtypes.MetricStreamFilter{{Namespace: new(":App")}}
		},
		"filter metric": func(in *cloudwatch.PutMetricStreamInput) {
			in.IncludeFilters = []cwtypes.MetricStreamFilter{{Namespace: new("App"), MetricNames: []string{"\n"}}}
		},
		"statistics": func(in *cloudwatch.PutMetricStreamInput) {
			in.StatisticsConfigurations = []cwtypes.MetricStreamStatisticsConfiguration{{
				AdditionalStatistics: []string{"bad"},
				IncludeMetrics:       []cwtypes.MetricStreamStatisticsMetric{{Namespace: new("App"), MetricName: new("Latency")}},
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := metricStreamInput("metrics")
			mutate(in)
			_, err := c.PutMetricStream(context.Background(), in)
			expected := "InvalidParameterValue"
			if name == "missing role" {
				expected = "MissingParameter"
			}

			if name == "both filters" {
				expected = "InvalidParameterCombination"
			}

			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != expected {
				t.Fatalf("error = %v, want %s", err, expected)
			}
		})
	}
}

func TestCloudWatchMetricStreamDelivery(t *testing.T) {
	const streamARN = "arn:aws:cloudwatch:us-east-1:" + deliveryAccount + ":metric-stream/metrics"
	const firehoseARN = "arn:aws:firehose:us-east-1:" + deliveryAccount + ":deliverystream/metrics"
	choices := append(append([]roleChoice{}, roleChoices...), roleAllowed)
	for i, choice := range choices {
		label := string(choice)
		exclude := i == len(choices)-1
		if exclude {
			label += "/exclude"
		}

		t.Run(label, func(t *testing.T) {
			s, _ := deliveryServer(t, config.AuthorizationEnforce)
			ctx := context.Background()
			buckets := s3Client(s, "test", "us-east-1")
			_, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("metric-lake")})
			mustAuthorize(t, err)
			trust, permissions := roleAllowed.documentsFor("firehose.amazonaws.com", firehoseARN,
				"arn:aws:s3:::metric-lake/*", "s3:PutObject")
			destinationRole := createDeliveryRole(t, s, "destination", trust, permissions)
			_, err = firehoseClient(s).CreateDeliveryStream(ctx, &firehose.CreateDeliveryStreamInput{
				DeliveryStreamName: new("metrics"),
				ExtendedS3DestinationConfiguration: &fhtypes.ExtendedS3DestinationConfiguration{
					BucketARN: new("arn:aws:s3:::metric-lake"), RoleARN: &destinationRole,
					BufferingHints: &fhtypes.BufferingHints{IntervalInSeconds: new(int32(0)), SizeInMBs: new(int32(1))},
				},
			})
			mustAuthorize(t, err)
			trust, permissions = choice.documentsFor("streams.metrics.cloudwatch.amazonaws.com", streamARN,
				firehoseARN, "firehose:PutRecordBatch")
			role := createDeliveryRole(t, s, "metric-stream", trust, permissions)
			cw := cloudwatchClient(s)
			in := metricStreamInput("metrics")
			in.RoleArn = &role
			in.IncludeFilters = []cwtypes.MetricStreamFilter{{Namespace: new("App"), MetricNames: []string{"Latency"}}}
			if exclude {
				in.IncludeFilters = nil
				in.ExcludeFilters = []cwtypes.MetricStreamFilter{
					{Namespace: new("App"), MetricNames: []string{"Filtered"}},
					{Namespace: new("Other")},
				}
			}

			in.StatisticsConfigurations = []cwtypes.MetricStreamStatisticsConfiguration{
				{
					IncludeMetrics: []cwtypes.MetricStreamStatisticsMetric{
						{Namespace: new("App"), MetricName: new("Latency")},
					},
					AdditionalStatistics: []string{"p99"},
				},
			}

			_, err = cw.PutMetricStream(ctx, in)
			mustAuthorize(t, err)
			at := time.Now().Add(-time.Minute).Truncate(time.Minute)
			_, err = cw.PutMetricData(
				ctx,
				&cloudwatch.PutMetricDataInput{Namespace: new("App"), MetricData: []cwtypes.MetricDatum{
					{
						MetricName: new("Latency"),
						Timestamp:  &at,
						Values:     []float64{2, 4},
						Counts:     []float64{1, 2},
						Unit:       cwtypes.StandardUnitMilliseconds,
						Dimensions: []cwtypes.Dimension{{Name: new("Host"), Value: new("api")}},
					},
					{MetricName: new("Filtered"), Timestamp: &at, Value: new(float64(99))},
				}},
			)
			mustAuthorize(t, err)
			_, err = cw.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
				Namespace: new("Other"),
				MetricData: []cwtypes.MetricDatum{
					{MetricName: new("Latency"), Timestamp: &at, Value: new(float64(100))},
				},
			})
			mustAuthorize(t, err)
			wantRoleChecks(t, s, config.AuthorizationEnforce, choice, role)
			var keys []string
			list := func() bool {
				out, listErr := buckets.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new("metric-lake")})
				mustAuthorize(t, listErr)
				keys = nil
				for _, object := range out.Contents {
					keys = append(keys, *object.Key)
				}

				return len(keys) > 0
			}

			if choice != roleAllowed {
				if list() {
					t.Fatal("denied metric stream delivered to S3")
				}

				return
			}

			waitFor(t, "metric stream S3 delivery", list)
			var lines []map[string]any
			for _, key := range keys {
				out, getErr := buckets.GetObject(ctx, &s3.GetObjectInput{Bucket: new("metric-lake"), Key: &key})
				mustAuthorize(t, getErr)
				data, readErr := io.ReadAll(out.Body)
				out.Body.Close()
				mustAuthorize(t, readErr)
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}

					lines = append(lines, record)
				}
			}

			if len(lines) != 1 {
				t.Fatalf("records: %+v", lines)
			}

			record := lines[0]
			value := record["value"].(map[string]any)
			dims := record["dimensions"].(map[string]any)
			if record["metric_stream_name"] != "metrics" || record["account_id"] != deliveryAccount ||
				record["region"] != "us-east-1" || record["namespace"] != "App" || record["metric_name"] != "Latency" ||
				record["unit"] != "Milliseconds" || record["timestamp"] != float64(at.UnixMilli()) || dims["Host"] != "api" ||
				value["max"] != float64(4) || value["min"] != float64(2) || value["sum"] != float64(10) ||
				value["count"] != float64(3) || value["p99"] != float64(4) {
				t.Fatalf("record: %+v", record)
			}
		})
	}
}

func TestCloudFormationMetricStream(t *testing.T) {
	s := server(t)
	c := cfnClient(s)
	ctx := context.Background()
	template := `{"Resources":{"Stream":{"Type":"AWS::CloudWatch::MetricStream","Properties":{
 "Name":"managed-metrics","FirehoseArn":"arn:aws:firehose:us-east-1:000000000000:deliverystream/metrics",
 "RoleArn":"arn:aws:iam::000000000000:role/metric-stream","OutputFormat":"json",
 "Tags":[{"Key":"env","Value":"test"}]}}},
 "Outputs":{"Arn":{"Value":{"Fn::GetAtt":["Stream","Arn"]}},"State":{"Value":{"Fn::GetAtt":["Stream","State"]}}}}`
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("metric-stream"), TemplateBody: &template})
	mustAuthorize(t, err)
	stack := wantStatus(ctx, t, c, "metric-stream", cfntypes.StackStatusCreateComplete)
	if len(stack.Outputs) != 2 {
		t.Fatalf("outputs: %+v", stack.Outputs)
	}

	cw := cloudwatchClient(s)
	got, err := cw.GetMetricStream(ctx, &cloudwatch.GetMetricStreamInput{Name: new("managed-metrics")})
	mustAuthorize(t, err)
	updated := strings.ReplaceAll(template, `"Value":"test"`, `"Value":"updated"`)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("metric-stream"), TemplateBody: &updated})
	mustAuthorize(t, err)
	wantStatus(ctx, t, c, "metric-stream", cfntypes.StackStatusUpdateComplete)
	tags, err := cw.ListTagsForResource(ctx, &cloudwatch.ListTagsForResourceInput{ResourceARN: got.Arn})
	mustAuthorize(t, err)
	if len(tags.Tags) != 1 || *tags.Tags[0].Value != "updated" {
		t.Fatalf("tags: %+v", tags)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("metric-stream")})
	mustAuthorize(t, err)
	waitFor(t, "metric stream deletion", func() bool {
		_, getErr := cw.GetMetricStream(ctx, &cloudwatch.GetMetricStreamInput{Name: new("managed-metrics")})
		return getErr != nil
	})
	_, err = cw.GetMetricStream(ctx, &cloudwatch.GetMetricStreamInput{Name: new("managed-metrics")})
	if err == nil {
		t.Fatal("CloudFormation did not delete metric stream")
	}
}
