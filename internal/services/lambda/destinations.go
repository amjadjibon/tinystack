package lambda

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Each asynchronous invocation configuration is
// "invoke-config/<name>/<qualifier>", with $LATEST for the unqualified function.
const (
	kindInvokeConfig = "invoke-config"
	// maxEventAge is Lambda's default and largest MaximumEventAgeInSeconds.
	maxEventAge       = 21600
	minEventAge       = 60
	conditionSuccess  = "Success"
	conditionRetries  = "RetriesExhausted"
	conditionEventAge = "EventAgeExceeded"
	serviceLambda     = "lambda"
	serviceSQS        = "sqs"
	serviceSNS        = "sns"
	// recordVersion is the version of the records Lambda sends functions and
	// destinations.
	recordVersion = "1.0"
)

// invokeConfig is how a function's asynchronous invocations are retried and
// where their results go. Nil limits are Lambda's defaults.
type invokeConfig struct {
	MaximumRetryAttempts     *int64 `json:",omitempty"`
	MaximumEventAgeInSeconds *int64 `json:",omitempty"`
	Qualifier                string
	OnSuccess                string  `json:",omitempty"`
	OnFailure                string  `json:",omitempty"`
	LastModified             float64 // seconds
}

// UseCaller lets asynchronous invocations send their results to SQS, SNS, and
// EventBridge destinations. Call it before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

func invokeConfigKey(scope request.Scope, name, qualifier string) string {
	if qualifier == "" {
		qualifier = latest
	}

	return scope.Key("lambda", kindInvokeConfig, name+"/"+qualifier)
}

// attempts is how often an invocation runs before it fails: the first attempt
// and the retries.
func (c *invokeConfig) attempts() int {
	if c.MaximumRetryAttempts == nil {
		return asyncAttempts
	}

	return int(*c.MaximumRetryAttempts) + 1
}

func (c *invokeConfig) maxAge() time.Duration {
	if c.MaximumEventAgeInSeconds == nil {
		return maxEventAge * time.Second
	}

	return time.Duration(*c.MaximumEventAgeInSeconds) * time.Second
}

func (c *invokeConfig) output(scope request.Scope, name string) *api.FunctionEventInvokeConfig {
	destination := func(arn string) *string {
		if arn == "" {
			return nil
		}

		return new(arn)
	}

	return &api.FunctionEventInvokeConfig{
		LastModified:             new(c.LastModified),
		FunctionArn:              new(qualifiedARN(scope, name, c.Qualifier)),
		MaximumRetryAttempts:     c.MaximumRetryAttempts,
		MaximumEventAgeInSeconds: c.MaximumEventAgeInSeconds,
		DestinationConfig: &api.DestinationConfig{
			OnSuccess: &api.OnSuccess{Destination: destination(c.OnSuccess)},
			OnFailure: &api.OnFailure{Destination: destination(c.OnFailure)},
		},
	}
}

// configure applies a Put or Update request's settings; a nil setting keeps
// the configuration's, and an empty destination removes it.
func (c *invokeConfig) configure(
	scope request.Scope,
	name string,
	retries, age *int64,
	destinations *api.DestinationConfig,
) error {
	if retries != nil {
		if *retries < 0 || *retries > asyncAttempts-1 {
			return invalid("MaximumRetryAttempts must be between 0 and %d", asyncAttempts-1)
		}

		c.MaximumRetryAttempts = retries
	}

	if age != nil {
		if *age < minEventAge || *age > maxEventAge {
			return invalid("MaximumEventAgeInSeconds must be between %d and %d", minEventAge, maxEventAge)
		}

		c.MaximumEventAgeInSeconds = age
	}

	if destinations == nil {
		return nil
	}

	self := functionARN(scope, name)
	for _, d := range []struct {
		arn *string
		to  *string
	}{
		{arn: onSuccess(destinations), to: &c.OnSuccess},
		{arn: onFailure(destinations), to: &c.OnFailure},
	} {
		if d.arn == nil {
			continue
		}

		arn := *d.arn
		if arn != "" {
			if _, ok := destinationService(arn); !ok {
				return invalid("The destination ARN %s is invalid: it must be an SQS queue, SNS topic, Lambda "+
					"function, or EventBridge event bus", arn)
			}

			if name, _, err := resolveName(&request.Metadata{Scope: scope}, arn); err == nil &&
				functionARN(scope, name) == self {
				return invalid("You can't specify the function as a destination for itself.")
			}
		}

		*d.to = arn
	}

	return nil
}

func onSuccess(d *api.DestinationConfig) *string {
	if d.OnSuccess == nil {
		return nil
	}

	return new(common.Value(d.OnSuccess.Destination))
}

func onFailure(d *api.DestinationConfig) *string {
	if d.OnFailure == nil {
		return nil
	}

	return new(common.Value(d.OnFailure.Destination))
}

// destinationResources are the resource prefixes of each destination service's
// ARNs.
var destinationResources = map[string]string{
	serviceSQS: "", serviceSNS: "", serviceLambda: "function:", "events": "event-bus/",
}

// destinationService is the service of a destination ARN.
func destinationService(arn string) (string, bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || !strings.HasPrefix(arn, "arn:") || !accountID.MatchString(parts[4]) || parts[3] == "" {
		return "", false
	}

	resource, ok := destinationResources[parts[2]]
	if !ok || !strings.HasPrefix(parts[5], resource) {
		return "", false
	}

	return parts[2], true
}

// invokeConfigTarget resolves the function and qualifier a request names,
// which must exist.
func invokeConfigTarget(tx storage.ReadTx, m *request.Metadata, raw, qualifier *string) (name, q string, err error) {
	if name, q, err = resolveName(m, common.Value(raw)); err != nil {
		return "", "", err
	}

	if q, err = withQualifier(q, qualifier); err != nil {
		return "", "", err
	}

	if q == "" {
		q = latest
	}

	_, _, err = resolve(tx, m.Scope, name, q)
	return name, q, err
}

func noInvokeConfig(scope request.Scope, name, q string) error {
	return notFound("The function " + qualifiedARN(scope, name, q) + " doesn't have an EventInvokeConfig")
}

// changeInvokeConfig applies a Put (replace) or Update to a configuration.
func (s *Service) changeInvokeConfig(
	ctx context.Context,
	raw, qualifier *string,
	replace bool,
	retries, age *int64,
	destinations *api.DestinationConfig,
) (*api.FunctionEventInvokeConfig, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var c invokeConfig
	var name string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var q string
		var resolveErr error
		if name, q, resolveErr = invokeConfigTarget(tx, &m, raw, qualifier); resolveErr != nil {
			return resolveErr
		}

		key := invokeConfigKey(m.Scope, name, q)
		found, readErr := readJSON(tx, key, &c)
		switch {
		case readErr != nil:
			return readErr
		case replace:
			c = invokeConfig{}
		case !found:
			return noInvokeConfig(m.Scope, name, q)
		}

		c.Qualifier = q
		c.LastModified = float64(s.now().UnixMilli()) / 1000
		if configErr := c.configure(m.Scope, name, retries, age, destinations); configErr != nil {
			return configErr
		}

		return writeJSON(tx, key, &c)
	})
	if err != nil {
		return nil, err
	}

	return c.output(m.Scope, name), nil
}

func (s *Service) PutFunctionEventInvokeConfig(
	ctx context.Context,
	in *api.PutFunctionEventInvokeConfigRequest,
) (*api.FunctionEventInvokeConfig, error) {
	return s.changeInvokeConfig(ctx, in.FunctionName, in.Qualifier, true,
		in.MaximumRetryAttempts, in.MaximumEventAgeInSeconds, in.DestinationConfig)
}

func (s *Service) UpdateFunctionEventInvokeConfig(
	ctx context.Context,
	in *api.UpdateFunctionEventInvokeConfigRequest,
) (*api.FunctionEventInvokeConfig, error) {
	return s.changeInvokeConfig(ctx, in.FunctionName, in.Qualifier, false,
		in.MaximumRetryAttempts, in.MaximumEventAgeInSeconds, in.DestinationConfig)
}

func (s *Service) GetFunctionEventInvokeConfig(
	ctx context.Context,
	in *api.GetFunctionEventInvokeConfigRequest,
) (*api.FunctionEventInvokeConfig, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var c invokeConfig
	var name string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var q string
		var resolveErr error
		if name, q, resolveErr = invokeConfigTarget(tx, &m, in.FunctionName, in.Qualifier); resolveErr != nil {
			return resolveErr
		}

		found, readErr := readJSON(tx, invokeConfigKey(m.Scope, name, q), &c)
		if readErr == nil && !found {
			return noInvokeConfig(m.Scope, name, q)
		}

		return readErr
	})
	if err != nil {
		return nil, err
	}

	return c.output(m.Scope, name), nil
}

func (s *Service) DeleteFunctionEventInvokeConfig(
	ctx context.Context,
	in *api.DeleteFunctionEventInvokeConfigRequest,
) (*api.DeleteFunctionEventInvokeConfigOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		name, q, resolveErr := invokeConfigTarget(tx, &m, in.FunctionName, in.Qualifier)
		if resolveErr != nil {
			return resolveErr
		}

		key := invokeConfigKey(m.Scope, name, q)
		if _, found := tx.Get(key); !found {
			return noInvokeConfig(m.Scope, name, q)
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFunctionEventInvokeConfigOutput{}, nil
}

// ListFunctionEventInvokeConfigs lists a function's configurations by
// qualifier, 50 per page.
func (s *Service) ListFunctionEventInvokeConfigs(
	ctx context.Context,
	in *api.ListFunctionEventInvokeConfigsRequest,
) (*api.ListFunctionEventInvokeConfigsResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxItems, listLimit)
	if err != nil {
		return nil, err
	}

	start, err := after(in.Marker)
	if err != nil {
		return nil, err
	}

	out := &api.ListFunctionEventInvokeConfigsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		name, resolveErr := latestFunction(tx, &m, in.FunctionName)
		if resolveErr != nil {
			return resolveErr
		}

		for _, k := range tx.Keys(m.Key("lambda", kindInvokeConfig, name+"/")) {
			var c invokeConfig
			if _, readErr := readJSON(tx, k, &c); readErr != nil {
				return readErr
			}

			if c.Qualifier <= start {
				continue
			}

			if int64(len(out.FunctionEventInvokeConfigs)) == limit {
				out.NextMarker = nextMarker(start)
				return nil
			}

			out.FunctionEventInvokeConfigs = append(out.FunctionEventInvokeConfigs, *c.output(m.Scope, name))
			start = c.Qualifier
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// loadInvokeConfig reads the configuration of an asynchronous invocation of
// the function at qualifier, or Lambda's defaults when it has none.
func (s *Service) loadInvokeConfig(ctx context.Context, scope request.Scope, name, qualifier string) invokeConfig {
	var c invokeConfig
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		_, readErr := readJSON(tx, invokeConfigKey(scope, name, qualifier), &c)
		return readErr
	})
	if err != nil {
		s.logger.Warn("read event invoke config", "function", qualifiedARN(scope, name, qualifier), "error", err)
		return invokeConfig{}
	}

	return c
}

// invocationRecord is the document Lambda sends to a destination.
type invocationRecord struct {
	RequestPayload  json.RawMessage `json:"requestPayload"`
	ResponsePayload json.RawMessage `json:"responsePayload,omitempty"`
	Version         string          `json:"version"`
	Timestamp       string          `json:"timestamp"`
	RequestContext  struct {
		RequestID              string `json:"requestId"`
		FunctionArn            string `json:"functionArn"`
		Condition              string `json:"condition"`
		ApproximateInvokeCount int    `json:"approximateInvokeCount"`
	} `json:"requestContext"`
	ResponseContext struct {
		ExecutedVersion string `json:"executedVersion,omitempty"`
		FunctionError   string `json:"functionError,omitempty"`
		StatusCode      int    `json:"statusCode"`
	} `json:"responseContext"`
}

// attemptResult is the outcome of an asynchronous invocation's last attempt.
type attemptResult struct {
	err       error
	version   string
	condition string
	result    Result
	attempts  int
}

// sendResult sends an asynchronous invocation's record to destination, if any.
func (s *Service) sendResult(
	ctx context.Context,
	arn, destination, id string,
	payload []byte,
	r *attemptResult,
) {
	if destination == "" {
		return
	}

	record := invocationRecord{
		RequestPayload: payload,
		Version:        recordVersion,
		Timestamp:      s.now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	record.RequestContext.RequestID = id
	record.RequestContext.FunctionArn = arn
	record.RequestContext.Condition = r.condition
	record.RequestContext.ApproximateInvokeCount = r.attempts
	record.ResponseContext.StatusCode = http.StatusOK
	record.ResponseContext.ExecutedVersion = r.version
	switch {
	case r.err != nil:
		record.ResponseContext.StatusCode = http.StatusInternalServerError
		e := awserror.From(r.err)
		record.ResponsePayload, _ = json.Marshal(map[string]string{"errorType": e.Code, "errorMessage": e.Message})
	case json.Valid(r.result.Payload):
		record.ResponsePayload = r.result.Payload
	case len(r.result.Payload) > 0:
		record.ResponsePayload, _ = json.Marshal(string(r.result.Payload))
	}

	if r.result.Error {
		record.ResponseContext.FunctionError = "Unhandled"
	}

	body, err := json.Marshal(record)
	if err == nil {
		err = s.deliver(ctx, destination, arn, r.condition, body, nil)
	}

	s.deliveries.Add(delivery.ViaLambdaDestination, arn, destination, body, err)
	if err != nil {
		s.logger.Warn("sending an async invocation record failed", "function", arn, "destination", destination,
			"error", err)
	}
}

// deliver sends a record to an SQS queue, SNS topic, Lambda function, or
// EventBridge event bus, in the destination's account and region, under the
// execution role of the function arn names.
func (s *Service) deliver(
	ctx context.Context,
	destination, arn, condition string,
	body []byte,
	attributes map[string]any,
) error {
	service, _ := destinationService(destination)
	source := strings.Split(arn, ":")
	fnScope, name := request.Scope{AccountID: source[4], Region: source[3]}, source[6]
	// A destination is invoked asynchronously; the execution role needs lambda:InvokeFunction on it.
	if service == serviceLambda {
		invoke := &catalog.DeliveryRole{Service: serviceLambda, Operation: "Invoke", Params: map[string]any{
			"FunctionName": destination, "InvocationType": "Event",
		}}
		d, err := s.executionDelivery(ctx, fnScope, name, invoke)
		if err == nil {
			err = s.authorizer.Authorize(ctx, d)
		}

		if err != nil {
			return err
		}

		return s.InvokeAsync(ctx, destination, body)
	}

	if s.caller == nil {
		return invalid("Destinations are not available")
	}

	parts := strings.Split(destination, ":")
	scope := request.Scope{AccountID: parts[4], Region: parts[3]}
	call := func(c *catalog.DeliveryRole) (map[string]any, error) {
		callCtx := request.WithMetadata(
			ctx,
			request.Metadata{Scope: scope, Service: c.Service, RequestID: uuid.NewV4().String()},
		)
		d, err := s.executionDelivery(callCtx, fnScope, name, c)
		if err != nil {
			return nil, err
		}

		return s.authorizer.Call(callCtx, s.caller, d)
	}

	var err error
	switch service {
	case serviceSQS:
		url, _ := sqs.QueueURL(destination)
		_, err = call(&catalog.DeliveryRole{Service: service, Operation: "SendMessage", Params: map[string]any{
			"QueueUrl": url, "MessageBody": string(body), "MessageAttributes": attributes,
		}})
	case serviceSNS:
		_, err = call(&catalog.DeliveryRole{Service: service, Operation: "Publish", Params: map[string]any{
			"TopicArn": destination, "Message": string(body), "MessageAttributes": attributes,
		}})
	default:
		outcome := "Success"
		if condition != conditionSuccess {
			outcome = "Failure"
		}

		var out any
		out, err = call(&catalog.DeliveryRole{Service: "eventbridge", Operation: "PutEvents", Params: map[string]any{
			"Entries": []any{map[string]any{
				"Source":       serviceLambda,
				"DetailType":   "Lambda Function Invocation Result - " + outcome,
				"Detail":       string(body),
				"EventBusName": destination,
				"Resources":    []any{destination, arn},
			}},
		}})
		if err == nil {
			err = putEventsFailure(out)
		}
	}

	return err
}

// putEventsFailure reports a PutEvents entry that failed.
func putEventsFailure(out any) error {
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}

	var response struct {
		Entries []struct {
			ErrorCode    string
			ErrorMessage string
		}
	}
	if unmarshalErr := json.Unmarshal(data, &response); unmarshalErr != nil {
		return unmarshalErr
	}

	for _, e := range response.Entries {
		if e.ErrorCode != "" {
			return awserror.New(e.ErrorCode, e.ErrorMessage)
		}
	}

	return nil
}
