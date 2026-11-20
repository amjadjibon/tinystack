package lambda

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/eventstream"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const maxStreamPayload = 200 << 20

func (s *Service) InvokeWithResponseStream(
	ctx context.Context, in *api.InvokeWithResponseStreamRequest,
) (*api.InvokeWithResponseStreamResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, q, err := resolveName(&m, common.Value(in.FunctionName))
	if err != nil {
		return nil, err
	}

	q, err = withQualifier(q, in.Qualifier)
	if err != nil {
		return nil, err
	}

	tail := common.Value(in.LogType) == logTail
	switch common.Value(in.LogType) {
	case "", logNone, logTail:
	default:
		return nil, invalid("LogType must be None or Tail")
	}

	switch common.Value(in.InvocationType) {
	case "", invokeRequestResponse:
	case invokeDryRun:
		if in.ClientContext != nil || tail {
			return nil, invalid("ClientContext and LogType Tail are only supported for RequestResponse invocations")
		}

		if _, prepareErr := s.prepare(ctx, m.Scope, name, q); prepareErr != nil {
			return nil, prepareErr
		}

		return &api.InvokeWithResponseStreamResponse{StatusCode: new(int64(http.StatusNoContent))}, nil
	default:
		return nil, invalid("InvocationType must be RequestResponse or DryRun")
	}

	payload, err := checkPayload(in.Payload, maxSyncPayload)
	if err != nil {
		return nil, err
	}

	ctx, err = invocationClientContext(ctx, in.ClientContext)
	if err != nil {
		return nil, err
	}

	fn, err := s.prepare(ctx, m.Scope, name, q)
	if err != nil {
		return nil, err
	}

	var invoke invokeFunction
	stream := &eventstream.Stream{
		Prepare: func() error {
			var err error
			invoke, err = s.beginInvocation(ctx, m.Scope, fn, q)
			return err
		},
		Produce: func(send func(eventstream.Message) error) error {
			return streamInvocation(invoke, payload, tail, send)
		},
	}

	return &api.InvokeWithResponseStreamResponse{
		StatusCode: new(int64(http.StatusOK)), ExecutedVersion: new(fn.Version), EventStream: stream,
	}, nil
}

func streamInvocation(
	invoke invokeFunction, payload []byte, tail bool, send func(eventstream.Message) error,
) error {
	result, log, runErr := invoke(payload, func(_ string, body io.Reader) error {
		return sendPayloadChunks(body, send)
	})
	complete := invokeCompletion{}
	switch {
	case runErr != nil:
		e := awserror.From(runErr)
		complete.ErrorCode, complete.ErrorDetails = e.Code, e.Message
	case result.Error:
		var details struct {
			Type string `json:"errorType"`
		}

		_ = json.Unmarshal(result.Payload, &details)
		complete.ErrorCode = cmp.Or(details.Type, "Unhandled")
		complete.ErrorDetails = string(result.Payload)
	case len(result.Payload) > 0:
		if err := sendPayloadChunks(bytes.NewReader(result.Payload), send); err != nil {
			return err
		}
	}

	if tail && log != "" {
		complete.LogResult = encodedLogTail(log)
	}

	encoded, err := json.Marshal(complete)
	if err != nil {
		return err
	}

	return send(eventstream.Event("event", "InvokeComplete", encoded))
}

type invokeCompletion struct {
	ErrorCode    string `json:"ErrorCode,omitempty"`
	ErrorDetails string `json:"ErrorDetails,omitempty"`
	LogResult    string `json:"LogResult,omitempty"`
}

func sendPayloadChunks(body io.Reader, send func(eventstream.Message) error) error {
	buf := make([]byte, 32<<10)
	size := 0
	for {
		n, err := body.Read(buf)
		if n > 0 {
			size += n
			if size > maxStreamPayload {
				return errors.New("streamed response exceeds 200 MiB")
			}

			if sendErr := send(eventstream.BinaryEvent("PayloadChunk", buf[:n])); sendErr != nil {
				return sendErr
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}
	}
}
