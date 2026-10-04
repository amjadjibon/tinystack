package sfn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const (
	waitForToken = ".waitForTaskToken"
	syncSuffix   = ".sync"
	sync2Suffix  = ".sync:2"
	pollInterval = 100 * time.Millisecond
)

// integrations are the optimized service integrations, with the suffixes
// each accepts.
var integrations = map[string][]string{
	"lambda:invoke":         {waitForToken},
	"sqs:sendMessage":       {waitForToken},
	"sns:publish":           {waitForToken},
	"dynamodb:getItem":      nil,
	"dynamodb:putItem":      nil,
	"dynamodb:deleteItem":   nil,
	"dynamodb:updateItem":   nil,
	"events:putEvents":      {waitForToken},
	"states:startExecution": {syncSuffix, sync2Suffix, waitForToken},
}

// optimized maps an optimized integration's service to the TinyStack service
// it calls and the prefix of its error names.
var optimized = map[string]struct{ service, prefix string }{
	"lambda":   {"lambda", "Lambda"},
	"sqs":      {"sqs", "SQS"},
	"sns":      {"sns", "SNS"},
	"dynamodb": {svcDynamoDB, "DynamoDB"},
	"events":   {svcEventBridge, "EventBridge"},
	states:     {service, "StepFunctions"},
}

// sdkServices maps an AWS SDK integration's service name to TinyStack's, and
// to the prefix of its error names, where they differ from the name.
var sdkServices = map[string]struct{ service, prefix string }{
	"cognitoidentityprovider": {"cognitoidp", "CognitoIdentityProvider"},
	"cloudwatchlogs":          {"logs", "CloudWatchLogs"},
	svcDynamoDB:               {svcDynamoDB, "DynamoDb"},
	svcEventBridge:            {svcEventBridge, "EventBridge"},
	"secretsmanager":          {"secretsmanager", "SecretsManager"},
	"apigateway":              {"apigateway", "ApiGateway"},
	"apigatewayv2":            {"apigatewayv2", "ApiGatewayV2"},
	"dynamodbstreams":         {"dynamodbstreams", "DynamoDbStreams"},
}

// outcome is how a task a token stands for ended.
type outcome struct {
	output any
	err    *stateError
}

// waiter is a task waiting on its token.
type waiter struct {
	done      chan outcome
	heartbeat chan struct{}
	started   chan string
	execution string
}

// activityTask is a task scheduled on an activity.
type activityTask struct {
	token string
	input string
}

// register makes a task token for an execution.
func (s *Service) register(execution string) (string, *waiter) {
	w := &waiter{
		done: make(chan outcome, 1), heartbeat: make(chan struct{}, 1), started: make(chan string, 1),
		execution: execution,
	}
	token := newToken()
	s.mu.Lock()
	s.tokens[token] = w
	s.mu.Unlock()
	return token, w
}

func (s *Service) unregister(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// queue returns an activity's task queue.
func (s *Service) queue(activityARN string) chan *activityTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[activityARN]
	if !ok {
		q = make(chan *activityTask, 1000)
		s.queues[activityARN] = q
	}

	return q
}

// timeouts reads a Task state's TimeoutSeconds and HeartbeatSeconds, or
// their paths.
func timeouts(s *state, input any, e *env) (timeout, heartbeat time.Duration, err error) {
	read := func(fixed *int64, path string) (time.Duration, error) {
		if path == "" {
			return time.Duration(value(fixed)) * time.Second, nil
		}

		v, pathErr := resolvePath(path, input, e)
		n, ok := v.(float64)
		if pathErr != nil || !ok || n <= 0 {
			return 0, &stateError{Name: errRuntime, Cause: path + " must select a positive number"}
		}

		return time.Duration(n) * time.Second, nil
	}

	if timeout, err = read(s.TimeoutSeconds, s.TimeoutSecondsPath); err != nil {
		return 0, 0, err
	}

	heartbeat, err = read(s.HeartbeatSeconds, s.HeartbeatSecondsPath)
	return timeout, heartbeat, err
}

// task runs a Task state once: its resource with its parameters.
//
//nolint:gocyclo,cyclop // One branch per kind of resource.
func (r *run) task(ctx context.Context, s *state, input any, e *env) (any, error) {
	timeout, heartbeat, err := timeouts(s, input, e)
	if err != nil {
		return nil, err
	}

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, &stateError{
			Name: errTimeout, Cause: "The task did not complete within its TimeoutSeconds.",
		})
		defer cancel()
	}

	resource := s.Resource
	activity := activityARN.MatchString(resource)
	var token string
	var w *waiter
	if activity || strings.HasSuffix(resource, waitForToken) {
		token, w = r.s.register(r.exec.ARN)
		defer r.s.unregister(token)
		e.context[stateTask] = map[string]any{"Token": token}
	}

	params := input
	if s.Parameters != nil {
		if params, err = template(s.Parameters, input, e); err != nil {
			return nil, err
		}
	}

	switch {
	case functionARN.MatchString(resource):
		return r.function(ctx, resource, params)
	case activity:
		r.event(ctx, "ActivityScheduled", func(e *api.HistoryEvent) {
			e.ActivityScheduledEventDetails = &api.ActivityScheduledEventDetails{
				Resource: new(resource), Input: text(params), TimeoutInSeconds: seconds(timeout),
				HeartbeatInSeconds: seconds(heartbeat),
			}
		})
		select {
		case r.s.queue(resource) <- &activityTask{token: token, input: *text(params)}:
		default:
			return nil, &stateError{Name: errRuntime, Cause: "The activity's task queue is full"}
		}

		out, waitErr := r.await(ctx, w, heartbeat, func(worker string) {
			r.event(ctx, "ActivityStarted", func(e *api.HistoryEvent) {
				e.ActivityStartedEventDetails = &api.ActivityStartedEventDetails{WorkerName: new(worker)}
			})
		})
		r.taskEnd(ctx, eventActivity, "", "", out, waitErr)
		return out, waitErr
	}

	resourceType, action, suffix := splitResource(resource)
	r.event(ctx, "TaskScheduled", func(e *api.HistoryEvent) {
		e.TaskScheduledEventDetails = &api.TaskScheduledEventDetails{
			Resource: new(action), ResourceType: new(resourceType), Region: new(r.scope.Region),
			Parameters: text(params), TimeoutInSeconds: seconds(timeout), HeartbeatInSeconds: seconds(heartbeat),
		}
	})
	r.event(ctx, "TaskStarted", nil)
	out, err := r.integrate(ctx, resourceType, action, suffix, params)
	if err == nil && w != nil {
		r.event(ctx, "TaskSubmitted", func(e *api.HistoryEvent) {
			e.TaskSubmittedEventDetails = &api.TaskSubmittedEventDetails{
				Resource: new(action), ResourceType: new(resourceType), Output: text(out),
			}
		})
		out, err = r.await(ctx, w, heartbeat, nil)
	}

	r.taskEnd(ctx, stateTask, resourceType, action, out, err)
	return out, err
}

func seconds(d time.Duration) *int64 {
	if d == 0 {
		return nil
	}

	return new(int64(d / time.Second))
}

// taskEnd records how a task ended.
func (r *run) taskEnd(ctx context.Context, kind, resourceType, action string, out any, err error) {
	if err == nil {
		r.event(ctx, kind+"Succeeded", func(e *api.HistoryEvent) {
			if kind == eventActivity {
				e.ActivitySucceededEventDetails = &api.ActivitySucceededEventDetails{Output: text(out)}
				return
			}

			e.TaskSucceededEventDetails = &api.TaskSucceededEventDetails{
				Resource: new(action), ResourceType: new(resourceType), Output: text(out),
			}
		})
		return
	}

	se := asStateError(err)
	timedOut := se.Name == errTimeout || se.Name == errHeartbeat
	outcome := map[bool]string{true: "TimedOut", false: "Failed"}[timedOut]
	r.event(ctx, kind+outcome, func(e *api.HistoryEvent) {
		switch {
		case kind == eventActivity && timedOut:
			e.ActivityTimedOutEventDetails = &api.ActivityTimedOutEventDetails{
				Error: new(se.Name),
				Cause: new(se.Cause),
			}
		case kind == eventActivity:
			e.ActivityFailedEventDetails = &api.ActivityFailedEventDetails{Error: new(se.Name), Cause: new(se.Cause)}
		case timedOut:
			e.TaskTimedOutEventDetails = &api.TaskTimedOutEventDetails{
				Resource: new(action), ResourceType: new(resourceType), Error: new(se.Name), Cause: new(se.Cause),
			}
		default:
			e.TaskFailedEventDetails = &api.TaskFailedEventDetails{
				Resource: new(action), ResourceType: new(resourceType), Error: new(se.Name), Cause: new(se.Cause),
			}
		}
	})
}

// splitResource splits a service integration's resource into its type
// (lambda, or aws-sdk:<service>), action, and suffix.
func splitResource(resource string) (resourceType, action, suffix string) {
	if m := sdkResource.FindStringSubmatch(resource); m != nil {
		return "aws-sdk:" + m[1], m[2], m[3]
	}

	m := integration.FindStringSubmatch(resource)
	return m[1], m[2], m[3]
}

// await waits for a task token's outcome, failing with
// States.HeartbeatTimeout when heartbeats stop. started, if set, hears when
// an activity worker takes the task.
func (r *run) await(ctx context.Context, w *waiter, heartbeat time.Duration, started func(string)) (any, error) {
	var beat <-chan time.Time
	var timer *time.Timer
	if heartbeat > 0 {
		timer = time.NewTimer(heartbeat)
		defer timer.Stop()
		beat = timer.C
	}

	for {
		select {
		case o := <-w.done:
			if o.err != nil {
				return nil, o.err
			}

			return o.output, nil
		case worker := <-w.started:
			if started != nil {
				started(worker)
			}
		case <-w.heartbeat:
			if timer != nil {
				timer.Reset(heartbeat)
			}
		case <-beat:
			return nil, &stateError{Name: errHeartbeat, Cause: "The task sent no heartbeat in time."}
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// function invokes a Lambda function named by its ARN, whose result is its
// JSON payload.
func (r *run) function(ctx context.Context, resource string, params any) (any, error) {
	r.event(ctx, "LambdaFunctionScheduled", func(e *api.HistoryEvent) {
		e.LambdaFunctionScheduledEventDetails = &api.LambdaFunctionScheduledEventDetails{
			Resource: new(resource), Input: text(params),
		}
	})
	r.event(ctx, "LambdaFunctionStarted", nil)
	out, err := r.invoke(ctx, map[string]any{"FunctionName": resource, "Payload": params})
	if err != nil {
		se := asStateError(err)
		r.event(ctx, "LambdaFunctionFailed", func(e *api.HistoryEvent) {
			e.LambdaFunctionFailedEventDetails = &api.LambdaFunctionFailedEventDetails{
				Error: new(se.Name), Cause: new(se.Cause),
			}
		})
		return nil, se
	}

	r.event(ctx, "LambdaFunctionSucceeded", func(e *api.HistoryEvent) {
		e.LambdaFunctionSucceededEventDetails = &api.LambdaFunctionSucceededEventDetails{Output: text(out["Payload"])}
	})
	return out["Payload"], nil
}

// invoke calls Lambda's Invoke and parses the payload. A function error
// fails with its errorType (Lambda.Unknown without one) and the payload as
// the cause.
func (r *run) invoke(ctx context.Context, params map[string]any) (map[string]any, error) {
	out, err := r.call(ctx, "lambda", "Invoke", "Lambda", params)
	if err != nil {
		return nil, err
	}

	payload, _ := out["Payload"].(string)
	var parsed any = payload
	if json.Valid([]byte(payload)) {
		_ = json.Unmarshal([]byte(payload), &parsed)
	}

	if out["FunctionError"] != nil {
		name := "Lambda.Unknown"
		if m, ok := parsed.(map[string]any); ok {
			if t, isString := m["errorType"].(string); isString && t != "" {
				name = t
			}
		}

		return nil, &stateError{Name: name, Cause: payload}
	}

	out["Payload"] = parsed
	return out, nil
}

// call runs an operation of a TinyStack service in the execution's account
// and region, naming its errors <prefix>.<code>.
func (r *run) call(ctx context.Context, svc, op, prefix string, params map[string]any) (map[string]any, error) {
	if r.s.caller == nil {
		return nil, &stateError{Name: errRuntime, Cause: "Service integrations are not available"}
	}

	ctx = request.WithMetadata(ctx, request.Metadata{
		Scope: r.scope, RequestID: newUUID(), Service: svc, Operation: op, Endpoint: r.exec.Endpoint,
	})
	out, err := r.s.caller.Call(ctx, svc, op, params)
	var ae *awserror.Error
	switch {
	case err == nil:
		return out, nil
	case ctx.Err() != nil:
		return nil, context.Cause(ctx)
	case errors.As(err, &ae):
		return nil, &stateError{Name: prefix + "." + ae.Code, Cause: ae.Message}
	}

	return nil, &stateError{Name: prefix + ".SdkClientException", Cause: err.Error()}
}

// jsonField encodes a parameter that must be text, such as an SQS message
// body, when the definition gives JSON.
func jsonField(params map[string]any, name string) {
	if v, ok := params[name]; ok {
		if _, isString := v.(string); !isString {
			params[name] = *text(v)
		}
	}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}

	return string(unicode.ToUpper(rune(s[0]))) + s[1:]
}

// integrate runs a service integration's action.
func (r *run) integrate(ctx context.Context, resourceType, action, suffix string, params any) (any, error) {
	fields, ok := params.(map[string]any)
	if !ok {
		return nil, &stateError{Name: errRuntime, Cause: "A service integration's parameters must be an object"}
	}

	fields = copyMap(fields)
	if sdk, isSDK := strings.CutPrefix(resourceType, "aws-sdk:"); isSDK {
		target, known := sdkServices[sdk]
		if !known {
			target.service, target.prefix = sdk, upperFirst(sdk)
		}

		return r.call(ctx, target.service, upperFirst(action), target.prefix, fields)
	}

	target := optimized[resourceType]
	switch resourceType + ":" + action {
	case "lambda:invoke":
		out, err := r.invoke(ctx, fields)
		if err != nil {
			return nil, err
		}

		out["SdkHttpMetadata"] = map[string]any{"HttpStatusCode": float64(200)}
		return out, nil
	case "sqs:sendMessage":
		jsonField(fields, "MessageBody")
	case "sns:publish":
		jsonField(fields, "Message")
	case "states:startExecution":
		jsonField(fields, "Input")
		return r.startChild(ctx, fields, suffix)
	case "events:putEvents":
		entries, _ := fields["Entries"].([]any)
		for _, e := range entries {
			if entry, isMap := e.(map[string]any); isMap {
				jsonField(entry, "Detail")
			}
		}

		out, err := r.call(ctx, target.service, "PutEvents", target.prefix, fields)
		if err == nil {
			if failed, _ := out["FailedEntryCount"].(float64); failed > 0 {
				return nil, &stateError{Name: "EventBridge.FailedEntry", Cause: *text(out)}
			}
		}

		return out, err
	}

	return r.call(ctx, target.service, upperFirst(action), target.prefix, fields)
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}

	return out
}

// startChild starts an execution of another state machine and, with .sync
// or .sync:2, waits for it, failing with States.TaskFailed when it fails.
func (r *run) startChild(ctx context.Context, params map[string]any, suffix string) (any, error) {
	out, err := r.call(ctx, service, "StartExecution", "StepFunctions", params)
	if err != nil || (suffix != syncSuffix && suffix != sync2Suffix) {
		return out, err
	}

	for {
		if sleepErr := sleep(ctx, pollInterval); sleepErr != nil {
			return nil, sleepErr
		}

		described, describeErr := r.call(ctx, service, "DescribeExecution", "StepFunctions",
			map[string]any{"ExecutionArn": out["ExecutionArn"]})
		if describeErr != nil {
			return nil, describeErr
		}

		switch described["Status"] {
		case statusRunning:
			continue
		case statusSucceeded:
			if suffix == sync2Suffix {
				for _, k := range []string{"Input", "Output"} {
					var parsed any
					if s, ok := described[k].(string); ok && json.Unmarshal([]byte(s), &parsed) == nil {
						described[k] = parsed
					}
				}
			}

			return described, nil
		}

		return nil, &stateError{Name: errTaskFailed, Cause: *text(described)}
	}
}
