package sns

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	timestampFormat = "2006-01-02T15:04:05.000Z"
	maxMessage      = 262144
	maxSubject      = 100
	maxAttributes   = 10
)

var attributeName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)

// notification is the JSON envelope SNS delivers to SQS unless the
// subscription uses raw message delivery. TinyStack does not sign messages, so
// the signature fields are omitted.
type notification struct {
	MessageAttributes map[string]notificationAttribute `json:",omitempty"`
	Type              string
	MessageID         string `json:"MessageId"`
	TopicArn          string
	Subject           string `json:",omitempty"`
	Message           string
	Timestamp         string
	SequenceNumber    string `json:",omitempty"`
	UnsubscribeURL    string
}

type notificationAttribute struct {
	Type  string
	Value string
}

// lambdaEvent is the event SNS passes to subscribed functions.
type lambdaEvent struct {
	Records []lambdaRecord
}

type lambdaRecord struct {
	EventSource          string
	EventVersion         string
	EventSubscriptionArn string
	Sns                  lambdaNotification
}

// lambdaNotification is the notification inside a Lambda event. Unlike the SQS
// envelope it always carries MessageAttributes and a Subject (null when unset),
// and it spells UnsubscribeUrl in mixed case. Signatures are empty.
type lambdaNotification struct {
	MessageAttributes map[string]notificationAttribute
	Subject           *string
	Type              string
	MessageID         string `json:"MessageId"`
	TopicArn          string
	Message           string
	Timestamp         string
	SignatureVersion  string
	Signature         string
	SigningCertURL    string `json:"SigningCertUrl"`
	UnsubscribeURL    string `json:"UnsubscribeUrl"`
}

// checkAttributes validates message attributes against the SNS data types.
func checkAttributes(attributes map[string]api.MessageAttributeValue) error {
	if len(attributes) > maxAttributes {
		return invalid("MessageAttributes: at most %d attributes are allowed", maxAttributes)
	}

	for name, a := range attributes {
		lower := strings.ToLower(name)
		if !attributeName.MatchString(name) || strings.HasPrefix(lower, "aws.") ||
			strings.HasPrefix(lower, "amazon.") || strings.Contains(name, "..") {
			return invalid("Message attribute name %s is invalid", name)
		}

		base, _, _ := strings.Cut(value(a.DataType), ".")
		switch base {
		case typeString, typeNumber:
			if value(a.StringValue) == "" || a.BinaryValue != nil {
				return invalid("Message attribute %s must contain a non-empty string value", name)
			}

			if _, err := strconv.ParseFloat(value(a.StringValue), 64); base == typeNumber && err != nil {
				return invalid("Could not cast message attribute '%s' value to number.", name)
			}
		case "Binary":
			if len(a.BinaryValue) == 0 || a.StringValue != nil {
				return invalid("Message attribute %s must contain a non-empty binary value", name)
			}
		default:
			return invalid("The message attribute %s has an invalid message attribute type", name)
		}
	}

	return nil
}

// messages selects the text subscribers of each protocol receive. With
// MessageStructure "json" the message is a JSON object holding a "default"
// text and optional per-protocol overrides such as "sqs" or "lambda".
func messages(message, structure string) (func(protocol string) string, error) {
	switch structure {
	case "":
		return func(string) string { return message }, nil
	case "json":
		var texts map[string]any
		if err := json.Unmarshal([]byte(message), &texts); err != nil {
			return nil, invalid("Message Structure - JSON message body failed to parse")
		}

		text, ok := texts["default"].(string)
		if !ok {
			return nil, invalid("Message Structure - No default entry in JSON message body")
		}

		return func(protocol string) string {
			if override, isString := texts[protocol].(string); isString {
				return override
			}

			return text
		}, nil
	default:
		return nil, invalid("MessageStructure must be json")
	}
}

func (s *Service) Publish(ctx context.Context, in *api.PublishInput) (*api.PublishResponse, error) {
	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	id, sequence, err := s.publish(ctx, key, &api.PublishBatchRequestEntry{
		Message: in.Message, Subject: in.Subject, MessageStructure: in.MessageStructure,
		MessageAttributes: in.MessageAttributes, MessageGroupId: in.MessageGroupId,
		MessageDeduplicationId: in.MessageDeduplicationId,
	})
	if err != nil {
		return nil, err
	}

	return &api.PublishResponse{MessageId: new(id), SequenceNumber: sequence}, nil
}

// checkMessage validates a message to publish and returns the text each
// protocol's subscribers receive.
func checkMessage(e *api.PublishBatchRequestEntry) (func(protocol string) string, error) {
	message := value(e.Message)
	if message == "" {
		return nil, invalid("Empty message")
	}

	if !utf8.ValidString(message) {
		return nil, invalid("Message must be valid UTF-8")
	}

	if messageSize(e) > maxMessage {
		return nil, invalid("Message too long")
	}

	if subject := value(e.Subject); len(subject) > maxSubject || strings.ContainsAny(subject, "\r\n") {
		return nil, invalid("Subject must be at most 100 characters on one line")
	}

	if err := checkAttributes(e.MessageAttributes); err != nil {
		return nil, err
	}

	return messages(message, value(e.MessageStructure))
}

// messageSize is a message's size as SNS limits it: its text and attributes.
func messageSize(e *api.PublishBatchRequestEntry) int {
	size := len(value(e.Message))
	for name, a := range e.MessageAttributes {
		size += len(name) + len(value(a.DataType)) + len(value(a.StringValue)) + len(a.BinaryValue)
	}

	return size
}

// publish sends one message to the subscribers of the topic stored under key
// and returns its message ID and, for a FIFO topic, its sequence number. A
// FIFO topic's duplicate returns the first message's, and is not delivered.
func (s *Service) publish(
	ctx context.Context,
	key string,
	e *api.PublishBatchRequestEntry,
) (id string, sequence *string, err error) {
	m, err := identity(ctx)
	if err != nil {
		return "", nil, err
	}

	textFor, err := checkMessage(e)
	if err != nil {
		return "", nil, err
	}

	id = newID()
	now := s.now()
	var subs []subscription
	var placed *sequenced
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		subs = t.Subscriptions
		var placeErr error
		if placed, placeErr = t.place(e, id, now); placeErr != nil || placed == nil || placed.Duplicate {
			return placeErr
		}

		return save(tx, key, t)
	})
	if err != nil {
		return "", nil, err
	}

	envelope := notification{
		Type:      "Notification",
		MessageID: id,
		TopicArn:  keyARN(key),
		Subject:   value(e.Subject),
		Timestamp: now.UTC().Format(timestampFormat),
	}
	if placed != nil {
		if placed.Duplicate {
			return placed.ID, &placed.Number, nil
		}

		sequence = &placed.Number
		envelope.SequenceNumber = placed.Number
	}

	for name, a := range e.MessageAttributes {
		if envelope.MessageAttributes == nil {
			envelope.MessageAttributes = map[string]notificationAttribute{}
		}

		v := value(a.StringValue)
		if a.BinaryValue != nil {
			v = base64.StdEncoding.EncodeToString(a.BinaryValue)
		}

		envelope.MessageAttributes[name] = notificationAttribute{Type: value(a.DataType), Value: v}
	}

	for i := range subs {
		sub := &subs[i]
		if sub.Pending {
			continue
		}

		subARN := envelope.TopicArn + ":" + sub.ID
		envelope.Message = textFor(sub.Protocol)
		accepted, filterErr := sub.accepts(e.MessageAttributes, envelope.Message)
		if filterErr != nil {
			return "", nil, filterErr
		}

		// A message the filter policy rejects is not delivered, and not logged.
		if !accepted {
			continue
		}

		envelope.UnsubscribeURL = m.Endpoint + "/?Action=Unsubscribe&SubscriptionArn=" + url.QueryEscape(subARN)
		if deliverErr := s.deliver(ctx, sub, subARN, &envelope, e, placed); deliverErr != nil {
			return "", nil, deliverErr
		}
	}

	return id, sequence, nil
}

// deliver sends a notification to one subscriber. As in SNS, a failed
// delivery (such as a deleted queue or function) does not fail Publish; the
// delivery log records it.
func (s *Service) deliver(
	ctx context.Context,
	sub *subscription,
	subARN string,
	envelope *notification,
	e *api.PublishBatchRequestEntry,
	placed *sequenced,
) error {
	if confirmable(sub.Protocol) {
		return s.deliverEndpoint(sub, subARN, envelope)
	}

	if sub.Protocol == protocolLambda {
		event, err := json.Marshal(lambdaEvent{Records: []lambdaRecord{{
			EventSource:          "aws:sns",
			EventVersion:         "1.0",
			EventSubscriptionArn: subARN,
			Sns:                  envelope.forLambda(e.Subject),
		}}})
		if err != nil {
			return err
		}

		err = s.functions.InvokeAsync(ctx, sub.Endpoint, event)
		s.deliveries.Add(delivery.ViaSNS, envelope.TopicArn, sub.Endpoint, event, err)
		return nil
	}

	body := envelope.Message
	var attributes map[string]sqsapi.MessageAttributeValue
	if sub.Raw {
		attributes = sqsAttributes(e.MessageAttributes)
	} else {
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return err
		}

		body = string(encoded)
	}

	entry := &sqsapi.SendMessageBatchRequestEntry{MessageBody: &body, MessageAttributes: attributes}
	// A FIFO queue keeps the topic's groups and deduplicates as the topic does.
	if placed != nil && strings.HasSuffix(sub.Endpoint, fifoSuffix) {
		entry.MessageGroupId, entry.MessageDeduplicationId = &placed.Group, &placed.Dedup
	}

	err := s.queues.Deliver(ctx, sub.Endpoint, entry)
	s.deliveries.Add(delivery.ViaSNS, envelope.TopicArn, sub.Endpoint, []byte(body), err)
	return nil
}

// forLambda renders the notification as a Lambda event carries it.
func (n *notification) forLambda(subject *string) lambdaNotification {
	attributes := n.MessageAttributes
	if attributes == nil {
		attributes = map[string]notificationAttribute{}
	}

	return lambdaNotification{
		MessageAttributes: attributes,
		Subject:           subject,
		Type:              n.Type,
		MessageID:         n.MessageID,
		TopicArn:          n.TopicArn,
		Message:           n.Message,
		Timestamp:         n.Timestamp,
		SignatureVersion:  "1",
		UnsubscribeURL:    n.UnsubscribeURL,
	}
}

// sqsAttributes forwards message attributes on raw delivery.
func sqsAttributes(attributes map[string]api.MessageAttributeValue) map[string]sqsapi.MessageAttributeValue {
	if len(attributes) == 0 {
		return nil
	}

	out := make(map[string]sqsapi.MessageAttributeValue, len(attributes))
	for name, a := range attributes {
		out[name] = sqsapi.MessageAttributeValue{
			DataType:    a.DataType,
			StringValue: a.StringValue,
			BinaryValue: a.BinaryValue,
		}
	}

	return out
}
