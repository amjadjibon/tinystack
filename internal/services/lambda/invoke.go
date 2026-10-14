package lambda

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/recent"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxSyncPayload  = 6 << 20
	maxAsyncPayload = 1 << 20
	// asyncAttempts is how often an asynchronous invocation runs before it is
	// dropped: the first attempt and two retries, as in Lambda.
	asyncAttempts = 3
	// maxLogTail is how much of the log LogType Tail returns.
	maxLogTail = 4096
)

// retryDelay spaces asynchronous retries.
var retryDelay = time.Second

type clientContextKey struct{}

// prepare loads what the runtime needs to run the function stored under name
// at qualifier: $LATEST when it is empty, a version, or an alias.
func (s *Service) prepare(ctx context.Context, scope request.Scope, name, qualifier string) (*Function, error) {
	var fn *function
	var code []byte
	var layers [][]byte
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var codeKey string
		var loadErr error
		if fn, codeKey, loadErr = resolveRouted(tx, scope, name, qualifier, true); loadErr != nil {
			return loadErr
		}

		code, _ = tx.Get(codeKey)
		layers, loadErr = layerCode(tx, &request.Metadata{Scope: scope}, fn.Layers)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	env := map[string]string{
		"AWS_REGION":                      scope.Region,
		"AWS_DEFAULT_REGION":              scope.Region,
		"AWS_LAMBDA_FUNCTION_NAME":        fn.Name,
		"AWS_LAMBDA_FUNCTION_VERSION":     fn.version(),
		"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": strconv.FormatInt(fn.MemorySize, 10),
		"AWS_LAMBDA_FUNCTION_TIMEOUT":     strconv.FormatInt(fn.Timeout, 10),
		// The creator's access key selects the function's account for SDK
		// calls back to TinyStack, signed with its secret when one is configured.
		"AWS_ACCESS_KEY_ID":     placeholderKey,
		"AWS_SECRET_ACCESS_KEY": placeholderKey,
	}

	if fn.AccessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = fn.AccessKey
	}

	if secret, ok := s.secretKeys[fn.AccessKey]; ok {
		env["AWS_SECRET_ACCESS_KEY"] = secret
	}

	for k, v := range fn.Environment {
		env[k] = v
	}

	metadata, _ := request.FromContext(ctx)
	clientContext, _ := ctx.Value(clientContextKey{}).(string)
	return &Function{
		Endpoint:      metadata.Endpoint,
		RequestID:     metadata.RequestID,
		TraceID:       metadata.TraceID,
		ClientContext: clientContext,
		Environment:   env,
		ARN:           fn.arn(scope),
		Name:          fn.Name,
		Version:       fn.version(),
		Region:        scope.Region,
		Runtime:       fn.Runtime,
		Handler:       fn.Handler,
		PackageType:   fn.PackageType,
		ImageURI:      fn.ImageURI,
		Architecture:  fn.Architecture,
		Revision:      fn.CodeSha256 + "/" + fn.LastModified + "/" + fn.RevisionID,
		Code:          code,
		HotReloadPath: fn.HotReloadPath,
		Layers:        layers,
		Timeout:       time.Duration(fn.Timeout) * time.Second,
		MemorySize:    fn.MemorySize,
	}, nil
}

// run invokes the function at qualifier synchronously and returns the
// invocation's log, which it also writes to the function's log group, and the
// version that ran.
func (s *Service) run(
	ctx context.Context,
	scope request.Scope,
	name, qualifier string,
	payload []byte,
) (result Result, log, version string, err error) {
	fn, err := s.prepare(ctx, scope, name, qualifier)
	if err != nil {
		return Result{}, "", "", err
	}

	if fn.HotReloadPath != "" {
		runtime, ok := s.runtime.(DirectoryRuntime)
		if !ok || !runtime.SupportsDirectories() {
			return Result{}, "", "", invalid("Hot reload invocation requires a directory-capable runtime; " +
				"the in-process runtime does not support host directories")
		}
	}

	release, err := s.acquire(ctx, scope, name)
	if err != nil {
		return Result{}, "", "", err
	}

	defer release()
	started := time.Now()
	result, err = s.runtime.Invoke(ctx, fn, payload)
	if err != nil {
		return Result{}, "", "", awserror.New("ServiceException", "The function could not be run: "+err.Error())
	}

	return result, s.record(ctx, scope, fn, time.Since(started), result.Log), fn.Version, nil
}

// record writes an invocation's log, the function's output between START and
// END lines and a REPORT line, to /aws/lambda/<name> in a stream per day and
// revision, and returns it. A failure to write the log does not fail the
// invocation.
func (s *Service) record(
	ctx context.Context,
	scope request.Scope,
	fn *Function,
	duration time.Duration,
	output string,
) string {
	id := uuid.NewV4().String()
	if m, ok := request.FromContext(ctx); ok && m.RequestID != "" {
		id = m.RequestID
	}

	lines := []string{"START RequestId: " + id + " Version: " + fn.Version}
	for line := range strings.SplitSeq(strings.TrimRight(output, "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}

	ms := float64(duration.Microseconds()) / 1000
	lines = append(lines,
		"END RequestId: "+id,
		fmt.Sprintf("REPORT RequestId: %s\tDuration: %.2f ms\tBilled Duration: %d ms\tMemory Size: %d MB",
			id, ms, int64(math.Ceil(ms)), fn.MemorySize),
	)

	revision := sha256.Sum256([]byte(fn.Revision))
	stream := time.Now().UTC().Format("2006/01/02") + "/[" + fn.Version + "]" + hex.EncodeToString(revision[:16])
	if err := s.logs.Append(context.WithoutCancel(ctx), scope, "/aws/lambda/"+fn.Name, stream, lines); err != nil {
		s.logger.Warn("writing lambda log failed", "function", fn.ARN, "error", err)
	}

	return strings.Join(lines, "\n") + "\n"
}

// runAsync invokes the function at qualifier in the background, retrying
// failures as its event invoke configuration allows and then sending the
// result to its destination. It reports false when the service is stopping.
func (s *Service) runAsync(scope request.Scope, name, qualifier string, payload []byte) bool {
	arn := qualifiedARN(scope, name, qualifier)
	queued := s.now()
	return s.bg.Go(func(ctx context.Context) {
		cfg := s.loadInvokeConfig(ctx, scope, name, qualifier)
		attempts, id := cfg.attempts(), uuid.NewV4().String()
		for attempt := 1; ; attempt++ {
			result, _, version, err := s.run(ctx, scope, name, qualifier, payload)
			r := &attemptResult{err: err, version: version, result: result, attempts: attempt}
			failure := invokeError(result, err)
			if failure != nil {
				failure = fmt.Errorf("attempt %d of %d: %w", attempt, attempts, failure)
			}

			s.deliveries.Add(delivery.ViaLambdaAsync, "", arn, payload, failure)
			if failure == nil {
				r.condition = conditionSuccess
				s.sendResult(ctx, arn, cfg.OnSuccess, id, payload, r)
				return
			}

			s.logger.Warn(
				"async lambda invocation failed",
				"function", arn,
				"attempt", attempt,
				"error", err,
				"payload", string(result.Payload),
			)

			if attempt == attempts {
				r.condition = conditionRetries
				s.sendResult(ctx, arn, cfg.OnFailure, id, payload, r)
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay * time.Duration(attempt)):
			}

			if s.now().Sub(queued) > cfg.maxAge() {
				r.condition = conditionEventAge
				s.sendResult(ctx, arn, cfg.OnFailure, id, payload, r)
				return
			}
		}
	})
}

// invokeError reports why an invocation failed: the runtime error, or the
// function's error payload. It is nil when the function succeeded.
func invokeError(result Result, err error) error {
	if err != nil {
		return err
	}

	if result.Error {
		return errors.New("function error: " + recent.Text(result.Payload))
	}

	return nil
}

func checkPayload(payload []byte, limit int) ([]byte, error) {
	if len(payload) > limit {
		return nil, &awserror.Error{
			Code:       "RequestTooLargeException",
			Message:    "Request must be smaller than " + strconv.Itoa(limit) + " bytes for the Invoke operation",
			StatusCode: http.StatusRequestEntityTooLarge,
		}
	}

	if strings.TrimSpace(string(payload)) == "" {
		return []byte("{}"), nil
	}

	if !json.Valid(payload) {
		return nil, awserror.New("InvalidRequestContentException", "Could not parse request body into json")
	}

	return payload, nil
}

func (s *Service) Invoke(ctx context.Context, in *api.InvocationRequest) (*api.InvocationResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, q, err := resolveName(m, common.Value(in.FunctionName))
	if err != nil {
		return nil, err
	}

	if q, err = withQualifier(q, in.Qualifier); err != nil {
		return nil, err
	}

	fn, err := s.prepare(ctx, m.Scope, name, q)
	if err != nil {
		return nil, err
	}

	tail := false
	switch common.Value(in.LogType) {
	case "", "None":
	case "Tail":
		tail = true
	default:
		return nil, invalid("LogType must be None or Tail")
	}

	if tail && common.Value(in.InvocationType) != "" && common.Value(in.InvocationType) != "RequestResponse" {
		return nil, invalid("LogType Tail is only supported for RequestResponse invocations")
	}

	switch common.Value(in.InvocationType) {
	case "", "RequestResponse":
		payload, checkErr := checkPayload(in.Payload, maxSyncPayload)
		if checkErr != nil {
			return nil, checkErr
		}

		if in.ClientContext != nil {
			if len(*in.ClientContext) > 3583 {
				return nil, invalid("ClientContext must be at most 3583 base64 bytes")
			}

			decoded, decodeErr := base64.StdEncoding.DecodeString(*in.ClientContext)
			if decodeErr != nil || !json.Valid(decoded) {
				return nil, invalid("ClientContext must be base64-encoded JSON")
			}

			var document map[string]json.RawMessage
			if json.Unmarshal(decoded, &document) != nil || document == nil {
				return nil, invalid("ClientContext must contain a JSON object")
			}

			ctx = context.WithValue(ctx, clientContextKey{}, string(decoded))
		}

		result, log, version, runErr := s.run(ctx, m.Scope, name, q, payload)
		if runErr != nil {
			return nil, runErr
		}

		out := &api.InvocationResponse{
			StatusCode:      new(int64(http.StatusOK)),
			Payload:         result.Payload,
			ExecutedVersion: &version,
		}

		if tail {
			out.LogResult = new(base64.StdEncoding.EncodeToString([]byte(log[max(0, len(log)-maxLogTail):])))
		}

		if result.Error {
			out.FunctionError = new("Unhandled")
		}

		return out, nil
	case "Event":
		payload, checkErr := checkPayload(in.Payload, maxAsyncPayload)
		if checkErr != nil {
			return nil, checkErr
		}

		if !s.runAsync(m.Scope, name, q, payload) {
			return nil, awserror.New("ServiceException", "The service is shutting down")
		}

		return &api.InvocationResponse{StatusCode: new(int64(http.StatusAccepted)), ExecutedVersion: &fn.Version}, nil
	case "DryRun":
		return &api.InvocationResponse{StatusCode: new(int64(http.StatusNoContent))}, nil
	default:
		return nil, invalid("InvocationType must be RequestResponse, Event, or DryRun")
	}
}

// InvokeAsync queues an asynchronous invocation of the function, version, or
// alias named by arn, as an Event invocation does. Other services, such as
// SNS, call it in-process instead of going through HTTP.
func (s *Service) InvokeAsync(ctx context.Context, arn string, payload []byte) error {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 || parts[0] != "arn" || parts[2] != "lambda" || !accountID.MatchString(parts[4]) {
		return invalid("%s is not a Lambda function ARN", arn)
	}

	m := request.Metadata{Scope: request.Scope{AccountID: parts[4], Region: parts[3]}}
	name, q, err := resolveName(m, arn)
	if err != nil {
		return err
	}

	if _, err = s.prepare(ctx, m.Scope, name, q); err != nil {
		return err
	}

	if !s.runAsync(m.Scope, name, q, payload) {
		return awserror.New("ServiceException", "The service is shutting down")
	}

	return nil
}
