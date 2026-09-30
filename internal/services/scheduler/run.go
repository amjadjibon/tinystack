package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/scheduler"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// runInterval is how often the runner looks for due schedules.
var runInterval = time.Second

func (s *Service) runner() {
	t := time.NewTicker(runInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.visit()
		}
	}
}

type due struct {
	scope request.Scope
	group string
	name  string
}

func (s *Service) visit() {
	var runs []due
	_ = s.store.View(s.ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(indexPrefix) {
			parts := strings.SplitN(strings.TrimPrefix(k, indexPrefix), "/", 4)
			if len(parts) == 4 {
				runs = append(runs, due{request.Scope{AccountID: parts[0], Region: parts[1]}, parts[2], parts[3]})
			}
		}

		return nil
	})

	for _, r := range runs {
		if s.ctx.Err() != nil {
			return
		}

		target, ok, err := s.claim(r)
		if err != nil {
			s.logger.Warn("claiming a schedule's run failed", "schedule", r.name, "error", err)
		}

		if ok {
			s.invoke(r.scope, target)
		}
	}
}

// claim advances a due schedule to its next run, or retires it, and returns
// the target to invoke.
func (s *Service) claim(r due) (*api.GetScheduleOutput, bool, error) {
	var out *api.GetScheduleOutput
	now := s.now()
	err := s.store.Update(s.ctx, func(tx storage.WriteTx) error {
		k := key(r.scope, kindSchedule, r.group+"/"+r.name)
		rec, found, err := read[record](tx, k)
		switch {
		case err != nil:
			return err
		case !found:
			tx.Delete(indexKey(r.scope, r.group, r.name))
			return nil
		case rec.Next == 0 || fromUnix(rec.Next).After(now) || value(rec.Schedule.State) != stateEnabled:
			return nil
		}

		sc := &rec.Schedule
		p, err := parseExpression(value(sc.ScheduleExpression), value(sc.ScheduleExpressionTimezone))
		if err != nil {
			return err
		}

		out = sc
		rec.Next = 0
		if next, ok := p.next(now, sc.StartDate, sc.EndDate, true); ok {
			rec.Next = unix(next)
		}

		if rec.Next == 0 && value(sc.ActionAfterCompletion) == afterDelete {
			tx.Delete(k)
			tx.Delete(indexKey(r.scope, r.group, r.name))
			return nil
		}

		s.index(tx, r.scope, rec)
		return write(tx, k, rec)
	})

	return out, out != nil && err == nil, err
}

// invoke runs a schedule's target and sends a failed run to its dead-letter
// queue.
func (s *Service) invoke(scope request.Scope, sc *api.GetScheduleOutput) {
	if s.caller == nil {
		return
	}

	t := sc.Target
	arn, input := value(t.Arn), value(t.Input)
	target, op, params, err := targetCall(t)
	if err == nil {
		ctx := request.WithMetadata(s.ctx, request.Metadata{Scope: scope, Service: target, RequestID: randomID()})
		_, err = s.caller.Call(ctx, target, op, params)
	}

	if err == nil {
		return
	}

	s.logger.Warn("a schedule's target failed", "schedule", value(sc.Arn), "target", arn, "error", err)
	if t.DeadLetterConfig == nil {
		return
	}

	e := awserror.From(err)
	attribute := func(v string) any { return map[string]any{"DataType": "String", "StringValue": v} }
	queueURL, _ := sqs.QueueURL(value(t.DeadLetterConfig.Arn))
	ctx := request.WithMetadata(s.ctx, request.Metadata{Scope: scope, Service: "sqs", RequestID: randomID()})
	_, dlqErr := s.caller.Call(ctx, "sqs", "SendMessage", map[string]any{
		"QueueUrl": queueURL, "MessageBody": cmpOr(input, "{}"),
		"MessageAttributes": map[string]any{
			"ERROR_CODE": attribute(e.Code), "ERROR_MESSAGE": attribute(e.Message),
			"EXHAUSTED_RETRY_CONDITION": attribute("MaximumRetryAttempts"), "RETRY_ATTEMPTS": attribute("0"),
			"TARGET_ARN": attribute(arn), "SCHEDULE_ARN": attribute(value(sc.Arn)),
		},
	})
	if dlqErr != nil {
		s.logger.Warn("sending a failed schedule run to its dead-letter queue failed", "schedule", value(sc.Arn),
			"error", dlqErr)
	}
}

// targetCall maps a target to the operation that invokes it.
func targetCall(t *api.Target) (target, op string, params map[string]any, err error) {
	arn, input := value(t.Arn), value(t.Input)
	switch {
	case strings.HasPrefix(arn, universal):
		svc, action, _ := strings.Cut(strings.TrimPrefix(arn, universal), ":")
		params = map[string]any{}
		if err = json.Unmarshal([]byte(cmpOr(input, "{}")), &params); err != nil {
			return "", "", nil, errors.New("the input of a universal target must be a JSON object")
		}

		return svc, strings.ToUpper(action[:1]) + action[1:], params, nil
	case strings.HasPrefix(arn, "arn:aws:lambda:"):
		return "lambda", "Invoke",
			map[string]any{"FunctionName": arn, "InvocationType": "Event", "Payload": cmpOr(input, "{}")}, nil
	case strings.HasPrefix(arn, "arn:aws:sqs:"):
		queueURL, ok := sqs.QueueURL(arn)
		if !ok {
			return "", "", nil, errors.New("the target is not an SQS queue ARN")
		}

		params = map[string]any{"QueueUrl": queueURL, "MessageBody": cmpOr(input, "{}")}
		if p := t.SqsParameters; p != nil && p.MessageGroupId != nil {
			params["MessageGroupId"] = *p.MessageGroupId
		}

		return "sqs", "SendMessage", params, nil
	case strings.HasPrefix(arn, "arn:aws:sns:"):
		return "sns", "Publish", map[string]any{"TopicArn": arn, "Message": cmpOr(input, "{}")}, nil
	case strings.HasPrefix(arn, "arn:aws:events:"):
		p := t.EventBridgeParameters
		entry := map[string]any{
			"EventBusName": arn, "Source": value(p.Source), "DetailType": value(p.DetailType),
			"Detail": cmpOr(input, "{}"),
		}
		return "eventbridge", "PutEvents", map[string]any{"Entries": []any{entry}}, nil
	case strings.HasPrefix(arn, "arn:aws:states:"):
		return "sfn", "StartExecution", map[string]any{"StateMachineArn": arn, "Input": cmpOr(input, "{}")}, nil
	case strings.HasPrefix(arn, "arn:aws:kinesis:"):
		return "kinesis", "PutRecord", map[string]any{
			"StreamARN": arn, "Data": cmpOr(input, "{}"), "PartitionKey": value(t.KinesisParameters.PartitionKey),
		}, nil
	}

	return "", "", nil, errors.New("TinyStack does not support the target " + arn)
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
