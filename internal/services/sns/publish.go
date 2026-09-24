package sns

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxMessage    = 262144
	maxSubject    = 100
	maxAttributes = 10
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
		case "String", "Number":
			if value(a.StringValue) == "" || a.BinaryValue != nil {
				return invalid("Message attribute %s must contain a non-empty string value", name)
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
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	key, _, err := resolve(ctx, in.TopicArn, false)
	if err != nil {
		return nil, err
	}

	message := value(in.Message)
	if message == "" {
		return nil, invalid("Empty message")
	}

	if !utf8.ValidString(message) {
		return nil, invalid("Message must be valid UTF-8")
	}

	size := len(message)
	for name, a := range in.MessageAttributes {
		size += len(name) + len(value(a.DataType)) + len(value(a.StringValue)) + len(a.BinaryValue)
	}

	if size > maxMessage {
		return nil, invalid("Message too long")
	}

	if subject := value(in.Subject); len(subject) > maxSubject || strings.ContainsAny(subject, "\r\n") {
		return nil, invalid("Subject must be at most 100 characters on one line")
	}

	if guardErr := checkAttributes(in.MessageAttributes); guardErr != nil {
		return nil, guardErr
	}

	textFor, err := messages(message, value(in.MessageStructure))
	if err != nil {
		return nil, err
	}

	var subs []subscription
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, loadErr := load(tx, key)
		if loadErr == nil {
			subs = t.Subscriptions
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	id := newID()
	arn := keyARN(key)
	envelope := notification{
		Type:      "Notification",
		MessageID: id,
		TopicArn:  arn,
		Subject:   value(in.Subject),
		Timestamp: s.now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	for name, a := range in.MessageAttributes {
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
		subARN := arn + ":" + sub.ID
		envelope.Message = textFor(sub.Protocol)
		envelope.UnsubscribeURL = m.Endpoint + "/?Action=Unsubscribe&SubscriptionArn=" + url.QueryEscape(subARN)
		// As in SNS, a failed delivery (such as a deleted queue or function)
		// does not fail Publish; the delivery log records it.
		if sub.Protocol == protocolLambda {
			event, encodeErr := json.Marshal(lambdaEvent{Records: []lambdaRecord{{
				EventSource:          "aws:sns",
				EventVersion:         "1.0",
				EventSubscriptionArn: subARN,
				Sns:                  envelope.forLambda(in.Subject),
			}}})
			if encodeErr != nil {
				return nil, encodeErr
			}

			err := s.functions.InvokeAsync(ctx, sub.Endpoint, event)
			s.deliveries.Add(delivery.ViaSNS, arn, sub.Endpoint, event, err)
			continue
		}

		body := envelope.Message
		var attributes map[string]sqsapi.MessageAttributeValue
		if sub.Raw {
			attributes = sqsAttributes(in.MessageAttributes)
		} else {
			encoded, encodeErr := json.Marshal(envelope)
			if encodeErr != nil {
				return nil, encodeErr
			}

			body = string(encoded)
		}

		err := s.queues.Deliver(ctx, sub.Endpoint, &sqsapi.SendMessageBatchRequestEntry{
			MessageBody:       &body,
			MessageAttributes: attributes,
		})
		s.deliveries.Add(delivery.ViaSNS, arn, sub.Endpoint, []byte(body), err)
	}

	return &api.PublishResponse{MessageId: ptr(id)}, nil
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
