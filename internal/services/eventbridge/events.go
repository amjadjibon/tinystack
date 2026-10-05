package eventbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxEntries bounds the total size of one PutEvents request, as EventBridge does.
const maxEntries = 256 * 1024

// event is the envelope EventBridge delivers, in its documented field order.
//
//nolint:govet // The field order is the JSON order.
type event struct {
	// at is the event's time, which orders it in archives.
	at         time.Time
	Version    string          `json:"version"`
	ID         string          `json:"id"`
	DetailType string          `json:"detail-type"`
	Source     string          `json:"source"`
	Account    string          `json:"account"`
	Time       string          `json:"time"`
	Region     string          `json:"region"`
	Resources  []string        `json:"resources"`
	Detail     json.RawMessage `json:"detail"`
	// ReplayName names the replay that sent an archived event again.
	ReplayName string `json:"replay-name,omitempty"`
}

func failed(code, message string) api.PutEventsResultEntry {
	return api.PutEventsResultEntry{ErrorCode: new(code), ErrorMessage: new(message)}
}

func required(name string) api.PutEventsResultEntry {
	return failed("InvalidArgument", "Parameter "+name+" is not valid. Reason: "+name+" is a required argument.")
}

// newEvent validates one entry and builds its envelope; a failed result entry
// explains a rejected one.
func (s *Service) newEvent(scope request.Scope, in *api.PutEventsRequestEntry) (*event, api.PutEventsResultEntry) {
	switch {
	case common.Value(in.Source) == "":
		return nil, required("Source")
	case common.Value(in.DetailType) == "":
		return nil, required("DetailType")
	case common.Value(in.Detail) == "":
		return nil, required("Detail")
	}

	if _, ok := pattern.Flatten([]byte(*in.Detail)); !ok {
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
		at:         at,
	}, api.PutEventsResultEntry{}
}

func (s *Service) PutEvents(ctx context.Context, in *api.PutEventsRequest) (*api.PutEventsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if len(in.Entries) == 0 || len(in.Entries) > 10 {
		return nil, invalid("Entries must contain between 1 and 10 entries.")
	}

	size := 0
	for i := range in.Entries {
		entry := &in.Entries[i]
		size += len(common.Value(entry.Source)) + len(common.Value(entry.DetailType)) + len(common.Value(entry.Detail))
		for _, r := range entry.Resources {
			size += len(r)
		}

		if !utf8.ValidString(common.Value(entry.Detail)) {
			return nil, invalid("Detail must be valid UTF-8.")
		}
	}

	if size > maxEntries {
		return nil, invalid("Total size of the entries in the request is over the limit.")
	}

	archives, err := s.archivers(ctx, m.Scope)
	if err != nil {
		return nil, err
	}

	// Each entry names its bus; the rules of each bus are loaded once.
	busRules := map[string][]compiledRule{}
	var kept []archived
	out := &api.PutEventsResponse{FailedEntryCount: new(int64(0))}
	for i := range in.Entries {
		rules, known := busRules[common.Value(in.Entries[i].EventBusName)]
		if !known {
			var rulesErr error
			if rules, rulesErr = s.enabledRules(ctx, m.Scope, in.Entries[i].EventBusName); rulesErr != nil {
				if _, missing := errors.AsType[*awserror.Error](rulesErr); !missing {
					return nil, rulesErr
				}

				*out.FailedEntryCount++
				out.Entries = append(out.Entries, failed("NotFoundException",
					"Event bus "+common.Value(in.Entries[i].EventBusName)+" does not exist."))
				continue
			}

			busRules[common.Value(in.Entries[i].EventBusName)] = rules
		}

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

		leafValues, _ := pattern.Flatten(encoded)
		bus, _ := busName(m.Scope, in.Entries[i].EventBusName)
		for _, a := range archives[bus] {
			if a.pattern == nil || a.pattern.Matches(leafValues) {
				kept = append(kept, archived{at: e.at, archive: a.name, id: e.ID, encoded: encoded})
			}
		}

		for _, r := range rules {
			if !r.pattern.Matches(leafValues) {
				continue
			}

			for j := range r.targets {
				s.deliver(ctx, m.Scope, &r, &r.targets[j], e, encoded)
			}
		}

		out.Entries = append(out.Entries, api.PutEventsResultEntry{EventId: new(e.ID)})
	}

	if keepErr := s.keep(ctx, m.Scope, kept); keepErr != nil {
		return nil, keepErr
	}

	return out, nil
}

type compiledRule struct {
	pattern *pattern.Pattern
	name    string
	arn     string
	targets []target
}

// enabledRules loads and compiles the enabled rules of a bus given by name or
// ARN; an awserror reports a bus that does not exist.
func (s *Service) enabledRules(ctx context.Context, scope request.Scope, ref *string) ([]compiledRule, error) {
	bus, err := busName(scope, ref)
	if err != nil {
		return nil, busNotFound(common.Value(ref))
	}

	var rules []compiledRule
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if busErr := checkBus(tx, scope, bus); busErr != nil {
			return busErr
		}

		for _, k := range tx.Keys(rulePrefix(scope, bus)) {
			r, _, loadErr := storage.Read[rule](tx, k)
			if loadErr != nil {
				return loadErr
			}

			// A rule with only a schedule matches no events.
			if r.State != stateEnabled || r.Pattern == "" || len(r.Targets) == 0 {
				continue
			}

			p, compileErr := compilePattern(r.Pattern)
			if compileErr != nil {
				return compileErr
			}

			rules = append(rules, compiledRule{
				pattern: p,
				name:    r.Name,
				arn:     ruleARN(scope, bus, r.Name),
				targets: r.Targets,
			})
		}

		return nil
	})

	return rules, err
}

// render returns what a target receives: its constant Input, the value at its
// InputPath, its InputTransformer's output, or the whole event.
func render(r *compiledRule, t *target, e *event, encoded []byte) []byte {
	switch {
	case t.Input != "":
		return []byte(t.Input)
	case t.Transformer != nil:
		return t.Transformer.render(r, e, encoded)
	case t.InputPath == "" || t.InputPath == "$":
		return encoded
	}

	selected, _ := json.Marshal(lookup(decodeEvent(encoded), t.InputPath))
	return selected
}

// decodeEvent decodes an encoded event, keeping numbers as written.
func decodeEvent(encoded []byte) any {
	var v any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	_ = decoder.Decode(&v)
	return v
}

// deliver sends an event to one target in scope. As in EventBridge, a failed
// delivery (such as a deleted queue or function) does not fail PutEvents; the
// delivery log records it, and it is retried or sent to the target's
// dead-letter queue (retry.go).
func (s *Service) deliver(
	ctx context.Context,
	scope request.Scope,
	r *compiledRule,
	t *target,
	e *event,
	encoded []byte,
) {
	d := &pending{
		Target:  *t,
		Rule:    r.arn,
		EventID: e.ID,
		Payload: string(render(r, t, e, encoded)),
		First:   s.now().UnixMilli(),
	}
	if err := s.attempt(ctx, scope, d); err != nil {
		s.failed(ctx, retryKey(scope), d, err)
	}
}

// send makes one delivery of payload to a target.
func (s *Service) send(ctx context.Context, t *target, eventID string, payload []byte) error {
	var err error
	switch strings.SplitN(t.ARN, ":", 4)[2] {
	case serviceSQS:
		entry := &sqsapi.SendMessageBatchRequestEntry{MessageBody: new(string(payload))}
		if t.MessageGroupID != "" {
			entry.MessageGroupId = new(t.MessageGroupID)
			entry.MessageDeduplicationId = new(eventID)
		}

		err = s.queues.Deliver(ctx, t.ARN, entry)
	case serviceSNS:
		_, err = s.topics.Publish(ctx, &snsapi.PublishInput{TopicArn: new(t.ARN), Message: new(string(payload))})
	case serviceLambda:
		err = s.functions.InvokeAsync(ctx, t.ARN, payload)
	}

	return err
}
