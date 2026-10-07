package iot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"uuid"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// ingestPrefix starts Basic Ingest topics, $aws/rules/<rule>/<topic>,
	// which run one rule and reach no subscriber.
	ingestPrefix = "$aws/rules/"
	// ruleClientPrefix starts the client IDs republish actions publish as,
	// so a rule never runs on what it republished itself.
	ruleClientPrefix = "$rule/"
	// fnLambda is the SQL function that invokes Lambda functions.
	fnLambda = "aws_lambda"
	// Members of the calls actions make.
	paramData      = "Data"
	paramMessage   = "Message"
	paramTimestamp = "Timestamp"
)

// message is a published message, as the rules engine sees it.
type message struct {
	scope     request.Scope
	topic     string
	clientID  string
	principal string
	// ingest names the one rule a Basic Ingest message runs.
	ingest string
	// origin is the client that published the message.
	origin  string
	payload []byte
}

// runRules runs, in the background, the enabled rules a message triggers.
func (s *Service) runRules(m *message) {
	s.bg.Go(func(ctx context.Context) {
		var rules []*topicRule
		err := s.store.View(ctx, func(tx storage.ReadTx) error {
			var err error
			rules, err = storage.Records[topicRule](tx, m.scope.Prefix(service, kindRule))
			return err
		})
		if err != nil {
			s.logger.Warn("iot rules unreadable", "error", err)
			return
		}

		for _, r := range rules {
			if common.Value(r.Payload.RuleDisabled) || m.origin == ruleClientPrefix+r.Name ||
				m.ingest != "" && m.ingest != r.Name {
				continue
			}

			q, err := parseQuery(common.Value(r.Payload.Sql))
			if err != nil || m.ingest == "" && !topicMatches(q.topic, m.topic) {
				continue
			}

			s.runRule(ctx, m, r, q)
		}
	})
}

// failure is an action that failed, as an error action's payload reports
// it.
type failure struct {
	FailedAction   string `json:"failedAction"`
	FailedResource string `json:"failedResource"`
	ErrorMessage   string `json:"errorMessage"`
}

// runRule evaluates a rule on a message and runs its actions, then its error
// action if any of them failed.
func (s *Service) runRule(ctx context.Context, m *message, r *topicRule, q *query) {
	e := &env{
		doc: decodePayload(m.payload), raw: m.payload, now: s.now(), topic: m.topic, clientID: m.clientID,
		principal: m.principal, traceID: uuid.NewV4().String(),
		lambda: func(arn string, payload any) (any, error) { return s.invokeSync(ctx, m.scope, arn, payload) },
	}

	ruleARN := arn(m.scope, kindRule, r.Name)
	var failures []failure
	if q.matches(e) {
		result := q.selectResult(e)
		payload := encodeResult(result)
		if e.err != nil {
			failures = append(failures, failure{"SqlFunction", fnLambda, e.err.Error()})
		} else {
			out := &env{
				doc: result, raw: payload, now: e.now, topic: e.topic, clientID: e.clientID,
				principal: e.principal, traceID: e.traceID,
			}
			for i := range r.Payload.Actions {
				kind, target, err := s.act(ctx, m.scope, r.Name, &r.Payload.Actions[i], out)
				s.deliveries.Add(delivery.ViaIoT, ruleARN, target, payload, err)
				if err != nil {
					failures = append(failures, failure{actionName(kind), target, err.Error()})
				}
			}
		}
	} else if e.err != nil {
		failures = append(failures, failure{"SqlFunction", fnLambda, e.err.Error()})
	}

	if len(failures) == 0 || r.Payload.ErrorAction == nil {
		return
	}

	report := map[string]any{
		"ruleName": r.Name, keyTopic: m.topic, "cloudwatchTraceId": e.traceID, keyClientID: m.clientID,
		"base64OriginalPayload": base64.StdEncoding.EncodeToString(m.payload), "failures": failures,
	}
	payload := encodeResult(report)
	report = decodePayload(payload).(map[string]any)
	_, target, err := s.act(ctx, m.scope, r.Name, r.Payload.ErrorAction, &env{
		doc: report, raw: payload, now: e.now, topic: m.topic, clientID: m.clientID, principal: m.principal,
		traceID: e.traceID,
	})
	s.deliveries.Add(delivery.ViaIoT, ruleARN, target, payload, err)
}

// actionName names an action kind as error action payloads do (SqsAction).
func actionName(kind string) string {
	return strings.ToUpper(kind[:1]) + kind[1:] + "Action"
}

// act runs one action with e's SELECT result, after substituting its
// templates, and returns the action's kind and the resource it targeted.
func (s *Service) act(
	ctx context.Context,
	scope request.Scope,
	rule string,
	a *api.Action,
	e *env,
) (kind, target string, err error) {
	kind, fields, err := actionFields(a)
	if err != nil {
		return "action", "", err
	}

	err = walkStrings(fields, func(name, text string) (string, error) {
		if name == fieldRole {
			return text, nil
		}

		t, parseErr := parseTemplate(text)
		if parseErr != nil {
			return "", parseErr
		}

		return t.render(e), nil
	})
	if err != nil {
		return kind, "", err
	}

	data, err := json.Marshal(map[string]any{kind: fields})
	if err != nil {
		return kind, "", err
	}

	var act api.Action
	if err := json.Unmarshal(data, &act); err != nil {
		return kind, "", err
	}

	call := func(svc, op string, params map[string]any) error {
		_, err := s.caller.Call(callContext(ctx, scope, svc), svc, op, params)
		return err
	}

	payload := string(e.raw)
	switch {
	case act.Republish != nil:
		return kind, *act.Republish.Topic, s.republish(scope, rule, act.Republish, e.raw)
	case act.Sqs != nil:
		body := payload
		if common.Value(act.Sqs.UseBase64) {
			body = base64.StdEncoding.EncodeToString(e.raw)
		}

		return kind, *act.Sqs.QueueUrl, call("sqs", "SendMessage",
			map[string]any{"QueueUrl": *act.Sqs.QueueUrl, "MessageBody": body})
	case act.Sns != nil:
		params := map[string]any{"TopicArn": *act.Sns.TargetArn, paramMessage: payload}
		if common.Value(act.Sns.MessageFormat) == "JSON" {
			params["MessageStructure"] = "json"
		}

		return kind, *act.Sns.TargetArn, call("sns", "Publish", params)
	case act.Lambda != nil:
		return kind, *act.Lambda.FunctionArn, call("lambda", "Invoke", map[string]any{
			"FunctionName": *act.Lambda.FunctionArn, "InvocationType": "Event", "Payload": payload,
		})
	case act.DynamoDB != nil:
		return kind, *act.DynamoDB.TableName, s.dynamoDB(call, act.DynamoDB, e)
	case act.DynamoDBv2 != nil:
		table := *act.DynamoDBv2.PutItem.TableName
		doc, ok := e.doc.(map[string]any)
		if !ok {
			return kind, table, errors.New("the dynamoDBv2 action needs a JSON object")
		}

		item := map[string]any{}
		for k, v := range doc {
			item[k] = attribute(v)
		}

		return kind, table, call("dynamodb", "PutItem", map[string]any{"TableName": table, "Item": item})
	case act.Kinesis != nil:
		key := common.Value(act.Kinesis.PartitionKey)
		if key == "" {
			key = uuid.NewV4().String()
		}

		return kind, *act.Kinesis.StreamName, call("kinesis", "PutRecord", map[string]any{
			"StreamName": *act.Kinesis.StreamName, paramData: payload, "PartitionKey": key,
		})
	case act.Firehose != nil:
		return kind, *act.Firehose.DeliveryStreamName, s.firehose(call, act.Firehose, e)
	case act.S3 != nil:
		return kind, *act.S3.BucketName, call("s3", "PutObject", map[string]any{
			"Bucket": *act.S3.BucketName, "Key": *act.S3.Key, "Body": payload,
		})
	case act.CloudwatchLogs != nil:
		return kind, *act.CloudwatchLogs.LogGroupName, s.cloudwatchLogs(call, rule, act.CloudwatchLogs, e)
	case act.CloudwatchMetric != nil:
		return kind, *act.CloudwatchMetric.MetricName, cloudwatchMetric(call, act.CloudwatchMetric, e.now)
	case act.CloudwatchAlarm != nil:
		a := act.CloudwatchAlarm
		return kind, *a.AlarmName, call("cloudwatch", "SetAlarmState", map[string]any{
			"AlarmName": *a.AlarmName, "StateValue": *a.StateValue, "StateReason": *a.StateReason,
		})
	case act.StepFunctions != nil:
		a := act.StepFunctions
		machine := "arn:aws:states:" + scope.Region + ":" + scope.AccountID + ":stateMachine:" + *a.StateMachineName
		return kind, machine, call("sfn", "StartExecution", map[string]any{
			"StateMachineArn": machine, "Name": common.Value(a.ExecutionNamePrefix) + uuid.NewV4().String(),
			"Input": payload,
		})
	}

	return kind, "", fmt.Errorf("the %s action is not supported", kind)
}

// callContext is the context of a call an action makes in scope.
func callContext(ctx context.Context, scope request.Scope, target string) context.Context {
	return request.WithMetadata(context.WithoutCancel(ctx),
		request.Metadata{Scope: scope, Service: target, RequestID: common.RandomID(32, common.AlphabetLowerHex)})
}

// republish publishes a rule's result to another topic, as the rule's own
// client.
func (s *Service) republish(scope request.Scope, rule string, a *api.RepublishAction, payload []byte) error {
	topic := *a.Topic
	if !withinLimits(topic) || !mqtt.IsValidFilter(topic, true) {
		return fmt.Errorf("the topic %q is invalid", topic)
	}

	b, err := s.broker(scope)
	if err != nil {
		return err
	}

	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish},
		TopicName:   topic, Payload: payload,
	}
	// The broker never acknowledges its own messages, but a QoS 1 message
	// needs a packet ID.
	if common.Value(a.Qos) == 1 {
		pk.FixedHeader.Qos, pk.PacketID = 1, 1
	}

	if h := a.Headers; h != nil {
		pk.Properties.ContentType = common.Value(h.ContentType)
		pk.Properties.ResponseTopic = common.Value(h.ResponseTopic)
		if h.PayloadFormatIndicator != nil {
			pk.Properties.PayloadFormatFlag = true
			if *h.PayloadFormatIndicator == "UTF8_DATA" {
				pk.Properties.PayloadFormat = 1
			}
		}

		if h.CorrelationData != nil {
			if pk.Properties.CorrelationData, err = base64.StdEncoding.DecodeString(*h.CorrelationData); err != nil {
				return errors.New("the correlation data is not base64")
			}
		}

		if h.MessageExpiry != nil {
			expiry, err := strconv.ParseUint(*h.MessageExpiry, 10, 32)
			if err != nil {
				return errors.New("the message expiry is not a number of seconds")
			}

			pk.Properties.MessageExpiryInterval = uint32(expiry)
		}

		for _, p := range h.UserProperties {
			pk.Properties.User = append(pk.Properties.User,
				packets.UserProperty{Key: common.Value(p.Key), Val: common.Value(p.Value)})
		}
	}

	cl := b.server.NewClient(nil, mqtt.LocalListener, ruleClientPrefix+rule, true)
	cl.Properties.ProtocolVersion = 5
	return b.server.InjectPacket(cl, pk)
}

// dynamoDB writes a message to a table as the dynamoDB action does: the
// hash and range keys, and the payload in payloadField.
func (s *Service) dynamoDB(call func(svc, op string, params map[string]any) error, a *api.DynamoDBAction,
	e *env,
) error {
	key := map[string]any{*a.HashKeyField: keyAttribute(*a.HashKeyValue, a.HashKeyType)}
	if a.RangeKeyField != nil {
		key[*a.RangeKeyField] = keyAttribute(common.Value(a.RangeKeyValue), a.RangeKeyType)
	}

	params := map[string]any{"TableName": *a.TableName}
	if common.Value(a.Operation) == "DELETE" {
		params["Key"] = key
		return call("dynamodb", "DeleteItem", params)
	}

	item := key
	if _, ok := e.doc.([]byte); ok {
		item[common.Value(common.OrElse(a.PayloadField, new("payload")))] = map[string]any{"B": string(e.raw)}
	} else {
		item[common.Value(common.OrElse(a.PayloadField, new("payload")))] = attribute(e.doc)
	}

	params["Item"] = item
	return call("dynamodb", "PutItem", params)
}

func keyAttribute(value string, typ *string) map[string]any {
	if common.Value(typ) == "NUMBER" {
		return map[string]any{"N": value}
	}

	return map[string]any{"S": value}
}

// attribute converts a JSON value to a DynamoDB attribute value.
func attribute(v any) map[string]any {
	switch x := v.(type) {
	case nil:
		return map[string]any{"NULL": true}
	case bool:
		return map[string]any{"BOOL": x}
	case int64, float64:
		return map[string]any{"N": stringOf(x)}
	case string:
		return map[string]any{"S": x}
	case []any:
		list := make([]any, len(x))
		for i, e := range x {
			list[i] = attribute(e)
		}

		return map[string]any{"L": list}
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = attribute(e)
		}

		return map[string]any{"M": m}
	}

	return map[string]any{"S": stringOf(v)}
}

// firehose sends a message to a delivery stream, or, in batch mode, each
// element of a JSON array as its own record.
func (s *Service) firehose(call func(svc, op string, params map[string]any) error, a *api.FirehoseAction,
	e *env,
) error {
	sep := common.Value(a.Separator)
	if !common.Value(a.BatchMode) {
		return call("firehose", "PutRecord", map[string]any{
			"DeliveryStreamName": *a.DeliveryStreamName, "Record": map[string]any{paramData: string(e.raw) + sep},
		})
	}

	list, ok := e.doc.([]any)
	if !ok || len(list) == 0 || len(list) > 500 {
		return errors.New("a batch mode firehose action needs a JSON array of 1 to 500 records")
	}

	records := make([]any, len(list))
	for i, v := range list {
		records[i] = map[string]any{paramData: string(encodeResult(v)) + sep}
	}

	return call("firehose", "PutRecordBatch", map[string]any{
		"DeliveryStreamName": *a.DeliveryStreamName, "Records": records,
	})
}

// cloudwatchLogs writes a message to a log group's iot-<rule> stream, or,
// in batch mode, each {"timestamp", "message"} element of a JSON array.
func (s *Service) cloudwatchLogs(call func(svc, op string, params map[string]any) error, rule string,
	a *api.CloudwatchLogsAction, e *env,
) error {
	events := []any{map[string]any{paramTimestamp: float64(e.now.UnixMilli()), paramMessage: string(e.raw)}}
	if common.Value(a.BatchMode) {
		list, ok := e.doc.([]any)
		if !ok || len(list) == 0 {
			return errors.New("a batch mode cloudwatchLogs action needs a JSON array of log events")
		}

		events = events[:0]
		for _, v := range list {
			ev, _ := v.(map[string]any)
			ts, ok := float(ev[keyTimestamp])
			msg, mok := ev["message"].(string)
			if !ok || !mok {
				return errors.New("each batched log event needs a timestamp and a message")
			}

			events = append(events, map[string]any{paramTimestamp: ts, paramMessage: msg})
		}
	}

	stream := "iot-" + rule
	err := call("logs", "CreateLogStream", map[string]any{"LogGroupName": *a.LogGroupName, "LogStreamName": stream})
	if err != nil && !strings.Contains(err.Error(), "ResourceAlreadyExists") {
		return err
	}

	return call("logs", "PutLogEvents", map[string]any{
		"LogGroupName": *a.LogGroupName, "LogStreamName": stream, "LogEvents": events,
	})
}

// cloudwatchMetric puts one metric data point from a message.
func cloudwatchMetric(call func(svc, op string, params map[string]any) error, a *api.CloudwatchMetricAction,
	now time.Time,
) error {
	value, err := strconv.ParseFloat(*a.MetricValue, 64)
	if err != nil {
		return fmt.Errorf("the metric value %q is not a number", *a.MetricValue)
	}

	at := now
	if ts := common.Value(a.MetricTimestamp); ts != "" {
		secs, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return fmt.Errorf("the metric timestamp %q is not Unix seconds", ts)
		}

		at = time.Unix(secs, 0)
	}

	return call("cloudwatch", "PutMetricData", map[string]any{
		"Namespace": *a.MetricNamespace,
		"MetricData": []any{map[string]any{
			"MetricName": *a.MetricName, "Value": value, "Unit": *a.MetricUnit,
			paramTimestamp: at.UTC().Format(time.RFC3339),
		}},
	})
}

// invokeSync calls a function for aws_lambda and decodes its result.
func (s *Service) invokeSync(ctx context.Context, scope request.Scope, arn string, payload any) (any, error) {
	out, err := s.caller.Call(callContext(ctx, scope, "lambda"), "lambda", "Invoke", map[string]any{
		"FunctionName": arn, "InvocationType": "RequestResponse", "Payload": string(encodeResult(payload)),
	})
	if err != nil {
		return nil, err
	}

	if fe, _ := out["FunctionError"].(string); fe != "" {
		return nil, fmt.Errorf("the function failed: %s", fe)
	}

	result, _ := out["Payload"].(string)
	v := decodePayload([]byte(result))
	if b, ok := v.([]byte); ok {
		return string(b), nil
	}

	return v, nil
}
