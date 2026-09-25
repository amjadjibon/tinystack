package integration_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

func logsClient(s *httptest.Server, key string) *cloudwatchlogs.Client {
	return cloudwatchlogs.New(cloudwatchlogs.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

func messages[T any](events []T, message func(T) *string) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, aws.ToString(message(e)))
	}

	return out
}

func TestLogGroupsAndStreams(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := logsClient(s, "test")
	for _, name := range []string{"/app/web", "/app/worker", "/other"} {
		if _, err := c.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{
			LogGroupName: aws.String(name), Tags: map[string]string{"team": "core"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := c.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("/other")})
	code(t, err, "ResourceAlreadyExistsException")

	groups, err := c.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String("/app/"), Limit: aws.Int32(1),
	})
	if err != nil || len(groups.LogGroups) != 1 || *groups.LogGroups[0].LogGroupName != "/app/web" ||
		groups.NextToken == nil {
		t.Fatalf("first page: %+v %v", groups, err)
	}

	next, err := c.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String("/app/"), NextToken: groups.NextToken,
	})
	if err != nil || len(next.LogGroups) != 1 || *next.LogGroups[0].LogGroupName != "/app/worker" ||
		next.NextToken != nil {
		t.Fatalf("second page: %+v %v", next, err)
	}

	web := groups.LogGroups[0]
	if *web.Arn != "arn:aws:logs:us-east-1:000000000000:log-group:/app/web:*" ||
		*web.LogGroupArn != strings.TrimSuffix(*web.Arn, ":*") || web.LogGroupClass != logtypes.LogGroupClassStandard {
		t.Fatalf("group = %+v", web)
	}

	// Another account sees none of these groups.
	other, err := logsClient(s, "team-b").DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{})
	if err != nil || len(other.LogGroups) != 0 {
		t.Fatalf("other account: %+v %v", other, err)
	}

	// Retention and tags.
	if _, err = c.PutRetentionPolicy(ctx, &cloudwatchlogs.PutRetentionPolicyInput{
		LogGroupName: aws.String("/app/web"), RetentionInDays: aws.Int32(7),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutRetentionPolicy(ctx, &cloudwatchlogs.PutRetentionPolicyInput{
		LogGroupName: aws.String("/app/web"), RetentionInDays: aws.Int32(8),
	})
	code(t, err, "InvalidParameterException")

	if _, err = c.TagResource(ctx, &cloudwatchlogs.TagResourceInput{
		ResourceArn: web.LogGroupArn, Tags: map[string]string{"env": "dev"},
	}); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &cloudwatchlogs.ListTagsForResourceInput{ResourceArn: web.LogGroupArn})
	if err != nil || len(tags.Tags) != 2 || tags.Tags["env"] != "dev" {
		t.Fatalf("tags = %v %v", tags, err)
	}

	// Streams list by name or by their latest event.
	for _, name := range []string{"b", "a", "c"} {
		if _, err = c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
			LogGroupName: aws.String("/app/web"), LogStreamName: aws.String(name),
		}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UnixMilli()
	for i, name := range []string{"c", "a", "b"} {
		if _, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
			LogGroupName:  aws.String("/app/web"),
			LogStreamName: aws.String(name),
			LogEvents:     []logtypes.InputLogEvent{{Timestamp: aws.Int64(now + int64(i)), Message: aws.String(name)}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	streamNames := func(in *cloudwatchlogs.DescribeLogStreamsInput) []string {
		t.Helper()
		out, describeErr := c.DescribeLogStreams(ctx, in)
		if describeErr != nil {
			t.Fatal(describeErr)
		}

		return messages(out.LogStreams, func(s logtypes.LogStream) *string { return s.LogStreamName })
	}

	if got := streamNames(&cloudwatchlogs.DescribeLogStreamsInput{LogGroupName: aws.String("/app/web")}); !slices.Equal(
		got, []string{"a", "b", "c"},
	) {
		t.Fatalf("by name = %v", got)
	}

	if got := streamNames(&cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupIdentifier: web.LogGroupArn, OrderBy: logtypes.OrderByLastEventTime, Descending: aws.Bool(true),
	}); !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Fatalf("by last event = %v", got)
	}

	if _, err = c.DeleteLogGroup(
		ctx,
		&cloudwatchlogs.DeleteLogGroupInput{LogGroupName: aws.String("/app/web")},
	); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeLogStreams(ctx, &cloudwatchlogs.DescribeLogStreamsInput{LogGroupName: aws.String("/app/web")})
	code(t, err, "ResourceNotFoundException")
}

func TestLogEvents(t *testing.T) {
	ctx := context.Background()
	c := logsClient(server(t), "test")
	group, stream := aws.String("app"), aws.String("instance-1")
	if _, err := c.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{LogGroupName: group}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"instance-1", "instance-2"} {
		if _, err := c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
			LogGroupName: group, LogStreamName: aws.String(name),
		}); err != nil {
			t.Fatal(err)
		}
	}

	base := time.Now().Add(-time.Hour).UnixMilli()
	var batch []logtypes.InputLogEvent
	for i := range 25 {
		batch = append(batch, logtypes.InputLogEvent{
			Timestamp: aws.Int64(base + int64(i/2)), // equal timestamps keep their order
			Message:   aws.String(fmt.Sprintf("line %02d", i)),
		})
	}

	put, err := c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: group, LogStreamName: stream, LogEvents: batch,
	})
	if err != nil || put.RejectedLogEventsInfo != nil {
		t.Fatalf("put = %+v %v", put, err)
	}

	// Events older than 14 days are skipped and reported.
	old := time.Now().Add(-15 * 24 * time.Hour).UnixMilli()
	put, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: group, LogStreamName: stream, LogEvents: []logtypes.InputLogEvent{
			{Timestamp: aws.Int64(old), Message: aws.String("old")},
			{Timestamp: aws.Int64(old + 1), Message: aws.String("old")},
		},
	})
	if err != nil || put.RejectedLogEventsInfo == nil ||
		aws.ToInt32(put.RejectedLogEventsInfo.TooOldLogEventEndIndex) != 1 {
		t.Fatalf("old put = %+v %v", put, err)
	}

	_, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: group, LogStreamName: stream, LogEvents: []logtypes.InputLogEvent{
			{Timestamp: aws.Int64(base + 1), Message: aws.String("x")},
			{Timestamp: aws.Int64(base), Message: aws.String("y")},
		},
	})
	code(t, err, "InvalidParameterException")

	_, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: group, LogStreamName: aws.String("missing"),
		LogEvents: []logtypes.InputLogEvent{{Timestamp: aws.Int64(base), Message: aws.String("x")}},
	})
	code(t, err, "ResourceNotFoundException")

	if _, err = c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: group, LogStreamName: aws.String("instance-2"), LogEvents: []logtypes.InputLogEvent{
			{Timestamp: aws.Int64(base + 3), Message: aws.String("ERROR disk full")},
			{Timestamp: aws.Int64(base + 4), Message: aws.String("WARN disk almost full")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Reading forward from the head pages through all 25 events once;
	// at the end the same token comes back.
	var forward []string
	in := &cloudwatchlogs.GetLogEventsInput{
		LogGroupName: group, LogStreamName: stream, StartFromHead: aws.Bool(true), Limit: aws.Int32(10),
	}
	for {
		out, getErr := c.GetLogEvents(ctx, in)
		if getErr != nil {
			t.Fatal(getErr)
		}

		forward = append(forward, messages(out.Events, func(e logtypes.OutputLogEvent) *string { return e.Message })...)
		if in.NextToken != nil && *out.NextForwardToken == *in.NextToken {
			break
		}

		in.NextToken = out.NextForwardToken
	}

	if len(forward) != 25 || forward[0] != "line 00" || forward[24] != "line 24" {
		t.Fatalf("forward = %v", forward)
	}

	// Without startFromHead the latest events come first, and the backward
	// token reads the page before them.
	tail, err := c.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName: group, LogStreamName: stream, Limit: aws.Int32(5),
	})
	if err != nil {
		t.Fatal(err)
	}

	got := messages(tail.Events, func(e logtypes.OutputLogEvent) *string { return e.Message })
	if !slices.Equal(got, []string{"line 20", "line 21", "line 22", "line 23", "line 24"}) {
		t.Fatalf("tail = %v", got)
	}

	before, err := c.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName: group, LogStreamName: stream, Limit: aws.Int32(2), NextToken: tail.NextBackwardToken,
	})
	if got = messages(before.Events, func(e logtypes.OutputLogEvent) *string { return e.Message }); err != nil ||
		!slices.Equal(got, []string{"line 18", "line 19"}) {
		t.Fatalf("before = %v %v", got, err)
	}

	// Filtering interleaves streams in time order and matches terms.
	filtered, err := c.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: group, FilterPattern: aws.String(`?ERROR ?"line 05"`),
	})
	if err != nil {
		t.Fatal(err)
	}

	got = messages(filtered.Events, func(e logtypes.FilteredLogEvent) *string { return e.Message })
	if !slices.Equal(got, []string{"line 05", "ERROR disk full"}) ||
		*filtered.Events[1].LogStreamName != "instance-2" || *filtered.Events[0].EventId == "" {
		t.Fatalf("filtered = %v", got)
	}

	var all []string
	filterIn := &cloudwatchlogs.FilterLogEventsInput{LogGroupName: group, Limit: aws.Int32(4)}
	for {
		out, filterErr := c.FilterLogEvents(ctx, filterIn)
		if filterErr != nil {
			t.Fatal(filterErr)
		}

		all = append(all, messages(out.Events, func(e logtypes.FilteredLogEvent) *string { return e.Message })...)
		if out.NextToken == nil {
			break
		}

		filterIn.NextToken = out.NextToken
	}

	if len(all) != 27 {
		t.Fatalf("paged filter found %d events", len(all))
	}

	excluded, err := c.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: group, FilterPattern: aws.String("disk -WARN"),
	})
	if got = messages(excluded.Events, func(e logtypes.FilteredLogEvent) *string { return e.Message }); err != nil ||
		!slices.Equal(got, []string{"ERROR disk full"}) {
		t.Fatalf("excluded = %v %v", got, err)
	}

	_, err = c.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: group, FilterPattern: aws.String(`{ $.level = "error" }`),
	})
	code(t, err, "InvalidParameterException")
}

func TestLambdaLogs(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	functions := lambdaClient(s, "test")
	createFunction(t, functions, "echo", "app.echo")
	out, err := functions.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: aws.String("echo"), Payload: []byte(`{"n":1}`), LogType: lambdatypes.LogTypeTail,
	})
	if err != nil {
		t.Fatal(err)
	}

	tail, err := base64.StdEncoding.DecodeString(aws.ToString(out.LogResult))
	if err != nil || !strings.Contains(string(tail), `echo received {"n":1}`) ||
		!strings.Contains(string(tail), "REPORT RequestId: ") {
		t.Fatalf("log tail = %q %v", tail, err)
	}

	c := logsClient(s, "test")
	streams, err := c.DescribeLogStreams(ctx, &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName: aws.String("/aws/lambda/echo"),
	})
	if err != nil || len(streams.LogStreams) != 1 ||
		!strings.Contains(*streams.LogStreams[0].LogStreamName, "/[$LATEST]") {
		t.Fatalf("streams = %+v %v", streams, err)
	}

	events, err := c.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String("/aws/lambda/echo"),
		LogStreamName: streams.LogStreams[0].LogStreamName,
		StartFromHead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	got := messages(events.Events, func(e logtypes.OutputLogEvent) *string { return e.Message })
	if len(got) != 4 || !strings.HasPrefix(got[0], "START RequestId: ") || got[1] != `echo received {"n":1}` ||
		!strings.HasPrefix(got[2], "END RequestId: ") || !strings.HasPrefix(got[3], "REPORT RequestId: ") {
		t.Fatalf("function log = %q", got)
	}

	_, err = functions.Invoke(ctx, &sdklambda.InvokeInput{
		FunctionName: aws.String("echo"), InvocationType: lambdatypes.InvocationTypeEvent,
		LogType: lambdatypes.LogTypeTail,
	})
	if got, _ := lambdaCode(err); got != "InvalidParameterValueException" {
		t.Fatalf("tail of an async invocation: %v", err)
	}
}
