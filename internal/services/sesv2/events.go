package sesv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
)

// The event types messages produce.
const (
	eventSend             = "SEND"
	eventDelivery         = "DELIVERY"
	eventBounce           = "BOUNCE"
	eventComplaint        = "COMPLAINT"
	eventRenderingFailure = "RENDERING_FAILURE"
)

// eventNames are each event type's eventType in an event, its member there,
// and its EventBridge detail-type.
var eventNames = map[string][3]string{
	eventSend:             {"Send", "send", "Email Sent"},
	eventDelivery:         {"Delivery", "delivery", "Email Delivered"},
	eventBounce:           {"Bounce", "bounce", "Email Bounced"},
	eventComplaint:        {"Complaint", "complaint", "Email Complaint Received"},
	eventRenderingFailure: {"Rendering Failure", "failure", "Email Rendering Failed"},
}

// keyTimestamp is the time member of events.
const keyTimestamp = "timestamp"

// simulator is the domain of SES's mailbox simulator, whose addresses
// produce the outcome their local part names.
const simulator = "simulator.amazonses.com"

const reportingMTA = "a8-70.smtp-out.amazonses.com"

// dispatch records an outbox's messages and publishes their events.
func (s *Service) dispatch(ctx context.Context, o *outbox) {
	now := s.now().UTC()
	for _, e := range o.emails {
		payload, err := json.Marshal(e)
		if err != nil {
			continue
		}

		var failure error
		if e.failure != "" {
			failure = errors.New(e.failure)
		}

		s.deliveries.Add(delivery.ViaSES, o.identityARN, strings.Join(e.recipients(), ", "), payload, failure)
		if len(o.destinations) == 0 {
			continue
		}

		m := o.mail(e, now)
		stamp := now.Format(time.RFC3339Nano)
		s.publish(ctx, o, eventSend, m, map[string]any{})
		if e.failure != "" {
			s.publish(ctx, o, eventRenderingFailure, m, map[string]any{
				"errorMessage": e.failure, "templateName": e.Template,
			})
			continue
		}

		var delivered []string
		for _, r := range e.recipients() {
			local, kind := simulated(r)
			switch local {
			case "bounce", "suppressionlist":
				subtype := "General"
				if local == "suppressionlist" {
					subtype = "OnAccountSuppressionList"
				}

				s.publish(ctx, o, eventBounce, m, map[string]any{
					"bounceType": "Permanent", "bounceSubType": subtype, keyTimestamp: stamp,
					"feedbackId": messageID(now.UnixMilli()), "reportingMTA": "dsn; " + reportingMTA,
					"bouncedRecipients": []any{map[string]any{
						"emailAddress": kind, "action": "failed", "status": "5.1.1",
						"diagnosticCode": "smtp; 550 5.1.1 user unknown",
					}},
				})
				continue
			case "complaint":
				s.publish(ctx, o, eventComplaint, m, map[string]any{
					"complainedRecipients": []any{map[string]any{"emailAddress": kind}}, keyTimestamp: stamp,
					"feedbackId": messageID(now.UnixMilli()), "complaintFeedbackType": "abuse",
					"userAgent": "Amazon SES Mailbox Simulator",
				})
			}

			delivered = append(delivered, kind)
		}

		if len(delivered) > 0 {
			s.publish(ctx, o, eventDelivery, m, map[string]any{
				keyTimestamp: stamp, "processingTimeMillis": 0, "recipients": delivered,
				"smtpResponse": "250 2.6.0 Message received", "reportingMTA": reportingMTA,
			})
		}
	}
}

// simulated returns the mailbox simulator outcome a recipient asks for
// (success for any other address) and its bare address.
func simulated(recipient string) (outcome, address string) {
	a, err := mail.ParseAddress(recipient)
	if err != nil {
		return "success", recipient
	}

	local, domain, _ := strings.Cut(a.Address, "@")
	if !strings.EqualFold(domain, simulator) {
		return "success", a.Address
	}

	// Labels such as bounce+label@ reach the same simulated mailbox.
	local, _, _ = strings.Cut(strings.ToLower(local), "+")
	return local, a.Address
}

// mail is the mail object of a message's events.
func (o *outbox) mail(e *email, now time.Time) map[string]any {
	source := e.From
	if a, err := mail.ParseAddress(e.From); err == nil {
		source = a.Address
	}

	_, domain, _ := strings.Cut(source, "@")
	tags := map[string][]string{"ses:operation": {o.operation}, "ses:from-domain": {domain}}
	if o.set != "" {
		tags["ses:configuration-set"] = []string{o.set}
	}

	for k, v := range e.Tags {
		tags[k] = []string{v}
	}

	headers := map[string]any{"from": []string{e.From}, "subject": e.Subject, "messageId": e.ID}
	if len(e.To) > 0 {
		headers["to"] = e.To
	}

	if len(e.Cc) > 0 {
		headers["cc"] = e.Cc
	}

	return map[string]any{
		keyTimestamp: now.Format(time.RFC3339Nano), "source": e.From, "sourceArn": o.identityARN,
		"sendingAccountId": o.scope.AccountID, "messageId": e.ID, "destination": e.recipients(),
		"headersTruncated": false, "commonHeaders": headers, "tags": tags,
	}
}

// publish sends an event to the configuration set's destinations that
// match it.
func (s *Service) publish(ctx context.Context, o *outbox, kind string, m, detail map[string]any) {
	names := eventNames[kind]
	body, err := json.Marshal(map[string]any{"eventType": names[0], "mail": m, names[1]: detail})
	if err != nil {
		return
	}

	for _, d := range o.destinations {
		if !common.Value(d.Enabled) || !slices.Contains(d.MatchingEventTypes, kind) {
			continue
		}

		target, sendErr := s.deliver(ctx, o, d, names[2], body)
		s.deliveries.Add(delivery.ViaSES, arn(o.scope, kindConfigSet, o.set), target, body, sendErr)
	}
}

// deliver sends an event to a destination through the caller, in the
// destination's region.
func (s *Service) deliver(ctx context.Context, o *outbox, d api.EventDestination, detailType string, body []byte) (
	string, error,
) {
	var service, operation, target string
	var params map[string]any
	switch {
	case d.SnsDestination != nil:
		target = common.Value(d.SnsDestination.TopicArn)
		service, operation = "sns", "Publish"
		params = map[string]any{"TopicArn": target, "Message": string(body)}
	case d.EventBridgeDestination != nil:
		target = common.Value(d.EventBridgeDestination.EventBusArn)
		service, operation = "eventbridge", "PutEvents"
		params = map[string]any{"Entries": []any{map[string]any{
			"EventBusName": target, "Source": "aws.ses", "DetailType": detailType, "Detail": string(body),
			"Resources": []any{arn(o.scope, kindConfigSet, o.set)},
		}}}
	default:
		target = common.Value(common.OrElse(d.KinesisFirehoseDestination, &api.KinesisFirehoseDestination{}).
			DeliveryStreamArn)
		_, name, _ := strings.Cut(target, ":deliverystream/")
		service, operation = "firehose", "PutRecord"
		params = map[string]any{"DeliveryStreamName": name, "Record": map[string]any{"Data": string(body) + "\n"}}
	}

	if s.caller == nil {
		return target, fmt.Errorf("sesv2: no caller to reach %s", service)
	}

	scope := o.scope
	if parts := strings.Split(target, ":"); len(parts) > 4 && parts[3] != "" {
		scope.Region = parts[3]
	}

	out, err := s.caller.Call(request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: scope}),
		service, operation, params)
	if failed, _ := out["FailedEntryCount"].(float64); err == nil && failed > 0 {
		err = fmt.Errorf("the event bus rejected the event: %v", out["Entries"])
	}

	return target, err
}
