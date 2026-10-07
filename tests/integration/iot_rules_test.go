package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iot"
	iottypes "github.com/aws/aws-sdk-go-v2/service/iot/types"
	"github.com/aws/aws-sdk-go-v2/service/iotdataplane"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const ruleRole = "arn:aws:iam::000000000000:role/iot-rules"

func createRule(
	t *testing.T,
	c *iot.Client,
	name, sql string,
	actions []iottypes.Action,
	errorAction *iottypes.Action,
) {
	t.Helper()
	_, err := c.CreateTopicRule(context.Background(), &iot.CreateTopicRuleInput{
		RuleName: aws.String(name),
		TopicRulePayload: &iottypes.TopicRulePayload{
			Sql: aws.String(
				sql,
			),
			Actions:          actions,
			ErrorAction:      errorAction,
			AwsIotSqlVersion: aws.String("2016-03-23"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func sqsAction(queueURL string) iottypes.Action {
	return iottypes.Action{Sqs: &iottypes.SqsAction{QueueUrl: aws.String(queueURL), RoleArn: aws.String(ruleRole)}}
}

func TestIoTTopicRules(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")
	data := dataClient(s, "test")
	q := sqsClient(s, "test")
	queue := func(name string) string {
		out, err := q.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name)})
		if err != nil {
			t.Fatal(err)
		}

		return aws.ToString(out.QueueUrl)
	}

	readings, failures, ingested := queue("readings"), queue("failures"), queue("ingested")
	createRule(t, c, "hot",
		"SELECT temp, topic(2) AS device, temp * 2 AS doubled, upper(unit) AS unit FROM 'sensors/+/temp' "+
			"WHERE temp > 20 AND unit IN ['c', 'f']",
		[]iottypes.Action{sqsAction(readings)}, nil)

	publish := func(topic, payload string) {
		t.Helper()
		if _, err := data.Publish(ctx, &iotdataplane.PublishInput{
			Topic: aws.String(topic), Payload: []byte(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The WHERE clause drops the cold reading, and the hot one arrives as the
	// SELECT renders it.
	publish("sensors/d1/temp", `{"temp": 10, "unit": "c"}`)
	publish("sensors/d2/temp", `{"temp": 25, "unit": "c", "extra": true}`)
	var got map[string]any
	if err := json.Unmarshal([]byte(awaitBody(t, q, readings)), &got); err != nil ||
		got["temp"] != 25.0 || got["device"] != "d2" || got["doubled"] != 50.0 || got["unit"] != "C" ||
		got["extra"] != nil {
		t.Fatalf("rule output = %v, %v", got, err)
	}

	if body := receiveOne(t, q, readings); body != "" {
		t.Fatalf("the cold reading reached the queue: %s", body)
	}

	// A failed action runs the error action with IoT's failure report.
	createRule(t, c, "broken", "SELECT * FROM 'broken/#'",
		[]iottypes.Action{sqsAction("http://localhost/000000000000/missing")},
		&iottypes.Action{Sqs: &iottypes.SqsAction{QueueUrl: aws.String(failures), RoleArn: aws.String(ruleRole)}})
	publish("broken/x", `{"a":1}`)
	var report map[string]any
	if err := json.Unmarshal([]byte(awaitBody(t, q, failures)), &report); err != nil ||
		report["ruleName"] != "broken" || report["topic"] != "broken/x" ||
		report["base64OriginalPayload"] != "eyJhIjoxfQ==" {
		t.Fatalf("error action payload = %v, %v", report, err)
	}

	if f, _ := report["failures"].([]any); len(f) != 1 {
		t.Fatalf("failures = %v", report["failures"])
	}

	// Basic Ingest runs only the named rule, whatever its FROM, and reaches
	// no subscriber.
	watcher := connected(t, mqttURL(s), "watcher")
	watcher.subscribe("out/#")
	looped := connected(t, mqttURL(s), "looped")
	looped.subscribe("loop")
	createRule(t, c, "ingest", "SELECT *, topic() AS t FROM 'never/used'",
		[]iottypes.Action{sqsAction(ingested)}, nil)
	device := connected(t, mqttURL(s), "device-1")
	if code := device.publish("$aws/rules/ingest/plant/7", `{"v":1}`, false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if err := json.Unmarshal([]byte(awaitBody(t, q, ingested)), &got); err != nil ||
		got["v"] != 1.0 || got["t"] != "plant/7" {
		t.Fatalf("ingested = %v, %v", got, err)
	}

	// Republishing reaches subscribers, with substitution templates, and
	// never reruns the rule that republished.
	createRule(t, c, "relay", "SELECT * FROM 'in/+'", []iottypes.Action{{Republish: &iottypes.RepublishAction{
		Topic: aws.String("out/${topic(2)}"), RoleArn: aws.String(ruleRole), Qos: aws.Int32(1),
	}}}, nil)
	createRule(t, c, "loop", "SELECT * FROM 'loop'", []iottypes.Action{{Republish: &iottypes.RepublishAction{
		Topic: aws.String("loop"), RoleArn: aws.String(ruleRole),
	}}}, nil)
	if code := device.publish("in/abc", "raw bytes", false); code != 0 {
		t.Fatalf("puback 0x%x", code)
	}

	if msg := watcher.message("out/abc"); string(msg.Payload) != "raw bytes" {
		t.Fatalf("republished = %+v", msg)
	}

	publish("loop", "once")
	looped.message("loop")
	looped.message("loop")

	// DynamoDB v2 writes the selected document as an item.
	db := client(s, "test", "us-east-1")
	if _, err := db.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("telemetry"), BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: ddbtypes.KeyTypeHash}},
	}); err != nil {
		t.Fatal(err)
	}

	createRule(t, c, "store", "SELECT topic(2) AS id, level FROM 'tanks/+'", []iottypes.Action{{
		DynamoDBv2: &iottypes.DynamoDBv2Action{
			RoleArn: aws.String(ruleRole), PutItem: &iottypes.PutItemInput{TableName: aws.String("telemetry")},
		},
	}}, nil)
	publish("tanks/t1", `{"level": 3.5}`)
	waitFor(t, "the item", func() bool {
		item, err := db.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String("telemetry"),
			Key:       map[string]ddbtypes.AttributeValue{"id": &ddbtypes.AttributeValueMemberS{Value: "t1"}},
		})
		if err != nil {
			t.Fatal(err)
		}

		n, ok := item.Item["level"].(*ddbtypes.AttributeValueMemberN)
		return ok && n.Value == "3.5"
	})
}

func TestIoTTopicRuleAPI(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := iotClient(s, "test")
	action := []iottypes.Action{sqsAction("http://localhost/000000000000/q")}
	_, err := c.CreateTopicRule(ctx, &iot.CreateTopicRuleInput{
		RuleName: aws.String("first"),
		TopicRulePayload: &iottypes.TopicRulePayload{
			Sql: aws.String("SELECT * FROM 'a/#'"), Actions: action, Description: aws.String("first rule"),
		},
		Tags: aws.String("team=devices&env=dev"),
	})
	if err != nil {
		t.Fatal(err)
	}

	createRule(t, c, "second", "SELECT * FROM 'b/+'", action, nil)
	_, err = c.CreateTopicRule(ctx, &iot.CreateTopicRuleInput{
		RuleName:         aws.String("first"),
		TopicRulePayload: &iottypes.TopicRulePayload{Sql: aws.String("SELECT * FROM 'a'"), Actions: action},
	})
	wantCode(t, err, "ResourceAlreadyExistsException")

	got, err := c.GetTopicRule(ctx, &iot.GetTopicRuleInput{RuleName: aws.String("first")})
	if err != nil {
		t.Fatal(err)
	}

	if r := got.Rule; aws.ToString(got.RuleArn) != "arn:aws:iot:us-east-1:000000000000:rule/first" ||
		aws.ToString(r.Description) != "first rule" || aws.ToString(r.AwsIotSqlVersion) != "2015-10-08" ||
		len(r.Actions) != 1 || r.CreatedAt == nil || aws.ToBool(r.RuleDisabled) {
		t.Fatalf("rule = %+v", got)
	}

	tags, err := c.ListTagsForResource(ctx, &iot.ListTagsForResourceInput{ResourceArn: got.RuleArn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatalf("tags = %+v, %v", tags, err)
	}

	if _, err = c.DisableTopicRule(ctx, &iot.DisableTopicRuleInput{RuleName: aws.String("second")}); err != nil {
		t.Fatal(err)
	}

	list, err := c.ListTopicRules(ctx, &iot.ListTopicRulesInput{RuleDisabled: aws.Bool(true)})
	if err != nil || len(list.Rules) != 1 || aws.ToString(list.Rules[0].RuleName) != "second" ||
		aws.ToString(list.Rules[0].TopicPattern) != "b/+" {
		t.Fatalf("disabled rules = %+v, %v", list, err)
	}

	list, err = c.ListTopicRules(ctx, &iot.ListTopicRulesInput{Topic: aws.String("a/x/y")})
	if err != nil || len(list.Rules) != 1 || aws.ToString(list.Rules[0].RuleName) != "first" {
		t.Fatalf("rules for a/x/y = %+v, %v", list, err)
	}

	if _, err = c.EnableTopicRule(ctx, &iot.EnableTopicRuleInput{RuleName: aws.String("second")}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.ReplaceTopicRule(ctx, &iot.ReplaceTopicRuleInput{
		RuleName:         aws.String("second"),
		TopicRulePayload: &iottypes.TopicRulePayload{Sql: aws.String("SELECT x FROM 'c'"), Actions: action},
	}); err != nil {
		t.Fatal(err)
	}

	got, err = c.GetTopicRule(ctx, &iot.GetTopicRuleInput{RuleName: aws.String("second")})
	if err != nil || aws.ToString(got.Rule.Sql) != "SELECT x FROM 'c'" || aws.ToBool(got.Rule.RuleDisabled) {
		t.Fatalf("replaced = %+v, %v", got, err)
	}

	for sql, code := range map[string]string{
		"SELECT * FROM a":                       "SqlParseException",
		"SELECT nosuch(x) FROM 'a'":             "SqlParseException",
		"SELECT * FROM 'a/#/b'":                 "SqlParseException",
		"SELECT * FROM 'a' WHERE":               "SqlParseException",
		"SELECT * FROM 'a' WHERE x = 1 garbage": "SqlParseException",
	} {
		_, createErr := c.CreateTopicRule(ctx, &iot.CreateTopicRuleInput{
			RuleName:         aws.String("bad"),
			TopicRulePayload: &iottypes.TopicRulePayload{Sql: aws.String(sql), Actions: action},
		})
		wantCode(t, createErr, code)
	}

	for name, a := range map[string]iottypes.Action{
		"no role":  {Sqs: &iottypes.SqsAction{QueueUrl: aws.String("q"), RoleArn: aws.String("nope")}},
		"template": {Sqs: &iottypes.SqsAction{QueueUrl: aws.String("${bad("), RoleArn: aws.String(ruleRole)}},
		"empty":    {},
	} {
		_, createErr := c.CreateTopicRule(ctx, &iot.CreateTopicRuleInput{
			RuleName: aws.String("bad"),
			TopicRulePayload: &iottypes.TopicRulePayload{
				Sql: aws.String("SELECT * FROM 'a'"), Actions: []iottypes.Action{a},
			},
		})
		if !strings.Contains(errorCode(createErr), "InvalidRequestException") {
			t.Fatalf("%s: err = %v", name, createErr)
		}
	}

	_, err = c.CreateTopicRule(ctx, &iot.CreateTopicRuleInput{
		RuleName: aws.String("bad"),
		TopicRulePayload: &iottypes.TopicRulePayload{
			Sql:     aws.String("SELECT * FROM 'a'"),
			Actions: []iottypes.Action{{Http: &iottypes.HttpAction{Url: aws.String("https://example.com")}}},
		},
	})
	wantCode(t, err, "NotImplemented")

	if _, err = c.DeleteTopicRule(ctx, &iot.DeleteTopicRuleInput{RuleName: aws.String("first")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetTopicRule(ctx, &iot.GetTopicRuleInput{RuleName: aws.String("first")})
	wantCode(t, err, "UnauthorizedException")
	_, err = c.DeleteTopicRule(ctx, &iot.DeleteTopicRuleInput{RuleName: aws.String("first")})
	wantCode(t, err, "UnauthorizedException")
}
