package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
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
)

// retryDelay spaces asynchronous retries.
var retryDelay = time.Second

// prepare loads what the runtime needs to run the function stored under name.
func (s *Service) prepare(ctx context.Context, scope request.Scope, name string) (*Function, error) {
	var fn *function
	var code []byte
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var loadErr error
		fn, found, loadErr = loadFunction(tx, scopeKey(scope, kindFunction, name))
		if loadErr == nil && !found {
			return notFound("Function " + functionARN(scope, name))
		}

		code, _ = tx.Get(scopeKey(scope, "code", name))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	env := map[string]string{
		"AWS_REGION":                      scope.Region,
		"AWS_DEFAULT_REGION":              scope.Region,
		"AWS_LAMBDA_FUNCTION_NAME":        fn.Name,
		"AWS_LAMBDA_FUNCTION_VERSION":     latest,
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

	return &Function{
		Environment:  env,
		ARN:          functionARN(scope, fn.Name),
		Name:         fn.Name,
		Region:       scope.Region,
		Runtime:      fn.Runtime,
		Handler:      fn.Handler,
		PackageType:  fn.PackageType,
		ImageURI:     fn.ImageURI,
		Architecture: fn.Architecture,
		Revision:     fn.CodeSha256 + "/" + fn.LastModified,
		Code:         code,
		Timeout:      time.Duration(fn.Timeout) * time.Second,
		MemorySize:   fn.MemorySize,
	}, nil
}

// run invokes the function synchronously.
func (s *Service) run(ctx context.Context, scope request.Scope, name string, payload []byte) (Result, error) {
	fn, err := s.prepare(ctx, scope, name)
	if err != nil {
		return Result{}, err
	}

	result, err := s.runtime.Invoke(ctx, fn, payload)
	if err != nil {
		return Result{}, awserror.New("ServiceException", "The function could not be run: "+err.Error())
	}

	return result, nil
}

// runAsync invokes the function in the background, retrying failures. It
// reports false when the service is stopping.
func (s *Service) runAsync(scope request.Scope, name string, payload []byte) bool {
	return s.spawn(func(ctx context.Context) {
		for attempt := 1; ; attempt++ {
			result, err := s.run(ctx, scope, name, payload)
			failure := invokeError(result, err)
			if failure != nil {
				failure = fmt.Errorf("attempt %d of %d: %w", attempt, asyncAttempts, failure)
			}

			s.deliveries.Add(delivery.ViaLambdaAsync, "", functionARN(scope, name), payload, failure)
			if failure == nil {
				return
			}

			s.logger.Warn(
				"async lambda invocation failed",
				"function", functionARN(scope, name),
				"attempt", attempt,
				"error", err,
				"payload", string(result.Payload),
			)

			if attempt == asyncAttempts {
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay * time.Duration(attempt)):
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
	m, fn, err := s.load(ctx, in.FunctionName)
	if err != nil {
		return nil, err
	}

	switch value(in.InvocationType) {
	case "", "RequestResponse":
		payload, checkErr := checkPayload(in.Payload, maxSyncPayload)
		if checkErr != nil {
			return nil, checkErr
		}

		result, runErr := s.run(ctx, m.Scope, fn.Name, payload)
		if runErr != nil {
			return nil, runErr
		}

		out := &api.InvocationResponse{StatusCode: ptr(int64(http.StatusOK)), Payload: result.Payload}
		if result.Error {
			out.FunctionError = ptr("Unhandled")
		}

		return out, nil
	case "Event":
		payload, checkErr := checkPayload(in.Payload, maxAsyncPayload)
		if checkErr != nil {
			return nil, checkErr
		}

		if !s.runAsync(m.Scope, fn.Name, payload) {
			return nil, awserror.New("ServiceException", "The service is shutting down")
		}

		return &api.InvocationResponse{StatusCode: ptr(int64(http.StatusAccepted))}, nil
	case "DryRun":
		return &api.InvocationResponse{StatusCode: ptr(int64(http.StatusNoContent))}, nil
	default:
		return nil, invalid("InvocationType must be RequestResponse, Event, or DryRun")
	}
}

// InvokeAsync queues an asynchronous invocation of the function named by
// functionARN, as an Event invocation does. Other services, such as SNS, call
// it in-process instead of going through HTTP.
func (s *Service) InvokeAsync(ctx context.Context, arn string, payload []byte) error {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 || parts[0] != "arn" || parts[2] != "lambda" || !accountID.MatchString(parts[4]) {
		return invalid("%s is not a Lambda function ARN", arn)
	}

	m := request.Metadata{Scope: request.Scope{AccountID: parts[4], Region: parts[3]}}
	name, err := resolveName(m, arn)
	if err != nil {
		return err
	}

	if _, err = s.prepare(ctx, m.Scope, name); err != nil {
		return err
	}

	if !s.runAsync(m.Scope, name, payload) {
		return awserror.New("ServiceException", "The service is shutting down")
	}

	return nil
}
