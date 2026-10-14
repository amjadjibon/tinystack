package delivery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

const (
	channelEmail      = "email"
	channelSMS        = "sms"
	serviceCognitoIDP = "cognito-idp"
)

var e164Phone = regexp.MustCompile(`^\+[1-9]\d{4,14}$`)

// Message represents an outbound communication (email, SMS, or Cognito verification code).
type Message struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Channel string    `json:"channel"`
	Service string    `json:"service"`
	Source  string    `json:"source,omitempty"`
	To      string    `json:"to"`
	Subject string    `json:"subject,omitempty"`
	Body    string    `json:"body,omitempty"`
	Account string    `json:"account,omitempty"`
	Region  string    `json:"region,omitempty"`
}

type sesPayload struct {
	MessageID string   `json:"messageId"`
	From      string   `json:"from"`
	Subject   string   `json:"subject"`
	Text      string   `json:"text,omitempty"`
	HTML      string   `json:"html,omitempty"`
	Raw       string   `json:"raw,omitempty"`
	To        []string `json:"to,omitempty"`
	Cc        []string `json:"cc,omitempty"`
	Bcc       []string `json:"bcc,omitempty"`
}

type cognitoPayload struct {
	UserPoolID  string `json:"userPoolId"`
	Username    string `json:"username"`
	Destination string `json:"destination"`
	Subject     string `json:"subject,omitempty"`
	Message     string `json:"message"`
	Code        string `json:"code"`
}

func parseOutboundMessage(r *Record) (*Message, *sesPayload, bool) {
	switch r.Via {
	case ViaSES:
		var p sesPayload
		if err := json.Unmarshal([]byte(r.Payload), &p); err != nil || (p.MessageID == "" && p.From == "") {
			return nil, nil, false
		}

		body := p.Text
		if body == "" {
			body = p.HTML
		}
		if body == "" {
			body = p.Raw
		}

		msg := &Message{
			ID:      p.MessageID,
			Time:    r.Time,
			Channel: channelEmail,
			Service: ViaSES,
			Source:  p.From,
			To:      r.Target,
			Subject: p.Subject,
			Body:    body,
		}

		if a, err := arn.Parse(r.Source); err == nil {
			msg.Account = a.AccountID
			msg.Region = a.Region
		}

		return msg, &p, true

	case ViaSNS:
		if !e164Phone.MatchString(r.Target) {
			return nil, nil, false
		}

		sum := sha256.Sum256([]byte(r.Time.Format(time.RFC3339Nano) + r.Target + r.Payload))
		msg := &Message{
			ID:      fmt.Sprintf("sms-%x", sum[:12]),
			Time:    r.Time,
			Channel: channelSMS,
			Service: ViaSNS,
			Source:  r.Source,
			To:      r.Target,
			Body:    r.Payload,
		}

		if a, err := arn.Parse(r.Source); err == nil {
			msg.Account = a.AccountID
			msg.Region = a.Region
		}

		return msg, nil, true

	case ViaCognito:
		var p cognitoPayload
		if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
			return nil, nil, false
		}

		channel := channelEmail
		to := p.Destination
		if strings.HasPrefix(r.Target, "email:") {
			channel = channelEmail
			if to == "" {
				to = strings.TrimPrefix(r.Target, "email:")
			}
		} else if strings.HasPrefix(r.Target, "sms:") {
			channel = channelSMS
			if to == "" {
				to = strings.TrimPrefix(r.Target, "sms:")
			}
		}

		sum := sha256.Sum256([]byte(r.Time.Format(time.RFC3339Nano) + r.Target + r.Payload))
		msg := &Message{
			ID:      fmt.Sprintf("cognito-%x", sum[:12]),
			Time:    r.Time,
			Channel: channel,
			Service: serviceCognitoIDP,
			Source:  r.Source,
			To:      to,
			Subject: p.Subject,
			Body:    p.Message,
		}

		if a, err := arn.Parse(r.Source); err == nil {
			msg.Account = a.AccountID
			msg.Region = a.Region
		}

		return msg, nil, true

	default:
		return nil, nil, false
	}
}

func isOutbound(r *Record) bool {
	_, _, ok := parseOutboundMessage(r)
	return ok
}

// matchesTo reports whether query is one of a message's comma-separated recipients.
func matchesTo(target, query string) bool {
	for part := range strings.SplitSeq(target, ",") {
		if strings.EqualFold(strings.TrimSpace(part), query) {
			return true
		}
	}

	return false
}

// MessagesHandler serves GET and DELETE /_tinystack/messages.
func MessagesHandler(log *Log) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			query := r.URL.Query()
			for param := range query {
				if param != "channel" && param != "to" && param != "service" && param != "since" {
					http.Error(w, fmt.Sprintf("unsupported query parameter %q", param), http.StatusBadRequest)
					return
				}
			}

			qChannel := query.Get("channel")
			qTo := query.Get("to")
			if strings.HasPrefix(qTo, " ") {
				qTo = "+" + strings.TrimPrefix(qTo, " ")
			}

			qService := query.Get("service")
			if qChannel != "" && qChannel != channelEmail && qChannel != channelSMS {
				http.Error(w, "channel must be email or sms", http.StatusBadRequest)
				return
			}

			if qService != "" && qService != ViaSES && qService != ViaSNS && qService != serviceCognitoIDP {
				http.Error(w, "service must be ses, sns or cognito-idp", http.StatusBadRequest)
				return
			}

			qSince := query.Get("since")

			var sinceTime time.Time
			if qSince != "" {
				var err error
				sinceTime, err = time.Parse(time.RFC3339, qSince)
				if err != nil {
					sinceTime, err = time.Parse(time.RFC3339, strings.Replace(qSince, " ", "+", 1))
				}
				if err != nil {
					sinceTime, err = time.Parse(time.RFC3339Nano, qSince)
				}
				if err != nil {
					http.Error(
						w,
						fmt.Sprintf("invalid since parameter %q: must be RFC 3339", qSince),
						http.StatusBadRequest,
					)
					return
				}
			}

			records := log.List()
			out := make([]Message, 0)
			for i := range records {
				rec := &records[i]
				msg, _, ok := parseOutboundMessage(rec)
				if !ok {
					continue
				}

				if qChannel != "" && msg.Channel != qChannel {
					continue
				}

				if qTo != "" && !matchesTo(msg.To, qTo) {
					continue
				}

				if qService != "" && msg.Service != qService {
					continue
				}

				if !sinceTime.IsZero() && msg.Time.Before(sinceTime) {
					continue
				}

				out = append(out, *msg)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string][]Message{"messages": out})

		case http.MethodDelete:
			log.Retain(func(rec Record) bool {
				return !isOutbound(&rec)
			})
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// LocalStackSESMessage represents LocalStack's SES inspection item.
type LocalStackSESMessage struct {
	Id          string              `json:"Id"`
	Region      string              `json:"Region"`
	Destination map[string][]string `json:"Destination"`
	Source      string              `json:"Source"`
	Subject     string              `json:"Subject"`
	Body        LocalStackSESBody   `json:"Body"`
	Timestamp   string              `json:"Timestamp"`
}

type LocalStackSESBody struct {
	TextPart string `json:"text_part"`
	HTMLPart string `json:"html_part"`
}

// SESMessagesHandler serves LocalStack-compatible GET /_aws/ses.
func SESMessagesHandler(log *Log) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		qID := r.URL.Query().Get("id")
		qEmail := r.URL.Query().Get("email")

		records := log.List()
		messages := make([]LocalStackSESMessage, 0)
		for i := range records {
			rec := &records[i]
			msg, ses, ok := parseOutboundMessage(rec)
			if !ok || msg.Service != ViaSES || ses == nil {
				continue
			}

			if qID != "" && msg.ID != qID {
				continue
			}

			toAddrs := ses.To
			if len(toAddrs) == 0 && msg.To != "" {
				toAddrs = []string{msg.To}
			}

			if qEmail != "" {
				matched := false
				for _, addr := range toAddrs {
					if strings.EqualFold(addr, qEmail) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}

			region := msg.Region
			if region == "" {
				region = "us-east-1"
			}

			messages = append(messages, LocalStackSESMessage{
				Id:          msg.ID,
				Region:      region,
				Destination: map[string][]string{"ToAddresses": toAddrs},
				Source:      ses.From,
				Subject:     ses.Subject,
				Body: LocalStackSESBody{
					TextPart: ses.Text,
					HTMLPart: ses.HTML,
				},
				Timestamp: rec.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]LocalStackSESMessage{"messages": messages})
	})
}

// LocalStackSMSItem represents an SMS message in LocalStack's documented shape:
// https://docs.localstack.cloud/references/internal-endpoints/#sns-sms-messages
type LocalStackSMSItem struct {
	MessageAttributes map[string]any `json:"MessageAttributes"`
	MessageStructure  *string        `json:"MessageStructure"`
	Subject           *string        `json:"Subject"`
	Message           string         `json:"Message"`
}

// SMSMessagesHandler serves LocalStack-compatible GET /_aws/sns/sms-messages.
// LocalStack documentation reference:
// https://docs.localstack.cloud/references/internal-endpoints/#sns-sms-messages
func SMSMessagesHandler(log *Log) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			log.Retain(func(rec Record) bool {
				return rec.Via != ViaSNS || !e164Phone.MatchString(rec.Target)
			})
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		qPhone := r.URL.Query().Get("phoneNumber")
		if strings.HasPrefix(qPhone, " ") {
			qPhone = "+" + strings.TrimPrefix(qPhone, " ")
		}

		qRegion := r.URL.Query().Get("region")

		records := log.List()
		smsMap := make(map[string][]LocalStackSMSItem)
		resolvedRegion := "us-east-1"

		for i := range records {
			rec := &records[i]
			msg, _, ok := parseOutboundMessage(rec)
			if !ok || msg.Service != ViaSNS || msg.Channel != channelSMS {
				continue
			}

			if qPhone != "" && msg.To != qPhone {
				continue
			}

			if qRegion != "" && msg.Region != "" && !strings.EqualFold(msg.Region, qRegion) {
				continue
			}

			if msg.Region != "" {
				resolvedRegion = msg.Region
			}

			item := LocalStackSMSItem{
				Message:           msg.Body,
				MessageAttributes: map[string]any{},
				MessageStructure:  nil,
				Subject:           nil,
			}
			smsMap[msg.To] = append(smsMap[msg.To], item)
		}

		if qRegion != "" {
			resolvedRegion = qRegion
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sms_messages": smsMap,
			"region":       resolvedRegion,
		})
	})
}
