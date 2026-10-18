package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func TestEventBridgeArchivesAndReplays(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	q := sqsClient(s, "test")
	ctx := context.Background()
	if _, err := c.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: aws.String("orders")}); err != nil {
		t.Fatal(err)
	}

	busARN := "arn:aws:events:us-east-1:000000000000:event-bus/orders"
	_, err := c.CreateArchive(ctx, &eventbridge.CreateArchiveInput{
		ArchiveName: aws.String("missing"), EventSourceArn: aws.String(busARN + "x"),
	})
	code(t, err, "ResourceNotFoundException")
	_, err = c.CreateArchive(ctx, &eventbridge.CreateArchiveInput{
		ArchiveName: aws.String("bad"), EventSourceArn: aws.String(busARN), EventPattern: aws.String(`{"source": 1}`),
	})
	code(t, err, "InvalidEventPatternException")

	created, err := c.CreateArchive(ctx, &eventbridge.CreateArchiveInput{
		ArchiveName: aws.String("paid"), EventSourceArn: aws.String(busARN), Description: aws.String("paid orders"),
		EventPattern: aws.String(`{"detail-type": ["paid"]}`), RetentionDays: aws.Int32(7),
	})
	if err != nil || created.State != ebtypes.ArchiveStateEnabled ||
		aws.ToString(created.ArchiveArn) != "arn:aws:events:us-east-1:000000000000:archive/paid" {
		t.Fatalf("%+v %v", created, err)
	}

	_, err = c.CreateArchive(ctx, &eventbridge.CreateArchiveInput{
		ArchiveName: aws.String("paid"), EventSourceArn: aws.String(busARN),
	})
	code(t, err, "ResourceAlreadyExistsException")

	// Two paid events are archived; the shipped one is not.
	before := time.Now().Add(-time.Minute)
	for _, kind := range []string{"paid", "shipped", "paid"} {
		out, putErr := c.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
			EventBusName: aws.String("orders"), Source: aws.String("shop"), DetailType: aws.String(kind),
			Detail: aws.String(`{"kind": "` + kind + `"}`),
		}}})
		if putErr != nil || out.FailedEntryCount != 0 {
			t.Fatalf("%+v %v", out, putErr)
		}
	}

	described, err := c.DescribeArchive(ctx, &eventbridge.DescribeArchiveInput{ArchiveName: aws.String("paid")})
	if err != nil || described.EventCount != 2 || described.SizeBytes == 0 || described.RetentionDays == nil ||
		*described.RetentionDays != 7 || aws.ToString(described.EventSourceArn) != busARN ||
		aws.ToString(described.EventPattern) != `{"detail-type": ["paid"]}` {
		t.Fatalf("%+v %v", described, err)
	}

	if _, err = c.UpdateArchive(ctx, &eventbridge.UpdateArchiveInput{
		ArchiveName: aws.String("paid"), Description: aws.String("all paid orders"),
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListArchives(ctx, &eventbridge.ListArchivesInput{EventSourceArn: aws.String(busARN)})
	if err != nil || len(listed.Archives) != 1 || listed.Archives[0].EventCount != 2 {
		t.Fatalf("%+v %v", listed, err)
	}

	// A rule added after the events were sent receives them in a replay.
	queueURL, queue := queueARN(t, q, "replayed")
	if _, err = c.PutRule(ctx, &eventbridge.PutRuleInput{
		Name: aws.String("all"), EventBusName: aws.String("orders"), EventPattern: aws.String(`{"source": ["shop"]}`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: aws.String("all"), EventBusName: aws.String("orders"),
		Targets: []ebtypes.Target{{Id: aws.String("q"), Arn: &queue}},
	}); err != nil {
		t.Fatal(err)
	}

	archiveARN := aws.ToString(created.ArchiveArn)
	_, err = c.StartReplay(ctx, &eventbridge.StartReplayInput{
		ReplayName:     aws.String("wrong-bus"),
		EventSourceArn: &archiveARN,
		EventStartTime: aws.Time(before),
		EventEndTime:   aws.Time(time.Now().Add(time.Minute)),
		Destination: &ebtypes.ReplayDestination{
			Arn: aws.String("arn:aws:events:us-east-1:000000000000:event-bus/default"),
		},
	})
	code(t, err, "ValidationException")

	started, err := c.StartReplay(ctx, &eventbridge.StartReplayInput{
		ReplayName: aws.String("again"), EventSourceArn: &archiveARN, Description: aws.String("redo"),
		EventStartTime: aws.Time(before), EventEndTime: aws.Time(time.Now().Add(time.Minute)),
		Destination: &ebtypes.ReplayDestination{Arn: aws.String(busARN)},
	})
	if err != nil || started.State != ebtypes.ReplayStateStarting ||
		aws.ToString(started.ReplayArn) != "arn:aws:events:us-east-1:000000000000:replay/again" {
		t.Fatalf("%+v %v", started, err)
	}

	for range 2 {
		var e map[string]any
		if err = json.Unmarshal([]byte(receiveBody(t, q, queueURL)), &e); err != nil {
			t.Fatal(err)
		}

		if e["replay-name"] != "again" || e["detail-type"] != "paid" {
			t.Fatalf("replayed event %v", e)
		}
	}

	replayed, err := c.DescribeReplay(ctx, &eventbridge.DescribeReplayInput{ReplayName: aws.String("again")})
	if err != nil || replayed.State != ebtypes.ReplayStateCompleted || replayed.ReplayEndTime == nil ||
		replayed.EventLastReplayedTime == nil || aws.ToString(replayed.Destination.Arn) != busARN ||
		aws.ToString(replayed.Description) != "redo" {
		t.Fatalf("%+v %v", replayed, err)
	}

	replays, err := c.ListReplays(ctx, &eventbridge.ListReplaysInput{State: ebtypes.ReplayStateCompleted})
	if err != nil || len(replays.Replays) != 1 {
		t.Fatalf("%+v %v", replays, err)
	}

	_, err = c.CancelReplay(ctx, &eventbridge.CancelReplayInput{ReplayName: aws.String("again")})
	code(t, err, "IllegalStatusException")
	_, err = c.StartReplay(ctx, &eventbridge.StartReplayInput{
		ReplayName: aws.String("again"), EventSourceArn: &archiveARN,
		EventStartTime: aws.Time(before), EventEndTime: aws.Time(time.Now()),
		Destination: &ebtypes.ReplayDestination{Arn: aws.String(busARN)},
	})
	code(t, err, "ResourceAlreadyExistsException")

	// A filter that names no matching rule replays nothing, and the
	// replayed events were not archived again.
	if _, err = c.StartReplay(ctx, &eventbridge.StartReplayInput{
		ReplayName: aws.String("filtered"), EventSourceArn: &archiveARN,
		EventStartTime: aws.Time(before), EventEndTime: aws.Time(time.Now().Add(time.Minute)),
		Destination: &ebtypes.ReplayDestination{
			Arn: aws.String(busARN), FilterArns: []string{"arn:aws:events:us-east-1:000000000000:rule/orders/other"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	empty, err := q.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL})
	if err != nil || len(empty.Messages) != 0 {
		t.Fatalf("%+v %v", empty, err)
	}

	described, err = c.DescribeArchive(ctx, &eventbridge.DescribeArchiveInput{ArchiveName: aws.String("paid")})
	if err != nil || described.EventCount != 2 {
		t.Fatalf("%+v %v", described, err)
	}

	if _, err = c.DeleteArchive(ctx, &eventbridge.DeleteArchiveInput{ArchiveName: aws.String("paid")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeArchive(ctx, &eventbridge.DescribeArchiveInput{ArchiveName: aws.String("paid")})
	code(t, err, "ResourceNotFoundException")
}

func TestEventBridgeBusPolicies(t *testing.T) {
	s := server(t)
	c := eventsClient(s, "test")
	ctx := context.Background()
	_, err := c.RemovePermission(ctx, &eventbridge.RemovePermissionInput{StatementId: aws.String("a")})
	code(t, err, "ResourceNotFoundException")

	for _, in := range []*eventbridge.PutPermissionInput{
		{Action: aws.String("events:PutEvents"), Principal: aws.String("111122223333"), StatementId: aws.String("a")},
		{Action: aws.String("events:PutEvents"), Principal: aws.String("*"), StatementId: aws.String("public")},
	} {
		if _, err = c.PutPermission(ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.PutPermission(ctx, &eventbridge.PutPermissionInput{
		Action: aws.String("events:PutRule"), Principal: aws.String("111122223333"), StatementId: aws.String("b"),
	})
	code(t, err, "ValidationException")

	described, err := c.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{})
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Statement []map[string]any
	}
	if err = json.Unmarshal([]byte(aws.ToString(described.Policy)), &doc); err != nil || len(doc.Statement) != 2 ||
		doc.Statement[0]["Principal"].(map[string]any)["AWS"] != "arn:aws:iam::111122223333:root" ||
		doc.Statement[1]["Resource"] != "arn:aws:events:us-east-1:000000000000:event-bus/default" {
		t.Fatalf("%s %v", aws.ToString(described.Policy), err)
	}

	if _, err = c.RemovePermission(ctx, &eventbridge.RemovePermissionInput{StatementId: aws.String("a")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.RemovePermission(ctx, &eventbridge.RemovePermissionInput{StatementId: aws.String("a")})
	code(t, err, "ResourceNotFoundException")

	// A whole policy replaces the statements, and RemoveAllPermissions
	// removes it.
	if _, err = c.CreateEventBus(ctx, &eventbridge.CreateEventBusInput{Name: aws.String("shared")}); err != nil {
		t.Fatal(err)
	}

	whole := `{"Version": "2012-10-17", "Statement": [{"Sid": "all", "Effect": "Allow", "Principal": "*", ` +
		`"Action": "events:PutEvents", "Resource": "*"}]}`
	if _, err = c.PutPermission(ctx, &eventbridge.PutPermissionInput{
		EventBusName: aws.String("shared"), Policy: aws.String(whole),
	}); err != nil {
		t.Fatal(err)
	}

	buses, err := c.ListEventBuses(ctx, &eventbridge.ListEventBusesInput{NamePrefix: aws.String("shared")})
	if err != nil || len(buses.EventBuses) != 1 ||
		!strings.Contains(aws.ToString(buses.EventBuses[0].Policy), `"all"`) {
		t.Fatalf("%+v %v", buses, err)
	}

	if _, err = c.RemovePermission(ctx, &eventbridge.RemovePermissionInput{
		EventBusName: aws.String("shared"), RemoveAllPermissions: true,
	}); err != nil {
		t.Fatal(err)
	}

	described, err = c.DescribeEventBus(ctx, &eventbridge.DescribeEventBusInput{Name: aws.String("shared")})
	if err != nil || described.Policy != nil {
		t.Fatalf("%+v %v", described, err)
	}
}
