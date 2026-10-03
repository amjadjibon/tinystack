package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	logstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// queryRows runs a Logs Insights query over the web group's last hour and
// returns its rows as field-to-value maps.
func queryRows(t *testing.T, c *cloudwatchlogs.Client, query string) []map[string]string {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	started, err := c.StartQuery(ctx, &cloudwatchlogs.StartQueryInput{
		LogGroupName: aws.String("web"), QueryString: aws.String(query),
		StartTime: aws.Int64(now.Add(-time.Hour).Unix()), EndTime: aws.Int64(now.Unix()),
	})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}

	results, err := c.GetQueryResults(ctx, &cloudwatchlogs.GetQueryResultsInput{QueryId: started.QueryId})
	if err != nil || results.Status != logstypes.QueryStatusComplete {
		t.Fatalf("%s: %+v %v", query, results, err)
	}

	rows := make([]map[string]string, 0, len(results.Results))
	for _, fields := range results.Results {
		r := map[string]string{}
		for _, f := range fields {
			r[aws.ToString(f.Field)] = aws.ToString(f.Value)
		}

		rows = append(rows, r)
	}

	return rows
}

func TestLogsInsightsQueries(t *testing.T) {
	s := server(t)
	c := logsClient(s, "test")
	ctx := context.Background()
	if _, err := c.CreateLogGroup(
		ctx,
		&cloudwatchlogs.CreateLogGroupInput{LogGroupName: aws.String("web")},
	); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName: aws.String("web"), LogStreamName: aws.String("a"),
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Add(-time.Minute).UnixMilli()
	messages := []string{
		`{"path":"/a","status":200,"ms":10}`,
		`{"path":"/b","status":500,"ms":30}`,
		`{"path":"/a","status":200,"ms":20}`,
		"ERROR user=bob code=7",
	}
	events := make([]logstypes.InputLogEvent, 0, len(messages))
	for i, m := range messages {
		events = append(events, logstypes.InputLogEvent{Timestamp: aws.Int64(now + int64(i)), Message: aws.String(m)})
	}

	if _, err := c.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: aws.String("web"), LogStreamName: aws.String("a"), LogEvents: events,
	}); err != nil {
		t.Fatal(err)
	}

	rows := queryRows(t, c, "fields @timestamp, @message | sort @timestamp desc | limit 2")
	if len(rows) != 2 || rows[0]["@message"] != messages[3] || rows[0]["@timestamp"] == "" {
		t.Fatalf("latest: %+v", rows)
	}

	rows = queryRows(t, c, "filter status >= 500 or path like /^\\/A$/i | fields path, ms | sort ms asc")
	if len(rows) != 3 || rows[0]["ms"] != "10" || rows[2]["path"] != "/b" || rows[0]["@message"] != "" {
		t.Fatalf("filtered: %+v", rows)
	}

	rows = queryRows(t, c, "filter ispresent(path) | stats count(*) as hits, avg(ms) by path")
	if len(rows) != 2 || rows[0]["path"] != "/a" || rows[0]["hits"] != "2" || rows[0]["avg(ms)"] != "15" {
		t.Fatalf("stats: %+v", rows)
	}

	rows = queryRows(t, c, `parse @message "user=* code=*" as user, code | filter user = "bob" | display code`)
	if len(rows) != 1 || rows[0]["code"] != "7" {
		t.Fatalf("parsed: %+v", rows)
	}

	rows = queryRows(t, c, "stats count(*) by bin(5m)")
	if len(rows) == 0 || rows[0]["bin(5m)"] == "" {
		t.Fatalf("binned: %+v", rows)
	}

	var malformed *logstypes.MalformedQueryException
	if _, err := c.StartQuery(ctx, &cloudwatchlogs.StartQueryInput{
		LogGroupName: aws.String("web"), QueryString: aws.String("fields | nonsense"),
		StartTime: aws.Int64(0), EndTime: aws.Int64(time.Now().Unix()),
	}); !errors.As(err, &malformed) {
		t.Fatalf("malformed query: %v", err)
	}

	described, err := c.DescribeQueries(ctx, &cloudwatchlogs.DescribeQueriesInput{
		LogGroupName: aws.String("web"), Status: logstypes.QueryStatusComplete,
	})
	if err != nil || len(described.Queries) != 5 {
		t.Fatalf("described: %+v %v", described, err)
	}

	stopped, err := c.StopQuery(ctx, &cloudwatchlogs.StopQueryInput{QueryId: described.Queries[0].QueryId})
	if err != nil || stopped.Success {
		t.Fatalf("stopped: %+v %v", stopped, err)
	}

	var missing *logstypes.ResourceNotFoundException
	if _, err = c.GetQueryResults(ctx, &cloudwatchlogs.GetQueryResultsInput{
		QueryId: aws.String("00000000-0000-4000-8000-000000000000"),
	}); !errors.As(err, &missing) {
		t.Fatalf("missing query: %v", err)
	}
}
