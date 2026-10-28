package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	query "github.com/aws/aws-sdk-go-v2/service/timestreamquery"
	qt "github.com/aws/aws-sdk-go-v2/service/timestreamquery/types"
	write "github.com/aws/aws-sdk-go-v2/service/timestreamwrite"
	wt "github.com/aws/aws-sdk-go-v2/service/timestreamwrite/types"
)

func TestTimestreamStoredQueries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	e := timestreamQuerySetup(t, srv)
	at := time.Now().UTC().Truncate(time.Second)
	point := wt.Record{
		Dimensions:  []wt.Dimension{{Name: new("host"), Value: new("one")}},
		MeasureName: new("cpu"), MeasureValue: new("2"), MeasureValueType: wt.MeasureValueTypeDouble,
		Time: new(strconv.FormatInt(at.UnixNano(), 10)), TimeUnit: wt.TimeUnitNanoseconds,
	}

	put := func(r wt.Record) error {
		_, err := e.write.WriteRecords(
			ctx,
			&write.WriteRecordsInput{DatabaseName: new(e.dbName), TableName: new(e.tableName), Records: []wt.Record{r}},
		)
		return err
	}

	if err := put(point); err != nil {
		t.Fatal(err)
	}

	if err := put(point); err != nil {
		t.Fatal(err)
	}

	point.MeasureValue = new("4")
	wantTimestreamError(t, put(point), "RejectedRecordsException")
	point.Version = new(int64(2))
	if err := put(point); err != nil {
		t.Fatal(err)
	}

	multi := point
	multi.MeasureName = new("metrics")
	multi.MeasureValue = nil
	multi.MeasureValueType = wt.MeasureValueTypeMulti
	multi.MeasureValues = []wt.MeasureValue{
		{Name: new("temperature"), Type: wt.MeasureValueTypeDouble, Value: new("12.5")},
	}

	if err := put(multi); err != nil {
		t.Fatal(err)
	}

	table := fmt.Sprintf(`%q.%q`, e.dbName, e.tableName)
	sql := "SELECT time, host, measure_value::double FROM " + table + " WHERE measure_name = 'cpu' AND time > ago(1h)"
	out, err := e.query.Query(ctx, &query.QueryInput{QueryString: new(sql)})
	if err != nil || len(out.Rows) != 1 ||
		aws.ToString(out.Rows[0].Data[0].ScalarValue) != at.Format("2006-01-02 15:04:05.000000000") ||
		aws.ToString(out.Rows[0].Data[2].ScalarValue) != "4" ||
		out.ColumnInfo[0].Type.ScalarType != qt.ScalarTypeTimestamp {
		t.Fatalf("query: %+v %v", out, err)
	}

	aggregate := "SELECT bin(time, 1m) AS minute, avg(measure_value::double) AS average FROM " + table +
		" WHERE measure_name='cpu' GROUP BY bin(time,1m)"
	out, err = e.query.Query(ctx, &query.QueryInput{QueryString: new(aggregate)})
	if err != nil || len(out.Rows) != 1 || out.ColumnInfo[0].Type.ScalarType != qt.ScalarTypeTimestamp {
		t.Fatalf("aggregate: %+v %v", out, err)
	}

	out, err = e.query.Query(
		ctx,
		&query.QueryInput{QueryString: new("SELECT temperature FROM " + table + " WHERE measure_name='metrics'")},
	)
	if err != nil || len(out.Rows) != 1 || aws.ToString(out.Rows[0].Data[0].ScalarValue) != "12.5" {
		t.Fatalf("multi: %+v %v", out, err)
	}

	paged := "SELECT measure_name FROM " + table + " ORDER BY measure_name"
	first, err := e.query.Query(ctx, &query.QueryInput{QueryString: new(paged), MaxRows: new(int32(1))})
	if err != nil || first.NextToken == nil {
		t.Fatalf("first: %+v %v", first, err)
	}

	for range 2 {
		page, pageErr := e.query.Query(
			ctx,
			&query.QueryInput{QueryString: new(paged), NextToken: first.NextToken, MaxRows: new(int32(1))},
		)
		if pageErr != nil || len(page.Rows) != 1 || aws.ToString(page.Rows[0].Data[0].ScalarValue) != "metrics" {
			t.Fatalf("page: %+v %v", page, pageErr)
		}
	}

	if _, err = e.query.CancelQuery(ctx, &query.CancelQueryInput{QueryId: first.QueryId}); err != nil {
		t.Fatal(err)
	}

	_, err = e.query.Query(ctx, &query.QueryInput{QueryString: new(paged), NextToken: first.NextToken})
	wantTimestreamError(t, err, "ValidationException")
	for _, bad := range []string{
		"DELETE FROM " + table,
		"SELECT made_up(time) FROM " + table,
		"SELECT * FROM " + table + " JOIN other ON true",
	} {
		_, err = e.query.Query(ctx, &query.QueryInput{QueryString: new(bad)})
		wantTimestreamError(t, err, "ValidationException")
	}

	// A manual invocation exercises the same runner as the background scheduler, including SNS delivery.
	queueClient := sqsClient(srv, "test")
	queue, err := queueClient.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: new("timestream-runs")})
	if err != nil {
		t.Fatal(err)
	}

	attrs, err := queueClient.GetQueueAttributes(
		ctx,
		&sqs.GetQueueAttributesInput{
			QueueUrl:       queue.QueueUrl,
			AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = snsClient(
		srv,
		"test",
	).Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: new(e.topicARN), Protocol: new("sqs"), Endpoint: new(attrs.Attributes["QueueArn"]),
	})
	if err != nil {
		t.Fatal(err)
	}

	input := e.createInput("run-query", "run-query-token")
	input.QueryString = new(
		"SELECT bin(time,1m) AS binned_timestamp, host, avg(measure_value::double) AS avg_cpu FROM " + table +
			" WHERE measure_name='cpu' GROUP BY bin(time,1m),host",
	)
	input.TargetConfiguration.TimestreamConfiguration.MultiMeasureMappings.TargetMultiMeasureName = new("rollup")
	created, err := e.query.CreateScheduledQuery(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.query.ExecuteScheduledQuery(
		ctx,
		&query.ExecuteScheduledQueryInput{ScheduledQueryArn: created.Arn, InvocationTime: new(at)},
	)
	if err != nil {
		t.Fatal(err)
	}

	desc, err := e.query.DescribeScheduledQuery(ctx, &query.DescribeScheduledQueryInput{ScheduledQueryArn: created.Arn})
	if err != nil || desc.ScheduledQuery.LastRunSummary == nil ||
		desc.ScheduledQuery.LastRunSummary.FailureReason != nil {
		t.Fatalf("run: %+v %v", desc, err)
	}

	out, err = e.query.Query(
		ctx,
		&query.QueryInput{QueryString: new("SELECT avg_cpu FROM " + table + " WHERE measure_name='rollup'")},
	)
	if err != nil || len(out.Rows) != 1 || aws.ToString(out.Rows[0].Data[0].ScalarValue) != "4" {
		t.Fatalf("target: %+v %v", out, err)
	}

	messages, err := queueClient.ReceiveMessage(
		ctx,
		&sqs.ReceiveMessageInput{QueueUrl: queue.QueueUrl, WaitTimeSeconds: 1},
	)
	if err != nil || len(messages.Messages) == 0 {
		t.Fatalf("notification: %+v %v", messages, err)
	}

	bad := e.createInput("failed-query", "failed-query-token")
	bad.QueryString = new("SELECT unsupported(time) FROM " + table)
	failed, err := e.query.CreateScheduledQuery(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.query.ExecuteScheduledQuery(
		ctx,
		&query.ExecuteScheduledQueryInput{ScheduledQueryArn: failed.Arn, InvocationTime: new(at)},
	)
	if err != nil {
		t.Fatal(err)
	}

	desc, err = e.query.DescribeScheduledQuery(ctx, &query.DescribeScheduledQueryInput{ScheduledQueryArn: failed.Arn})
	if err != nil || len(desc.ScheduledQuery.RecentlyFailedRuns) != 1 ||
		desc.ScheduledQuery.LastRunSummary.ErrorReportLocation == nil {
		t.Fatalf("failed run: %+v %v", desc, err)
	}

	objects, err := s3Client(srv, "test", "us-east-1").ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: new(e.bucket)})
	if err != nil || len(objects.Contents) != 1 {
		t.Fatalf("error report: %+v %v", objects, err)
	}

	stop()
	srv, stop = persistent(t, dir, &fakeRuntime{})
	e.query = timestreamQueryClient(srv, "test", "us-east-1")
	out, err = e.query.Query(ctx, &query.QueryInput{QueryString: new(sql)})
	if err != nil || len(out.Rows) != 1 {
		t.Fatalf("restart: %+v %v", out, err)
	}

	status, body := post(t, srv, "/_tinystack/reset", nil)
	if status != http.StatusOK {
		t.Fatalf("reset: %d %s", status, body)
	}

	_, err = e.query.Query(ctx, &query.QueryInput{QueryString: new(sql)})
	wantTimestreamError(t, err, "ValidationException")
}

func TestTimestreamRetentionExpiry(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	e := timestreamQuerySetup(t, srv)
	_, err := e.write.UpdateTable(ctx, &write.UpdateTableInput{
		DatabaseName: new(e.dbName),
		TableName:    new(e.tableName),
		RetentionProperties: &wt.RetentionProperties{
			MemoryStoreRetentionPeriodInHours:  new(int64(1)),
			MagneticStoreRetentionPeriodInDays: new(int64(7)),
		},
		MagneticStoreWriteProperties: &wt.MagneticStoreWriteProperties{EnableMagneticStoreWrites: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = e.write.WriteRecords(
		ctx,
		&write.WriteRecordsInput{DatabaseName: new(e.dbName), TableName: new(e.tableName), Records: []wt.Record{
			{
				MeasureName: new("old"), MeasureValue: new("1"), MeasureValueType: wt.MeasureValueTypeBigint,
				Time: new(strconv.FormatInt(time.Now().Add(-48*time.Hour).UnixMilli(), 10)),
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	sql := fmt.Sprintf("SELECT count(*) AS n FROM %q.%q", e.dbName, e.tableName)
	out, err := e.query.Query(ctx, &query.QueryInput{QueryString: new(sql)})
	if err != nil || aws.ToString(out.Rows[0].Data[0].ScalarValue) != "1" {
		t.Fatalf("before expiry: %+v %v", out, err)
	}

	_, err = e.write.UpdateTable(ctx, &write.UpdateTableInput{
		DatabaseName: new(e.dbName),
		TableName:    new(e.tableName),
		RetentionProperties: &wt.RetentionProperties{
			MemoryStoreRetentionPeriodInHours:  new(int64(1)),
			MagneticStoreRetentionPeriodInDays: new(int64(1)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err = e.query.Query(ctx, &query.QueryInput{QueryString: new(sql)})
	if err != nil || aws.ToString(out.Rows[0].Data[0].ScalarValue) != "0" {
		t.Fatalf("after expiry: %+v %v", out, err)
	}
}
