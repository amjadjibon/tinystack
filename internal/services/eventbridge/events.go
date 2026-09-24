package eventbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxEntries bounds the total size of one PutEvents request, as EventBridge does.
const maxEntries = 256 * 1024

// event is the envelope EventBridge delivers, in its documented field order.
type event struct {
	Version    string          `json:"version"`
	ID         string          `json:"id"`
	DetailType string          `json:"detail-type"`
	Source     string          `json:"source"`
	Account    string          `json:"account"`
	Time       string          `json:"time"`
	Region     string          `json:"region"`
	Resources  []string        `json:"resources"`
	Detail     json.RawMessage `json:"detail"`
}

func failed(code, message string) api.PutEventsResultEntry {
	return api.PutEventsResultEntry{ErrorCode: ptr(code), ErrorMessage: ptr(message)}
}

func required(name string) api.PutEventsResultEntry {
	return failed("InvalidArgument", "Parameter "+name+" is not valid. Reason: "+name+" is a required argument.")
}

// newEvent validates one entry and builds its envelope; a failed result entry
// explains a rejected one.
func (s *Service) newEvent(scope request.Scope, in *api.PutEventsRequestEntry) (*event, api.PutEventsResultEntry) {
	if err := checkBus(scope, in.EventBusName); err != nil {
		return nil, failed("NotFoundException", "Event bus "+value(in.EventBusName)+" does not exist.")
	}

	switch {
	case value(in.Source) == "":
		return nil, required("Source")
	case value(in.DetailType) == "":
		return nil, required("DetailType")
	case value(in.Detail) == "":
		return nil, required("Detail")
	}

	if _, ok := decodeObject(*in.Detail); !ok {
		return nil, failed("MalformedDetail", "Detail is malformed.")
	}

	var detail bytes.Buffer
	if err := json.Compact(&detail, []byte(*in.Detail)); err != nil {
		return nil, failed("MalformedDetail", "Detail is malformed.")
	}

	at := s.now()
	if in.Time != nil {
		seconds, fraction := math.Modf(*in.Time)
		at = time.Unix(int64(seconds), int64(fraction*1e9))
	}

	resources := in.Resources
	if resources == nil {
		resources = []string{}
	}

	return &event{
		Version:    "0",
		ID:         newID(),
		DetailType: *in.DetailType,
		Source:     *in.Source,
		Account:    scope.AccountID,
		Time:       at.UTC().Format("2006-01-02T15:04:05Z"),
		Region:     scope.Region,
		Resources:  resources,
		Detail:     detail.Bytes(),
	}, api.PutEventsResultEntry{}
}

func (s *Service) PutEvents(ctx context.Context, in *api.PutEventsRequest) (*api.PutEventsResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Entries) == 0 || len(in.Entries) > 10 {
		return nil, invalid("Entries must contain between 1 and 10 entries.")
	}

	size := 0
	for i := range in.Entries {
		entry := &in.Entries[i]
		size += len(value(entry.Source)) + len(value(entry.DetailType)) + len(value(entry.Detail))
		for _, r := range entry.Resources {
			size += len(r)
		}

		if !utf8.ValidString(value(entry.Detail)) {
			return nil, invalid("Detail must be valid UTF-8.")
		}
	}

	if size > maxEntries {
		return nil, invalid("Total size of the entries in the request is over the limit.")
	}

	rules, err := s.enabledRules(ctx, m.Scope)
	if err != nil {
		return nil, err
	}

	out := &api.PutEventsResponse{FailedEntryCount: ptr(int64(0))}
	for i := range in.Entries {
		e, result := s.newEvent(m.Scope, &in.Entries[i])
		if e == nil {
			*out.FailedEntryCount++
			out.Entries = append(out.Entries, result)
			continue
		}

		encoded, encodeErr := json.Marshal(e)
		if encodeErr != nil {
			return nil, encodeErr
		}

		leafValues, _ := flatten(encoded)
		for _, r := range rules {
			if !r.pattern.matches(leafValues) {
				continue
			}

			for j := range r.targets {
				s.deliver(ctx, r.arn, &r.targets[j], e.ID, encoded)
			}
		}

		out.Entries = append(out.Entries, api.PutEventsResultEntry{EventId: ptr(e.ID)})
	}

	return out, nil
}

type compiledRule struct {
	pattern *pattern
	arn     string
	targets []target
}

// enabledRules loads and compiles the enabled rules of the default bus.
func (s *Service) enabledRules(ctx context.Context, scope request.Scope) ([]compiledRule, error) {
	var rules []compiledRule
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys() {
			if !strings.HasPrefix(k, rulePrefix(scope)) {
				continue
			}

			r, _, err := load(tx, k)
			if err != nil {
				return err
			}

			if r.State != stateEnabled || len(r.Targets) == 0 {
				continue
			}

			p, err := compilePattern(r.Pattern)
			if err != nil {
				return err
			}

			rules = append(rules, compiledRule{pattern: p, arn: ruleARN(scope, r.Name), targets: r.Targets})
		}

		return nil
	})

	return rules, err
}

// render returns what a target receives: its constant Input, the value at its
// InputPath, or the whole event.
func render(t *target, encoded []byte) []byte {
	if t.Input != "" {
		return []byte(t.Input)
	}

	if t.InputPath == "" || t.InputPath == "$" {
		return encoded
	}

	var v any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	_ = decoder.Decode(&v)
	for _, field := range strings.Split(strings.TrimPrefix(t.InputPath, "$."), ".") {
		object, _ := v.(map[string]any)
		v = object[field]
	}

	selected, _ := json.Marshal(v)
	return selected
}

// deliver sends an event to one target. As in EventBridge, a failed delivery
// (such as a deleted queue or function) does not fail PutEvents; the delivery
// log records it.
func (s *Service) deliver(ctx context.Context, rule string, t *target, eventID string, encoded []byte) {
	payload := render(t, encoded)
	var err error
	switch strings.SplitN(t.ARN, ":", 4)[2] {
	case serviceSQS:
		entry := &sqsapi.SendMessageBatchRequestEntry{MessageBody: ptr(string(payload))}
		if t.MessageGroupID != "" {
			entry.MessageGroupId = ptr(t.MessageGroupID)
			entry.MessageDeduplicationId = ptr(eventID)
		}

		err = s.queues.Deliver(ctx, t.ARN, entry)
	case serviceSNS:
		_, err = s.topics.Publish(ctx, &snsapi.PublishInput{TopicArn: new(t.ARN), Message: new(string(payload))})
	case serviceLambda:
		err = s.functions.InvokeAsync(ctx, t.ARN, payload)
	}

	s.deliveries.Add(delivery.ViaEventBridge, rule, t.ARN, payload, err)
}
