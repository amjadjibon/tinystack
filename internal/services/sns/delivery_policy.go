package sns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const (
	attrDeliveryPolicy          = "DeliveryPolicy"
	attrEffectiveDeliveryPolicy = "EffectiveDeliveryPolicy"
	attrRedrivePolicy           = "RedrivePolicy"
	errDeliveryError            = "DeliveryError"

	defaultMinDelayTarget = 20
	defaultMaxDelayTarget = 20
	defaultNumRetries     = 3
	maxDeliveryRetries    = 100
	maxDelayTargetSeconds = 3600
	defaultContentType    = "text/plain; charset=UTF-8"
	maxDLQErrorMessage    = 1024

	// localRetryStep and maxLocalRetryDelay bound local HTTP retry waits so
	// configured retry policies exercise retries quickly in local tests.
	localRetryStep     = 5 * time.Millisecond
	maxLocalRetryDelay = 25 * time.Millisecond
)

type healthyRetryPolicy struct {
	MinDelayTarget     *int   `json:"minDelayTarget,omitempty"`
	MaxDelayTarget     *int   `json:"maxDelayTarget,omitempty"`
	NumRetries         *int   `json:"numRetries,omitempty"`
	NumMaxDelayRetries *int   `json:"numMaxDelayRetries,omitempty"`
	NumNoDelayRetries  *int   `json:"numNoDelayRetries,omitempty"`
	NumMinDelayRetries *int   `json:"numMinDelayRetries,omitempty"`
	BackoffFunction    string `json:"backoffFunction,omitempty"`
}

type throttlePolicy struct {
	MaxReceivesPerSecond *int `json:"maxReceivesPerSecond,omitempty"`
}

type requestPolicy struct {
	HeaderContentType string `json:"headerContentType,omitempty"`
}

type topicHTTPDeliveryPolicy struct {
	DefaultHealthyRetryPolicy    *healthyRetryPolicy `json:"defaultHealthyRetryPolicy,omitempty"`
	DefaultThrottlePolicy        *throttlePolicy     `json:"defaultThrottlePolicy,omitempty"`
	DefaultRequestPolicy         *requestPolicy      `json:"defaultRequestPolicy,omitempty"`
	DisableSubscriptionOverrides bool                `json:"disableSubscriptionOverrides"`
}

type topicDeliveryPolicy struct {
	HTTP *topicHTTPDeliveryPolicy `json:"http,omitempty"`
}

type subscriptionDeliveryPolicy struct {
	HealthyRetryPolicy *healthyRetryPolicy `json:"healthyRetryPolicy,omitempty"`
	ThrottlePolicy     *throttlePolicy     `json:"throttlePolicy,omitempty"`
	RequestPolicy      *requestPolicy      `json:"requestPolicy,omitempty"`
}

type httpStatusError struct {
	status int
}

func (e *httpStatusError) Error() string {
	return "the endpoint answered HTTP " + strconv.Itoa(e.status)
}

func defaultHealthyRetry() healthyRetryPolicy {
	return healthyRetryPolicy{
		MinDelayTarget:     new(defaultMinDelayTarget),
		MaxDelayTarget:     new(defaultMaxDelayTarget),
		NumRetries:         new(defaultNumRetries),
		NumMaxDelayRetries: new(0),
		NumNoDelayRetries:  new(0),
		NumMinDelayRetries: new(0),
		BackoffFunction:    "linear",
	}
}

func defaultRequestPolicy() *requestPolicy {
	return &requestPolicy{HeaderContentType: defaultContentType}
}

func decodeStrictJSON(raw string, dst any) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}

	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}

	return nil
}

func validateHealthyRetry(p *healthyRetryPolicy) error {
	if p == nil {
		return nil
	}

	minDelay := defaultMinDelayTarget
	if p.MinDelayTarget != nil {
		minDelay = *p.MinDelayTarget
		if minDelay < 1 || minDelay > maxDelayTargetSeconds {
			return invalid("DeliveryPolicy: minDelayTarget must be between 1 and %d", maxDelayTargetSeconds)
		}
	}

	maxDelay := defaultMaxDelayTarget
	if p.MaxDelayTarget != nil {
		maxDelay = *p.MaxDelayTarget
		if maxDelay < 1 || maxDelay > maxDelayTargetSeconds {
			return invalid("DeliveryPolicy: maxDelayTarget must be between 1 and %d", maxDelayTargetSeconds)
		}
	}

	if maxDelay < minDelay {
		return invalid("DeliveryPolicy: maxDelayTarget must be greater than or equal to minDelayTarget")
	}

	numRetries := defaultNumRetries
	if p.NumRetries != nil {
		numRetries = *p.NumRetries
		if numRetries < 0 || numRetries > maxDeliveryRetries {
			return invalid("DeliveryPolicy: numRetries must be between 0 and %d", maxDeliveryRetries)
		}
	}

	noDelay, minRetries, maxRetries := 0, 0, 0
	if p.NumNoDelayRetries != nil {
		noDelay = *p.NumNoDelayRetries
		if noDelay < 0 {
			return invalid("DeliveryPolicy: numNoDelayRetries must be non-negative")
		}
	}

	if p.NumMinDelayRetries != nil {
		minRetries = *p.NumMinDelayRetries
		if minRetries < 0 {
			return invalid("DeliveryPolicy: numMinDelayRetries must be non-negative")
		}
	}

	if p.NumMaxDelayRetries != nil {
		maxRetries = *p.NumMaxDelayRetries
		if maxRetries < 0 {
			return invalid("DeliveryPolicy: numMaxDelayRetries must be non-negative")
		}
	}

	if noDelay+minRetries+maxRetries > numRetries {
		return invalid("DeliveryPolicy: sum of numNoDelayRetries, numMinDelayRetries, and numMaxDelayRetries " +
			"cannot exceed numRetries")
	}

	switch p.BackoffFunction {
	case "", "linear", "arithmetic", "geometric", "exponential":
	default:
		return invalid("DeliveryPolicy: backoffFunction %q is not supported", p.BackoffFunction)
	}

	return nil
}

func validateThrottlePolicy(p *throttlePolicy) error {
	if p == nil || p.MaxReceivesPerSecond == nil {
		return nil
	}

	if *p.MaxReceivesPerSecond < 1 {
		return invalid("DeliveryPolicy: maxReceivesPerSecond must be at least 1")
	}

	return nil
}

func validateRequestPolicy(p *requestPolicy) error {
	if p == nil || p.HeaderContentType == "" {
		return nil
	}

	switch p.HeaderContentType {
	case defaultContentType, "application/json", "application/xml":
		return nil
	default:
		return invalid("DeliveryPolicy: headerContentType %q is not supported", p.HeaderContentType)
	}
}

func mergeHealthyRetry(base healthyRetryPolicy, overlay *healthyRetryPolicy) healthyRetryPolicy {
	if overlay == nil {
		return base
	}

	if overlay.MinDelayTarget != nil {
		base.MinDelayTarget = new(*overlay.MinDelayTarget)
	}

	if overlay.MaxDelayTarget != nil {
		base.MaxDelayTarget = new(*overlay.MaxDelayTarget)
	}

	if overlay.NumRetries != nil {
		base.NumRetries = new(*overlay.NumRetries)
	}

	if overlay.NumMaxDelayRetries != nil {
		base.NumMaxDelayRetries = new(*overlay.NumMaxDelayRetries)
	}

	if overlay.NumNoDelayRetries != nil {
		base.NumNoDelayRetries = new(*overlay.NumNoDelayRetries)
	}

	if overlay.NumMinDelayRetries != nil {
		base.NumMinDelayRetries = new(*overlay.NumMinDelayRetries)
	}

	if overlay.BackoffFunction != "" {
		base.BackoffFunction = overlay.BackoffFunction
	}

	return base
}

func checkTopicDeliveryPolicy(raw string) error {
	if raw == "" {
		return nil
	}

	var p topicDeliveryPolicy
	if err := decodeStrictJSON(raw, &p); err != nil {
		return invalid("DeliveryPolicy: %s", err)
	}

	if p.HTTP == nil {
		return invalid("DeliveryPolicy must contain an http object")
	}

	if err := validateHealthyRetry(p.HTTP.DefaultHealthyRetryPolicy); err != nil {
		return err
	}

	if err := validateThrottlePolicy(p.HTTP.DefaultThrottlePolicy); err != nil {
		return err
	}

	return validateRequestPolicy(p.HTTP.DefaultRequestPolicy)
}

func checkSubscriptionDeliveryPolicy(raw string) error {
	if raw == "" {
		return nil
	}

	var p subscriptionDeliveryPolicy
	if err := decodeStrictJSON(raw, &p); err != nil {
		return invalid("DeliveryPolicy: %s", err)
	}

	if err := validateHealthyRetry(p.HealthyRetryPolicy); err != nil {
		return err
	}

	if err := validateThrottlePolicy(p.ThrottlePolicy); err != nil {
		return err
	}

	return validateRequestPolicy(p.RequestPolicy)
}

func resolveTopicHTTPPolicy(raw string) topicHTTPDeliveryPolicy {
	retry := defaultHealthyRetry()
	out := topicHTTPDeliveryPolicy{
		DefaultHealthyRetryPolicy: &retry,
		DefaultRequestPolicy:      defaultRequestPolicy(),
	}
	if raw == "" {
		return out
	}

	var p topicDeliveryPolicy
	if decodeStrictJSON(raw, &p) != nil || p.HTTP == nil {
		return out
	}

	merged := mergeHealthyRetry(retry, p.HTTP.DefaultHealthyRetryPolicy)
	out.DefaultHealthyRetryPolicy = &merged
	out.DisableSubscriptionOverrides = p.HTTP.DisableSubscriptionOverrides
	if p.HTTP.DefaultThrottlePolicy != nil {
		out.DefaultThrottlePolicy = p.HTTP.DefaultThrottlePolicy
	}

	if p.HTTP.DefaultRequestPolicy != nil && p.HTTP.DefaultRequestPolicy.HeaderContentType != "" {
		out.DefaultRequestPolicy = &requestPolicy{HeaderContentType: p.HTTP.DefaultRequestPolicy.HeaderContentType}
	}

	return out
}

func effectiveTopicDeliveryPolicy(raw string) string {
	httpPolicy := resolveTopicHTTPPolicy(raw)
	b, _ := json.Marshal(topicDeliveryPolicy{HTTP: &httpPolicy})
	return string(b)
}

func effectiveSubscriptionDeliveryPolicy(topicRaw, subRaw string) (string, subscriptionDeliveryPolicy) {
	topicHTTP := resolveTopicHTTPPolicy(topicRaw)
	retry := *topicHTTP.DefaultHealthyRetryPolicy
	out := subscriptionDeliveryPolicy{
		HealthyRetryPolicy: &retry,
		ThrottlePolicy:     topicHTTP.DefaultThrottlePolicy,
		RequestPolicy:      topicHTTP.DefaultRequestPolicy,
	}
	if !topicHTTP.DisableSubscriptionOverrides && subRaw != "" {
		var p subscriptionDeliveryPolicy
		if decodeStrictJSON(subRaw, &p) == nil {
			merged := mergeHealthyRetry(retry, p.HealthyRetryPolicy)
			out.HealthyRetryPolicy = &merged
			if p.ThrottlePolicy != nil {
				out.ThrottlePolicy = p.ThrottlePolicy
			}

			if p.RequestPolicy != nil && p.RequestPolicy.HeaderContentType != "" {
				out.RequestPolicy = &requestPolicy{HeaderContentType: p.RequestPolicy.HeaderContentType}
			}
		}
	}

	b, _ := json.Marshal(out)
	return string(b), out
}

// retryDelay returns a bounded local delay before retry attempt (0-indexed).
func retryDelay(p *healthyRetryPolicy, attempt int) time.Duration {
	if p == nil {
		return localRetryStep
	}

	noDelay := 0
	if p.NumNoDelayRetries != nil {
		noDelay = *p.NumNoDelayRetries
	}

	if attempt < noDelay {
		return 0
	}

	minDelay := defaultMinDelayTarget
	if p.MinDelayTarget != nil && *p.MinDelayTarget > 0 {
		minDelay = *p.MinDelayTarget
	}

	maxDelay := defaultMaxDelayTarget
	if p.MaxDelayTarget != nil && *p.MaxDelayTarget > 0 {
		maxDelay = *p.MaxDelayTarget
	}

	delay := time.Duration(minDelay+attempt) * localRetryStep
	capDelay := min(time.Duration(maxDelay)*localRetryStep, maxLocalRetryDelay)
	return min(delay, capDelay)
}

// checkSQSARN validates an SQS queue ARN.
func checkSQSARN(arn string) error {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != arnScheme || parts[2] != protocolSQS || parts[3] == "" ||
		!accountID.MatchString(parts[4]) || parts[5] == "" {
		return invalid("SQS queue ARN")
	}

	return nil
}

// parseRedrivePolicy validates a subscription RedrivePolicy and returns its
// deadLetterTargetArn. Empty string or "{}" clears the redrive policy.
func parseRedrivePolicy(raw string) (string, error) {
	if raw == "" || raw == "{}" {
		return "", nil
	}

	var policy struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
	}

	if err := decodeStrictJSON(raw, &policy); err != nil {
		return "", invalid("RedrivePolicy: %s", err)
	}

	if policy.DeadLetterTargetArn == "" {
		return "", invalid("RedrivePolicy must include a deadLetterTargetArn")
	}

	if err := checkSQSARN(policy.DeadLetterTargetArn); err != nil {
		return "", invalid("RedrivePolicy deadLetterTargetArn must be a valid SQS queue ARN")
	}

	return policy.DeadLetterTargetArn, nil
}

func deliveryErrorDetails(err error) (code, message string) {
	if err == nil {
		return errDeliveryError, "delivery failed"
	}

	if statusErr, ok := errors.AsType[*httpStatusError](err); ok {
		return strconv.Itoa(statusErr.status), statusErr.Error()
	}

	if awsErr, ok := errors.AsType[*awserror.Error](err); ok {
		code = strings.TrimSuffix(awsErr.Code, "Exception")
		if code == "" {
			code = errDeliveryError
		}

		message = awsErr.Message
		if message == "" {
			message = awsErr.Error()
		}

		return code, truncateUTF8(message, maxDLQErrorMessage)
	}

	return errDeliveryError, truncateUTF8(err.Error(), maxDLQErrorMessage)
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}

	s = s[:maxBytes]
	for s != "" && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}

	return s
}

// redrive delivers a failed notification payload to the subscription's SQS DLQ
// with ErrorCode and ErrorMessage attributes, honoring delivery authorization.
func (s *Service) redrive(
	ctx context.Context,
	topicARN string,
	sub *subscription,
	body string,
	rawAttrs map[string]api.MessageAttributeValue,
	placed *sequenced,
	deliverErr error,
) {
	if sub.RedrivePolicy == "" {
		return
	}

	dlqARN, err := parseRedrivePolicy(sub.RedrivePolicy)
	if err != nil || dlqARN == "" {
		return
	}

	errCode, errMsg := deliveryErrorDetails(deliverErr)
	attrs := map[string]sqsapi.MessageAttributeValue{}
	if sub.Raw && len(rawAttrs) > 0 {
		for name, a := range sqsAttributes(rawAttrs) {
			if len(attrs) >= maxAttributes-2 || name == "ErrorCode" || name == "ErrorMessage" {
				continue
			}

			attrs[name] = a
		}
	}

	attrs["ErrorCode"] = sqsapi.MessageAttributeValue{DataType: new(typeString), StringValue: new(errCode)}
	attrs["ErrorMessage"] = sqsapi.MessageAttributeValue{DataType: new(typeString), StringValue: new(errMsg)}

	entry := &sqsapi.SendMessageBatchRequestEntry{MessageBody: &body, MessageAttributes: attrs}
	if strings.HasSuffix(dlqARN, fifoSuffix) {
		if placed != nil {
			entry.MessageGroupId, entry.MessageDeduplicationId = &placed.Group, &placed.Dedup
		} else {
			entry.MessageGroupId, entry.MessageDeduplicationId = new("sns-dlq"), new(newID())
		}
	}

	if _, ok := request.FromContext(ctx); !ok {
		parts := strings.Split(topicARN, ":")
		ctx = request.WithMetadata(ctx, request.Metadata{
			Scope:   request.Scope{AccountID: parts[4], Region: parts[3]},
			Service: serviceName,
		})
	}

	dlqErr := s.authorize(ctx, topicARN, dlqARN)
	if dlqErr == nil {
		if s.queues == nil {
			dlqErr = fmt.Errorf("SQS service is unavailable")
		} else {
			dlqErr = s.queues.Deliver(ctx, dlqARN, entry)
		}
	}

	if s.deliveries != nil {
		s.deliveries.Add(serviceName, topicARN, dlqARN, []byte(body), dlqErr)
	}

	s.redriven(topicARN, dlqErr)
}
