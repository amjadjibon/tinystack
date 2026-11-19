package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gt "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestGlueConnections(t *testing.T) {
	ctx := context.Background()
	srv := server(t)
	c := gluecrawlSetup(t, srv)
	jdbc := &gt.ConnectionInput{
		Name: new("JDBC/Source"), ConnectionType: gt.ConnectionTypeJdbc, MatchCriteria: []string{"spark", "dev"},
		ConnectionProperties: map[string]string{
			"JDBC_CONNECTION_URL": "jdbc:postgresql://host/db", "USERNAME": "reader", "PASSWORD": "test-password",
		},
	}
	createInput := &glue.CreateConnectionInput{ConnectionInput: jdbc, Tags: map[string]string{"team": "data"}}
	if _, err := c.CreateConnection(ctx, createInput); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateConnection(ctx, createInput)
	code(t, err, "AlreadyExistsException")
	for _, n := range []string{"net-a", "net-b"} {
		_, err = c.CreateConnection(ctx, &glue.CreateConnectionInput{ConnectionInput: &gt.ConnectionInput{
			Name: new(n), ConnectionType: gt.ConnectionTypeNetwork, ConnectionProperties: map[string]string{},
			MatchCriteria: []string{"dev"},
			PhysicalConnectionRequirements: &gt.PhysicalConnectionRequirements{
				SubnetId: new("subnet-example"), SecurityGroupIdList: []string{"sg-example"},
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = c.CreateConnection(ctx, &glue.CreateConnectionInput{ConnectionInput: &gt.ConnectionInput{
		Name: new("mongo"), ConnectionType: gt.ConnectionTypeMongodb,
		ConnectionProperties: map[string]string{"CONNECTION_URL": "mongodb://example/db"},
	}})
	code(t, err, "InvalidInputException")

	get := &glue.GetConnectionInput{Name: jdbc.Name, HidePassword: true}
	hidden, err := c.GetConnection(ctx, get)
	if err != nil || hidden.Connection.ConnectionProperties["PASSWORD"] != "" ||
		hidden.Connection.ConnectionProperties["USERNAME"] != "reader" {
		t.Fatalf("redacted connection = %+v, %v", hidden, err)
	}

	get.HidePassword = false
	visible, err := c.GetConnection(ctx, get)
	if err != nil || visible.Connection.ConnectionProperties["PASSWORD"] != "test-password" {
		t.Fatalf("connection = %+v, %v", visible, err)
	}

	filter := &glue.GetConnectionsInput{Filter: &gt.GetConnectionsFilter{
		ConnectionType: gt.ConnectionTypeNetwork, MatchCriteria: []string{"dev"},
	}, MaxResults: new(int32(1))}
	for _, n := range []string{"net-a", "net-b"} {
		out, listErr := c.GetConnections(ctx, filter)
		if listErr != nil || len(out.ConnectionList) != 1 || *out.ConnectionList[0].Name != n {
			t.Fatalf("page %s = %+v, %v", n, out, listErr)
		}

		filter.NextToken = out.NextToken
	}

	filter = &glue.GetConnectionsInput{
		Filter:       &gt.GetConnectionsFilter{MatchCriteria: []string{"spark", "dev"}},
		HidePassword: true,
	}
	out, err := c.GetConnections(ctx, filter)
	if err != nil || len(out.ConnectionList) != 1 || *out.ConnectionList[0].Name != *jdbc.Name ||
		out.ConnectionList[0].ConnectionProperties["PASSWORD"] != "" {
		t.Fatalf("criteria-filtered connections = %+v, %v", out, err)
	}

	jdbc.Description = new("updated")
	jdbc.ConnectionProperties = maps.Clone(jdbc.ConnectionProperties)
	jdbc.ConnectionProperties["PASSWORD"] = "replacement"
	if _, updateErr := c.UpdateConnection(
		ctx,
		&glue.UpdateConnectionInput{Name: jdbc.Name, ConnectionInput: jdbc},
	); updateErr != nil {
		t.Fatal(updateErr)
	}

	const arn = "arn:aws:glue:us-east-1:000000000000:connection/JDBC/Source"
	tags, err := c.GetTags(ctx, &glue.GetTagsInput{ResourceArn: new(arn)})
	if err != nil || tags.Tags["team"] != "data" {
		t.Fatalf("tags after update = %+v, %v", tags, err)
	}

	updated, err := c.GetConnection(ctx, get)
	if err != nil || aws.ToString(updated.Connection.Description) != "updated" ||
		!updated.Connection.CreationTime.Equal(*visible.Connection.CreationTime) {
		t.Fatalf("updated connection = %+v, %v", updated, err)
	}

	_, err = c.CreateJob(ctx, &glue.CreateJobInput{
		Name: new("linked"), Role: new("gluecrawl"),
		Command:     &gt.JobCommand{Name: new("pythonshell"), ScriptLocation: new("s3://gluecrawl/job.py")},
		Connections: &gt.ConnectionsList{Connections: []string{*jdbc.Name}},
	})
	if err != nil {
		t.Fatal(err)
	}

	job, err := c.GetJob(ctx, &glue.GetJobInput{JobName: new("linked")})
	if err != nil || !reflect.DeepEqual(job.Job.Connections.Connections, []string{*jdbc.Name}) {
		t.Fatalf("linked job = %+v, %v", job, err)
	}

	batch, err := c.BatchDeleteConnection(ctx, &glue.BatchDeleteConnectionInput{
		ConnectionNameList: []string{*jdbc.Name, "missing", "net-a"},
	})
	if err != nil || !reflect.DeepEqual(batch.Succeeded, []string{*jdbc.Name, "net-a"}) ||
		aws.ToString(batch.Errors["missing"].ErrorCode) != "EntityNotFoundException" {
		t.Fatalf("batch = %+v, %v", batch, err)
	}

	_, err = c.StartJobRun(ctx, &glue.StartJobRunInput{JobName: new("linked")})
	code(t, err, "EntityNotFoundException")
	if _, deleteErr := c.DeleteConnection(
		ctx,
		&glue.DeleteConnectionInput{ConnectionName: new("net-b")},
	); deleteErr != nil {
		t.Fatal(deleteErr)
	}

	_, err = c.GetConnection(ctx, get)
	code(t, err, "EntityNotFoundException")
}

func TestGlueConnectionCloudFormation(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c, stacks := glueClient(s), cfnClient(s)
	const n = "glue-connection-stack"
	const template = `{"Resources":{"Connection":{"Type":"AWS::Glue::Connection","Properties":{
 "CatalogId":{"Ref":"AWS::AccountId"},"ConnectionInput":{"Name":"stack-connection","ConnectionType":"KAFKA",
 "ConnectionProperties":{"KAFKA_BOOTSTRAP_SERVERS":"%s"}}}}},
 "Outputs":{"Name":{"Value":{"Ref":"Connection"}}}}`
	_, err := stacks.CreateStack(ctx, &cloudformation.CreateStackInput{
		StackName: new(n), TemplateBody: new(fmt.Sprintf(template, "kafka1:9092")),
	})
	if err != nil {
		t.Fatal(err)
	}

	st := settled(ctx, t, stacks, n)
	if st.StackStatus != cfntypes.StackStatusCreateComplete ||
		aws.ToString(st.Outputs[0].OutputValue) != "stack-connection" {
		t.Fatalf("create: %+v", st)
	}

	_, err = stacks.UpdateStack(ctx, &cloudformation.UpdateStackInput{
		StackName: new(n), TemplateBody: new(fmt.Sprintf(template, "kafka2:9092")),
	})
	if err != nil {
		t.Fatal(err)
	}

	if st := settled(ctx, t, stacks, n); st.StackStatus != cfntypes.StackStatusUpdateComplete {
		t.Fatalf("update: %+v", st)
	}

	get := &glue.GetConnectionInput{Name: new("stack-connection")}
	out, err := c.GetConnection(ctx, get)
	if err != nil || out.Connection.ConnectionProperties["KAFKA_BOOTSTRAP_SERVERS"] != "kafka2:9092" {
		t.Fatalf("updated connection = %+v, %v", out, err)
	}

	if _, err := stacks.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: new(n)}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "CloudFormation to delete the Glue connection", func() bool {
		_, err := c.GetConnection(ctx, get)
		return errorCode(err) == "EntityNotFoundException"
	})
}

func TestGlueDynamoDBCrawler(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c, ddb := gluecrawlSetup(t, s), client(s, "test", "us-east-1")
	created, err := create(ctx, ddb, "LocalOrders")
	if err != nil {
		t.Fatal(err)
	}

	for i := range 101 {
		item := key(fmt.Sprintf("order-%03d", i))
		item["quantity"] = &ddbtypes.AttributeValueMemberN{Value: "2"}
		item["price"] = &ddbtypes.AttributeValueMemberN{Value: "2.5"}
		item["active"] = &ddbtypes.AttributeValueMemberBOOL{Value: true}
		item["tags"] = &ddbtypes.AttributeValueMemberSS{Value: []string{"blue"}}
		item["weights"] = &ddbtypes.AttributeValueMemberNS{Value: []string{"1", "0.25"}}
		item["history"] = &ddbtypes.AttributeValueMemberL{Value: []ddbtypes.AttributeValue{
			&ddbtypes.AttributeValueMemberN{Value: "1"},
		}}
		nested := map[string]ddbtypes.AttributeValue{"city": &ddbtypes.AttributeValueMemberS{Value: "Paris"}}
		if i%2 == 0 {
			nested["zip"] = &ddbtypes.AttributeValueMemberN{Value: "75000"}
		}

		item["metadata"] = &ddbtypes.AttributeValueMemberM{Value: nested}
		_, putErr := ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: new("LocalOrders"), Item: item})
		if putErr != nil {
			t.Fatal(putErr)
		}
	}

	target := &gt.CrawlerTargets{DynamoDBTargets: []gt.DynamoDBTarget{{Path: new("LocalOrders"), ScanAll: new(false)}}}
	in := &glue.CreateCrawlerInput{
		Name:         new("dynamo-crawl"),
		Role:         new("gluecrawl"),
		DatabaseName: new("lake"),
		Targets:      target,
	}
	_, err = c.CreateCrawler(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	get := &glue.GetTableInput{DatabaseName: in.DatabaseName, Name: new("localorders")}
	for _, all := range []bool{false, true} {
		wantCount := "100"
		if all {
			wantCount = "101"
			target.DynamoDBTargets[0].ScanAll = new(true)
			_, updateErr := c.UpdateCrawler(ctx, &glue.UpdateCrawlerInput{Name: in.Name, Targets: target})
			if updateErr != nil {
				t.Fatal(updateErr)
			}
		}

		gluecrawlStart(t, c, *in.Name)
		out, getErr := c.GetTable(ctx, get)
		if getErr != nil {
			t.Fatal(getErr)
		}

		table := out.Table
		if table.Parameters["classification"] != "dynamodb" || table.Parameters["recordCount"] != wantCount ||
			*table.StorageDescriptor.Location != *created.TableDescription.TableArn {
			t.Fatalf("catalog table = %+v, storage = %+v", table, table.StorageDescriptor)
		}

		want := map[string]string{
			"PK":       "string",
			"quantity": "bigint",
			"price":    "double",
			"active":   "boolean",
			"tags":     "array<string>",
			"weights":  "array<double>",
			"history":  "array<bigint>",
			"metadata": "struct<city:string,zip:bigint>",
		}
		if columns := glueColumnTypes(table.StorageDescriptor.Columns); !maps.Equal(columns, want) {
			t.Fatalf("DynamoDB columns = %v, want %v", columns, want)
		}
	}

	target.DynamoDBTargets[0].ScanRate = new(0.5)
	_, err = c.UpdateCrawler(ctx, &glue.UpdateCrawlerInput{Name: in.Name, Targets: target})
	code(t, err, "ValidationException")
}

func glueColumnTypes(columns []gt.Column) map[string]string {
	out := make(map[string]string, len(columns))
	for _, col := range columns {
		out[aws.ToString(col.Name)] = aws.ToString(col.Type)
	}

	return out
}

func TestGlueCatalogCrawler(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := gluecrawlSetup(t, s)
	const database, table = "source_db", "manual_table"
	_, err := c.CreateDatabase(ctx, &glue.CreateDatabaseInput{DatabaseInput: &gt.DatabaseInput{Name: new(database)}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateTable(ctx, &glue.CreateTableInput{DatabaseName: new(database), TableInput: &gt.TableInput{
		Name: new(table), Description: new("maintained in place"), Parameters: map[string]string{"team": "data"},
		StorageDescriptor: &gt.StorageDescriptor{Location: new("s3://gluecrawl/source")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	gluecrawlPut(t, s, "source/day=1/data.json", []byte("{\"id\":1,\"enabled\":true}\n"))
	_, err = c.CreateCrawler(ctx, &glue.CreateCrawlerInput{
		Name: new("catalog-crawl"),
		Role: new("gluecrawl"),
		Targets: &gt.CrawlerTargets{
			CatalogTargets: []gt.CatalogTarget{{DatabaseName: new(database), Tables: []string{table}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	gluecrawlStart(t, c, "catalog-crawl")
	out, err := c.GetTables(ctx, &glue.GetTablesInput{DatabaseName: new(database)})
	if err != nil || len(out.TableList) != 1 {
		t.Fatalf("tables = %+v, %v", out, err)
	}

	got := out.TableList[0]
	if *got.Name != table || *got.Description != "maintained in place" || got.Parameters["team"] != "data" ||
		*got.StorageDescriptor.Location != "s3://gluecrawl/source" || len(got.StorageDescriptor.Columns) != 2 {
		t.Fatalf("recrawled table = %+v", got)
	}

	parts, err := c.GetPartitions(ctx, &glue.GetPartitionsInput{DatabaseName: new(database), TableName: new(table)})
	if err != nil || len(parts.Partitions) != 1 || !reflect.DeepEqual(parts.Partitions[0].Values, []string{"1"}) {
		t.Fatalf("partitions = %+v, %v", parts, err)
	}
}

func TestGlueWorkflowEventBridgeBatches(t *testing.T) {
	ctx := context.Background()
	s := authorizationServer(t, config.AuthorizationEnforce, true)
	c := gluecrawlSetup(t, s)
	const workflow = "by-event"
	const arn = "arn:aws:glue:us-east-1:000000000000:workflow/" + workflow
	gluecrawlPut(t, s, "events/data.json", []byte("{\"id\":1}\n"))
	_, err := c.CreateCrawler(ctx, &glue.CreateCrawlerInput{
		Name: new("event-crawl"), Role: new("gluecrawl"), DatabaseName: new("lake"),
		Targets: &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/events/")}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, createErr := c.CreateWorkflow(ctx, &glue.CreateWorkflowInput{Name: new(workflow)}); createErr != nil {
		t.Fatal(createErr)
	}

	_, err = c.CreateTrigger(ctx, &glue.CreateTriggerInput{
		Name:                   new("event-start"),
		WorkflowName:           new(workflow),
		Type:                   gt.TriggerTypeEvent,
		EventBatchingCondition: &gt.EventBatchingCondition{BatchSize: new(int32(2)), BatchWindow: new(int32(30))},
		Actions:                []gt.Action{{CrawlerName: new("event-crawl")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	roles := iamClient(s, "test", "us-east-1")
	role, err := roles.CreateRole(ctx, &iam.CreateRoleInput{RoleName: new("event-role"), AssumeRolePolicyDocument: new(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` +
			`"Principal":{"Service":"events.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
	)})
	if err != nil {
		t.Fatal(err)
	}

	events := eventsClient(s, "test")
	putRule(t, events, "glue-events", orderPattern)
	targets, err := events.PutTargets(
		ctx,
		&eventbridge.PutTargetsInput{Rule: new("glue-events"), Targets: []ebtypes.Target{
			{Id: new("workflow"), Arn: new(arn), RoleArn: role.Role.Arn},
		}},
	)
	if err != nil || targets.FailedEntryCount != 0 {
		t.Fatalf("targets = %+v, %v", targets, err)
	}

	putEvent := func(detail string) string {
		t.Helper()
		out, putErr := events.PutEvents(ctx, &eventbridge.PutEventsInput{Entries: []ebtypes.PutEventsRequestEntry{{
			Source: new("shop"), DetailType: new("order"), Detail: new(detail),
		}}})
		if putErr != nil || out.FailedEntryCount != 0 {
			t.Fatalf("put event = %+v, %v", out, putErr)
		}

		return aws.ToString(out.Entries[0].EventId)
	}
	putEvent(`{"denied":true}`)

	_, err = roles.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		RoleName:   new("event-role"),
		PolicyName: new("notify"),
		PolicyDocument: new(
			`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"glue:NotifyEvent","Resource":"` + arn + `"}]}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}

	firstID := putEvent(`{"number":1}`)
	get := &glue.GetWorkflowRunsInput{Name: new(workflow)}
	runs, err := c.GetWorkflowRuns(ctx, get)
	if err != nil || len(runs.Runs) != 0 {
		t.Fatalf("partial batch = %+v, %v", runs, err)
	}

	secondID := putEvent(`{"number":2}`)
	waitFor(t, "the event batch workflow to complete", func() bool {
		runs, err = c.GetWorkflowRuns(ctx, get)
		if err != nil {
			t.Fatal(err)
		}

		return len(runs.Runs) == 1 && runs.Runs[0].Status == gt.WorkflowRunStatusCompleted
	})
	first := runs.Runs[0]
	var ids []string
	if condition := first.StartingEventBatchCondition; condition == nil || aws.ToInt32(condition.BatchSize) != 2 ||
		json.Unmarshal(
			[]byte(first.WorkflowRunProperties["aws:eventIds"]),
			&ids,
		) != nil || !reflect.DeepEqual(ids, []string{firstID, secondID}) {
		t.Fatalf("event batch metadata = %+v, %v", condition, first.WorkflowRunProperties)
	}

	_, err = c.UpdateTrigger(ctx, &glue.UpdateTriggerInput{Name: new("event-start"), TriggerUpdate: &gt.TriggerUpdate{
		EventBatchingCondition: &gt.EventBatchingCondition{BatchSize: new(int32(3)), BatchWindow: new(int32(1))},
		Actions:                []gt.Action{{CrawlerName: new("event-crawl")}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	emit(t, events, "shop", `{"number":3}`)
	waitFor(t, "the event batch window to expire", func() bool {
		runs, err = c.GetWorkflowRuns(ctx, get)
		if err != nil {
			t.Fatal(err)
		}

		return len(runs.Runs) == 2
	})
	for _, run := range runs.Runs {
		if *run.WorkflowRunId != *first.WorkflowRunId &&
			(run.StartingEventBatchCondition == nil || aws.ToInt32(run.StartingEventBatchCondition.BatchWindow) != 1) {
			t.Fatalf("missing window condition: %+v", run)
		}
	}
}

func TestGlueGrokAndXMLClassifiers(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := gluecrawlSetup(t, s)
	grok := &gt.CreateGrokClassifierRequest{
		Name:           new("logs"),
		Classification: new("app-logs"),
		GrokPattern: new(
			`%{LEVEL:level} %{INT:pid:int} %{GREEDYDATA:message}`,
		),
		CustomPatterns: new(`LEVEL (INFO|WARN)`),
	}
	_, err := c.CreateClassifier(ctx, &glue.CreateClassifierInput{GrokClassifier: grok})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateClassifier(ctx, &glue.UpdateClassifierInput{GrokClassifier: &gt.UpdateGrokClassifierRequest{
		Name: grok.Name, GrokPattern: new(`%{UNKNOWN:column}`),
	}})
	code(t, err, "InvalidInputException")
	_, err = c.CreateClassifier(ctx, &glue.CreateClassifierInput{XMLClassifier: &gt.CreateXMLClassifierRequest{
		Name: new("rows"), Classification: new("xml"), RowTag: new("row"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	gluecrawlPut(t, s, "logs/data.log", []byte("INFO 42 ready\nWARN 406 retrying\n"))
	gluecrawlPut(
		t,
		s,
		"rows/data.xml",
		[]byte(`<root><row id="42"><name>alice</name><enabled>true</enabled></row></root>`),
	)
	for _, tc := range []struct {
		columns              map[string]string
		name, classification string
	}{
		{map[string]string{"level": "string", "pid": "int", "message": "string"}, "logs", "app-logs"},
		{map[string]string{"_id": "bigint", "name": "string", "enabled": "boolean"}, "rows", "xml"},
	} {
		_, err := c.CreateCrawler(ctx, &glue.CreateCrawlerInput{
			Name: new(
				tc.name,
			),
			Role:         new("gluecrawl"),
			DatabaseName: new("lake"),
			Classifiers:  []string{"logs", "rows"},
			Targets:      &gt.CrawlerTargets{S3Targets: []gt.S3Target{{Path: new("s3://gluecrawl/" + tc.name + "/")}}},
		})
		if err != nil {
			t.Fatal(err)
		}

		gluecrawlStart(t, c, tc.name)
		out, err := c.GetTable(ctx, &glue.GetTableInput{DatabaseName: new("lake"), Name: new(tc.name)})
		if err != nil {
			t.Fatal(err)
		}

		if got := glueColumnTypes(out.Table.StorageDescriptor.Columns); !maps.Equal(got, tc.columns) ||
			out.Table.Parameters["classification"] != tc.classification {
			t.Fatalf("%s: columns %v and classification %s", tc.name, got, out.Table.Parameters["classification"])
		}
	}
}
