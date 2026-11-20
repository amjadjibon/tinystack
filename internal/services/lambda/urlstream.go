package lambda

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"golang.org/x/net/http/httpguts"

	"github.com/amjadjibon/tinystack/internal/aws/execute"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const maxURLPrelude = 16 << 10

var urlPreludeDelimiter = []byte{0, 0, 0, 0, 0, 0, 0, 0}

func (s *Service) streamURL(
	ctx context.Context, scope request.Scope, name, qualifier string, event []byte, cors *api.Cors, origin string,
) *execute.Response {
	return &execute.Response{Stream: func(send func(int, http.Header, io.Reader) error) error {
		started := false
		sink := func(contentType string, body io.Reader) error {
			status := http.StatusOK
			headers := make(http.Header)
			if contentType == HTTPIntegrationResponse {
				var err error
				status, headers, body, err = urlStreamPrelude(body)
				if err != nil {
					return err
				}
			} else if contentType != "" {
				headers.Set("Content-Type", contentType)
			}

			applyURLCors(headers, cors, origin)
			started = true
			if status == http.StatusNoContent || status == http.StatusNotModified {
				if err := send(status, headers, http.NoBody); err != nil {
					return err
				}

				_, err := io.Copy(io.Discard, body)
				return err
			}

			return send(status, headers, body)
		}

		var result Result
		fn, err := s.prepare(ctx, scope, name, qualifier)
		if err == nil {
			var invoke invokeFunction
			invoke, err = s.beginInvocation(ctx, scope, fn, qualifier)
			if err == nil {
				result, _, err = invoke(event, sink)
			}
		}

		if started {
			return invokeError(result, err)
		}

		// Older RIE versions return the entire response without the Runtime API headers.
		if err == nil && !result.Error &&
			bytes.Contains(result.Payload[:min(len(result.Payload), maxURLPrelude)], urlPreludeDelimiter) {
			err = sink(HTTPIntegrationResponse, bytes.NewReader(result.Payload))
			if started {
				return err
			}
		}

		resp := s.urlResponse(result, err)
		applyURLCors(resp.Header, cors, origin)
		return send(resp.Status, resp.Header, bytes.NewReader(resp.Body))
	}}
}

func urlStreamPrelude(body io.Reader) (int, http.Header, io.Reader, error) {
	invalid := errors.New("invalid Lambda streaming HTTP response metadata")
	reader := bufio.NewReaderSize(body, maxURLPrelude)
	raw, err := reader.ReadSlice(0)
	if err != nil {
		return 0, nil, nil, invalid
	}

	var meta struct {
		Headers    map[string]string `json:"headers"`
		Cookies    []string          `json:"cookies"`
		StatusCode int               `json:"statusCode"`
	}

	if err := json.Unmarshal(raw[:len(raw)-1], &meta); err != nil || len(raw) > maxURLPrelude-7 {
		return 0, nil, nil, invalid
	}

	var delimiter [7]byte
	if _, err := io.ReadFull(reader, delimiter[:]); err != nil || delimiter != [7]byte{} {
		return 0, nil, nil, invalid
	}

	status := meta.StatusCode
	if status == 0 {
		status = http.StatusOK
	}

	if status < 200 || status > 599 {
		return 0, nil, nil, invalid
	}

	headers := make(http.Header)
	for k, v := range meta.Headers {
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) {
			return 0, nil, nil, invalid
		}

		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Content-Length", "Keep-Alive", "Proxy-Connection", "Te", "Trailer",
			"Transfer-Encoding", "Upgrade":
		default:
			headers.Set(k, v)
		}
	}

	for _, cookie := range meta.Cookies {
		if !httpguts.ValidHeaderFieldValue(cookie) {
			return 0, nil, nil, invalid
		}

		headers.Add("Set-Cookie", cookie)
	}

	return status, headers, reader, nil
}
