package pipes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
)

// deliver passes a filtered batch through the pipe's enrichment and input
// template to its target. Lambda functions and state machines take the batch
// as a JSON array, and every other target one call per item.
func (s *Service) deliver(ctx context.Context, scope request.Scope, p *pipe, events []json.RawMessage) error {
	tc := &templateContext{
		pipeName: p.Name, pipeARN: pipeARN(scope, p.Name), source: p.Source, enrichment: p.Enrichment,
		target: p.Target, ingestion: s.now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	items := events
	if p.Enrichment != "" {
		var err error
		if items, err = s.enrich(ctx, scope, p, tc, items); err != nil {
			return fmt.Errorf("enrichment: %w", err)
		}

		if len(items) == 0 {
			return nil
		}
	}

	if p.TargetParameters != nil && p.TargetParameters.InputTemplate != nil {
		templated := make([]json.RawMessage, len(items))
		for i, item := range items {
			templated[i] = render(*p.TargetParameters.InputTemplate, item, tc)
		}

		items = templated
	}

	return s.send(ctx, scope, p, items)
}

// call runs an operation of another service as the pipe's account and region.
func (s *Service) call(ctx context.Context, scope request.Scope, svc, op string, params map[string]any) (
	map[string]any, error,
) {
	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope, Service: svc, RequestID: common.RandomID(32)})

	return s.caller.Call(ctx, svc, op, params)
}

// enrich invokes the enrichment function with the batch and takes its answer
// as the new batch: the elements of an array, or else one item. A null or
// empty answer leaves nothing to deliver.
func (s *Service) enrich(
	ctx context.Context,
	scope request.Scope,
	p *pipe,
	tc *templateContext,
	events []json.RawMessage,
) ([]json.RawMessage, error) {
	input := events
	if p.EnrichmentParameters != nil && p.EnrichmentParameters.InputTemplate != nil {
		input = make([]json.RawMessage, len(events))
		for i, e := range events {
			input[i] = render(*p.EnrichmentParameters.InputTemplate, e, tc)
		}
	}

	payload, _ := json.Marshal(input)
	out, err := s.call(ctx, scope, "lambda", "Invoke", map[string]any{
		"FunctionName": p.Enrichment, "InvocationType": "RequestResponse", "Payload": string(payload),
	})
	if err != nil {
		return nil, err
	}

	result, _ := out["Payload"].(string)
	if failure, _ := out["FunctionError"].(string); failure != "" {
		return nil, fmt.Errorf("the function failed with %s: %s", failure, result)
	}

	result = strings.TrimSpace(result)
	if result == "" || result == "null" {
		return nil, nil
	}

	var list []json.RawMessage
	if json.Unmarshal([]byte(result), &list) == nil {
		return list, nil
	}

	if !json.Valid([]byte(result)) {
		return nil, errors.New("the function did not return JSON")
	}

	return []json.RawMessage{json.RawMessage(result)}, nil
}

// send calls the pipe's target with the items.
func (s *Service) send(ctx context.Context, scope request.Scope, p *pipe, items []json.RawMessage) error {
	kind, _ := targetKind(p.Target)
	tp := p.TargetParameters
	switch kind {
	case targetLambda:
		invocation := "RequestResponse"
		if tp != nil && tp.LambdaFunctionParameters != nil &&
			common.Value(tp.LambdaFunctionParameters.InvocationType) == invokeAsync {
			invocation = "Event"
		}

		payload, _ := json.Marshal(items)
		out, err := s.call(ctx, scope, "lambda", "Invoke", map[string]any{
			"FunctionName": p.Target, "InvocationType": invocation, "Payload": string(payload),
		})
		if err != nil {
			return err
		}

		if failure, _ := out["FunctionError"].(string); failure != "" {
			result, _ := out["Payload"].(string)
			return fmt.Errorf("the function failed with %s: %s", failure, result)
		}

		return nil
	case targetStates:
		payload, _ := json.Marshal(items)
		_, err := s.call(ctx, scope, "sfn", "StartExecution", map[string]any{
			"StateMachineArn": p.Target, "Input": string(payload),
		})

		return err
	}

	for _, item := range items {
		if err := s.sendItem(ctx, scope, p, kind, text(item)); err != nil {
			return err
		}
	}

	return nil
}

// sendItem calls a target that takes one item at a time.
func (s *Service) sendItem(ctx context.Context, scope request.Scope, p *pipe, kind, body string) error {
	tp := p.TargetParameters
	var svc, op string
	var params map[string]any
	switch kind {
	case targetSQS:
		queue, _ := sqs.QueueURL(p.Target)
		svc, op, params = "sqs", "SendMessage", map[string]any{"QueueUrl": queue, "MessageBody": body}
		if q := tp.SqsQueueParameters; q != nil {
			if q.MessageGroupId != nil {
				params["MessageGroupId"] = *q.MessageGroupId
			}

			if q.MessageDeduplicationId != nil {
				params["MessageDeduplicationId"] = *q.MessageDeduplicationId
			}
		}
	case targetSNS:
		svc, op, params = "sns", "Publish", map[string]any{"TopicArn": p.Target, "Message": body}
	case targetEventBridge:
		entry := map[string]any{
			"EventBusName": p.Target, "Source": "Pipe " + p.Name,
			"DetailType": "Event from " + sourceName(p.Source), "Detail": body,
		}
		if e := tp.EventBridgeEventBusParameters; e != nil {
			if e.Source != nil {
				entry["Source"] = *e.Source
			}

			if e.DetailType != nil {
				entry["DetailType"] = *e.DetailType
			}
		}

		svc, op, params = "eventbridge", "PutEvents", map[string]any{"Entries": []any{entry}}
	case targetKinesis:
		svc, op, params = "kinesis", "PutRecord", map[string]any{
			"StreamARN": p.Target, "Data": body, "PartitionKey": common.Value(tp.KinesisStreamParameters.PartitionKey),
		}
	default:
		return errors.New("TinyStack does not support the target " + p.Target)
	}

	out, err := s.call(ctx, scope, svc, op, params)
	if err != nil {
		return err
	}

	if svc == "eventbridge" {
		if n, _ := out["FailedEntryCount"].(float64); n > 0 {
			return fmt.Errorf("EventBridge rejected the event: %v", out["Entries"])
		}
	}

	return nil
}

// sourceName is how EventBridge events name the kind of source: aws:sqs.
func sourceName(arn string) string {
	if svc, _, _, ok := arnService(arn); ok {
		return "aws:" + svc
	}

	return arn
}
