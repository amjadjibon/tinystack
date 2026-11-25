package sns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/mail"
	"net/url"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	protocolHTTP      = "http"
	protocolHTTPS     = "https"
	protocolEmail     = "email"
	protocolEmailJSON = "email-json"
	// pendingARN is what Subscribe and the list operations report for a
	// subscription that is not confirmed.
	pendingARN = "pending confirmation"
	// postTimeout bounds a delivery to an HTTP endpoint, as SNS's 15-second
	// limit does.
	postTimeout = 15 * time.Second
)

// confirmable reports whether a protocol's subscriptions wait for the
// endpoint's owner to confirm them.
func confirmable(protocol string) bool {
	switch protocol {
	case protocolHTTP, protocolHTTPS, protocolEmail, protocolEmailJSON:
		return true
	}

	return false
}

// checkEndpoint checks an HTTP, HTTPS, or email endpoint.
func checkEndpoint(protocol, endpoint string) error {
	switch protocol {
	case protocolHTTP, protocolHTTPS:
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != protocol || u.Host == "" {
			return invalid("%s endpoint must be a URL that starts with %s://", protocol, protocol)
		}
	default:
		if a, err := mail.ParseAddress(endpoint); err != nil || a.Address != endpoint {
			return invalid("Email address")
		}
	}

	return nil
}

func newToken() string {
	b := make([]byte, 64)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// confirmation is the message that asks an endpoint's owner to confirm a
// subscription. TinyStack does not sign messages, so the signature is empty.
type confirmation struct {
	Type             string
	MessageID        string `json:"MessageId"`
	Token            string
	TopicArn         string
	Message          string
	SubscribeURL     string `json:"SubscribeURL"`
	Timestamp        string
	SignatureVersion string
	Signature        string
	SigningCertURL   string `json:"SigningCertURL"`
}

// requestConfirmation sends a pending subscription's confirmation request: an
// HTTP POST, or for email, a delivery log record holding the message.
func (s *Service) requestConfirmation(endpoint, topicARN string, sub *subscription) error {
	c := confirmation{
		Type: "SubscriptionConfirmation", MessageID: newID(), Token: sub.Token, TopicArn: topicARN,
		Message: "You have chosen to subscribe to the topic " + topicARN + ".\nTo confirm the subscription, " +
			"visit the SubscribeURL included in this message.",
		SubscribeURL: endpoint + "/?Action=ConfirmSubscription&TopicArn=" + url.QueryEscape(topicARN) +
			"&Token=" + sub.Token,
		Timestamp: s.now().UTC().Format(timestampFormat), SignatureVersion: "1",
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}

	if sub.Protocol == protocolEmail || sub.Protocol == protocolEmailJSON {
		if s.deliveries != nil {
			s.deliveries.Add(delivery.ViaSNS, topicARN, sub.Endpoint, body, nil)
		}

		return nil
	}

	s.post(topicARN, sub, subscriptionDeliveryPolicy{}, http.Header{
		"X-Amz-Sns-Message-Type": {c.Type}, "X-Amz-Sns-Message-Id": {c.MessageID},
		"X-Amz-Sns-Topic-Arn": {topicARN},
	}, body, nil, nil)
	return nil
}

// post sends a message to an HTTP or HTTPS endpoint in the background, as SNS
// delivers, retrying notifications according to the effective delivery policy
// before recording the outcome and redriving to the subscription DLQ on failure.
func (s *Service) post(
	topicARN string,
	sub *subscription,
	policy subscriptionDeliveryPolicy,
	header http.Header,
	body []byte,
	rawAttrs map[string]api.MessageAttributeValue,
	placed *sequenced,
) {
	subCopy := *sub
	contentType := defaultContentType
	if policy.RequestPolicy != nil && policy.RequestPolicy.HeaderContentType != "" {
		contentType = policy.RequestPolicy.HeaderContentType
	}

	header.Set("Content-Type", contentType)
	header.Set("User-Agent", "Amazon Simple Notification Service Agent")
	endpoint := subCopy.Endpoint
	isNotification := header.Get("X-Amz-Sns-Message-Type") == "Notification"
	maxRetries := 0
	if isNotification && policy.HealthyRetryPolicy != nil && policy.HealthyRetryPolicy.NumRetries != nil {
		maxRetries = *policy.HealthyRetryPolicy.NumRetries
	}

	clonedAttrs := maps.Clone(rawAttrs)
	s.bg.Go(func(ctx context.Context) {
		var err error
		for attempt := 0; attempt <= maxRetries; attempt++ {
			var retryable bool
			err, retryable = attemptHTTPPost(ctx, endpoint, header, body)
			if err == nil || !retryable || attempt == maxRetries {
				break
			}

			wait := retryDelay(policy.HealthyRetryPolicy, attempt)
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
		}

		if s.deliveries != nil {
			s.deliveries.Add(delivery.ViaSNS, topicARN, endpoint, body, err)
		}

		// A confirmation request is not a notification.
		if isNotification {
			s.notified(topicARN, err)
			if err != nil {
				s.redrive(ctx, topicARN, &subCopy, string(body), clonedAttrs, placed, err)
			}
		}
	})
}

func attemptHTTPPost(
	ctx context.Context,
	endpoint string,
	header http.Header,
	body []byte,
) (err error, retryable bool) {
	resp, transportErr := proxy.RoundTrip(ctx, http.MethodPost, endpoint, header, body, postTimeout)
	if transportErr != nil {
		return transportErr, true
	}

	if resp.Status >= 200 && resp.Status <= 299 {
		return nil, false
	}

	return &httpStatusError{status: resp.Status}, resp.Status >= 500 || resp.Status == http.StatusTooManyRequests
}

// deliverEndpoint delivers a notification to a confirmed HTTP, HTTPS, or email
// subscription. Email is recorded in the delivery log, never sent.
func (s *Service) deliverEndpoint(
	sub *subscription,
	subARN string,
	topicDeliveryRaw string,
	envelope *notification,
	e *api.PublishBatchRequestEntry,
	placed *sequenced,
) error {
	body := []byte(envelope.Message)
	if !sub.Raw && sub.Protocol != protocolEmail {
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return err
		}

		body = encoded
	}

	switch sub.Protocol {
	case protocolEmail, protocolEmailJSON:
		if s.deliveries != nil {
			s.deliveries.Add(delivery.ViaSNS, envelope.TopicArn, sub.Endpoint, body, nil)
		}

		s.notified(envelope.TopicArn, nil)
		return nil
	}

	header := http.Header{
		"X-Amz-Sns-Message-Type": {envelope.Type}, "X-Amz-Sns-Message-Id": {envelope.MessageID},
		"X-Amz-Sns-Topic-Arn": {envelope.TopicArn}, "X-Amz-Sns-Subscription-Arn": {subARN},
	}
	if sub.Raw {
		header.Set("X-Amz-Sns-Rawdelivery", valueTrue)
	}

	_, effectivePolicy := effectiveSubscriptionDeliveryPolicy(topicDeliveryRaw, sub.DeliveryPolicy)
	s.post(envelope.TopicArn, sub, effectivePolicy, header, body, e.MessageAttributes, placed)
	return nil
}

// isAuthenticated reports whether a request carries AWS authentication (SigV4
// credentials, an authenticated principal, or an internal/console call).
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func isAuthenticated(m request.Metadata) bool {
	if m.Principal != nil {
		return m.Principal.Kind != request.PrincipalAnonymous
	}

	return m.AccessKey != "" || m.Protocol == "console" || m.Protocol == ""
}

// ConfirmSubscription confirms a pending subscription with the token its
// confirmation request carried. The token is the credential, so the topic may
// belong to another account, as when an endpoint's owner follows the
// SubscribeURL.
func (s *Service) ConfirmSubscription(
	ctx context.Context,
	in *api.ConfirmSubscriptionInput,
) (*api.ConfirmSubscriptionResponse, error) {
	key, err := topicKeyOf(in.TopicArn)
	if err != nil {
		return nil, err
	}

	authUnsub := common.Value(in.AuthenticateOnUnsubscribe)
	if authUnsub != "" && authUnsub != valueTrue && authUnsub != valueFalse {
		return nil, invalid("AuthenticateOnUnsubscribe must be true or false")
	}

	m, _ := request.FromContext(ctx)
	authenticated := isAuthenticated(m)
	token := common.Value(in.Token)
	var id string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, loadErr := load(tx, key)
		if loadErr != nil {
			return loadErr
		}

		for i := range t.Subscriptions {
			sub := &t.Subscriptions[i]
			if token == "" || sub.Token != token {
				continue
			}

			id = sub.ID
			if !sub.Pending {
				return nil
			}

			sub.Pending = false
			sub.ConfirmationWasAuthenticated = authenticated
			sub.AuthenticateOnUnsubscribe = authUnsub == valueTrue && authenticated
			return storage.Write(tx, key, t)
		}

		return invalid("Token")
	})
	if err != nil {
		return nil, err
	}

	return &api.ConfirmSubscriptionResponse{SubscriptionArn: new(keyARN(key) + ":" + id)}, nil
}
