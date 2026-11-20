package integration_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda/local"
)

func TestLambdaResponseStreaming(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is absent")
	}

	s := serverWith(t, local.New(local.Options{}))
	c := lambdaClient(s, "test")
	handler := `
exports.handler = awslambda.streamifyResponse(async (event, stream, context) => {
  console.log('stream log');
  if (event.rawPath) {
    stream = awslambda.HttpResponseStream.from(stream, {
      statusCode: 201, headers: { 'Content-Type': 'text/plain', 'X-Stream': 'yes' },
      cookies: ['x=y', 'a=b']
    });
  }
  stream.write(context.clientContext?.custom?.first ?? 'first');
  const path = event.rawPath ?? '/sdk';
  const response = await fetch(process.env.GATE + path, { method: 'POST' });
  if (!response.ok) throw new Error('gate request failed');
  if (path === '/error') throw new TypeError('midstream failure');
  stream.end('last');
});`

	gates := map[string]chan struct{}{
		"/sdk":   make(chan struct{}),
		"/error": make(chan struct{}),
		"/url":   make(chan struct{}),
	}
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := gates[r.URL.Path]
		if r.Method != http.MethodPost || release == nil {
			http.NotFound(w, r)
			return
		}

		select {
		case <-release:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(gate.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
		FunctionName: new("streaming"), Runtime: lambdatypes.RuntimeNodejs22x, Handler: new("app.handler"),
		Role: new("arn:aws:iam::000000000000:role/lambda"), Timeout: new(int32(10)), Publish: true,
		Code:        &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.cjs", handler)},
		Environment: &lambdatypes.Environment{Variables: map[string]string{"GATE": gate.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.InvokeWithResponseStream(ctx, &sdklambda.InvokeWithResponseStreamInput{
		FunctionName: new("streaming"), Qualifier: new("1"), LogType: lambdatypes.LogTypeTail,
		ClientContext: new(base64.StdEncoding.EncodeToString([]byte(`{"custom":{"first":"client context"}}`))),
	})
	if err != nil {
		t.Fatal(err)
	}

	if out.StatusCode != 200 || out.ExecutedVersion == nil || *out.ExecutedVersion != "1" {
		t.Fatalf("unexpected response: %+v", out)
	}

	stream := out.GetStream()
	defer func() { _ = stream.Close() }()
	first, ok := (<-stream.Events()).(*lambdatypes.InvokeWithResponseStreamResponseEventMemberPayloadChunk)
	if !ok || string(first.Value.Payload) != "client context" {
		t.Fatalf("first chunk: %v, stream error: %v", first, stream.Err())
	}

	close(gates["/sdk"])
	body, completion := readLambdaStream(t, stream)
	if completion.LogResult == nil {
		t.Fatal("expected a log tail in the completion event")
	}

	log, err := base64.StdEncoding.DecodeString(*completion.LogResult)
	if body != "last" || completion.ErrorCode != nil || err != nil || !strings.Contains(string(log), "stream log") {
		t.Fatalf("remaining body %q, completion %+v, log %q, decode error %v", body, completion, log, err)
	}

	web, err := c.CreateFunctionUrlConfig(ctx, &sdklambda.CreateFunctionUrlConfigInput{
		FunctionName: new("streaming"), AuthType: lambdatypes.FunctionUrlAuthTypeNone,
		InvokeMode: lambdatypes.InvokeModeResponseStream,
		Cors:       &lambdatypes.Cors{AllowOrigins: []string{"https://example.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := url.Parse(*web.FunctionUrl)
	if err != nil {
		t.Fatal(err)
	}

	id, _, _ := strings.Cut(parsed.Hostname(), ".")
	for _, route := range []string{"/url", "/error"} {
		t.Run(route, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/_aws/lambda-url/"+id+route, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("Origin", "https://example.test")
			resp, err := s.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = resp.Body.Close() }()
			headers := resp.Header
			if resp.StatusCode != 201 || headers.Get("X-Stream") != "yes" || len(resp.Cookies()) != 2 ||
				headers.Get("Content-Type") != "text/plain" ||
				headers.Get("Access-Control-Allow-Origin") != "https://example.test" {
				t.Fatalf("unexpected URL response: %d %v", resp.StatusCode, headers)
			}

			var first [5]byte
			if _, readErr := io.ReadFull(resp.Body, first[:]); readErr != nil || string(first[:]) != "first" {
				t.Fatalf("first URL chunk %q: %v", first, readErr)
			}

			close(gates[route])
			body, err := io.ReadAll(resp.Body)
			if route == "/error" {
				if err == nil {
					t.Fatal("expected an interrupted stream")
				}
			} else if err != nil || string(body) != "last" {
				t.Fatalf("remaining body %q: %v", body, err)
			}
		})
	}
}

func readLambdaStream(
	t *testing.T, stream *sdklambda.InvokeWithResponseStreamEventStream,
) (string, lambdatypes.InvokeWithResponseStreamCompleteEvent) {
	t.Helper()
	var body strings.Builder
	var completion *lambdatypes.InvokeWithResponseStreamCompleteEvent
	for value := range stream.Events() {
		switch event := value.(type) {
		case *lambdatypes.InvokeWithResponseStreamResponseEventMemberPayloadChunk:
			if completion != nil {
				t.Fatal("received payload after completion")
			}

			body.Write(event.Value.Payload)
		case *lambdatypes.InvokeWithResponseStreamResponseEventMemberInvokeComplete:
			if completion != nil {
				t.Fatal("received two completion events")
			}

			completion = &event.Value
		default:
			t.Fatalf("unexpected event %T", event)
		}
	}

	if err := stream.Err(); err != nil || completion == nil {
		t.Fatalf("missing completion event: %v", err)
	}

	return body.String(), *completion
}

func TestLambdaResponseStreamingCompletion(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is absent")
	}

	c := lambdaClient(serverWith(t, local.New(local.Options{})), "test")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name, source, body, errorCode string
	}{
		{"buffered", "exports.handler = async () => ({value: true});", `{"value":true}`, ""},
		{"failed", "exports.handler = async () => { throw new RangeError('failed'); };", "", "RangeError"},
		{
			"midstream",
			"exports.handler = awslambda.streamifyResponse(async (_, stream) => {" +
				" stream.write('partial'); throw new TypeError('failed'); });",
			"partial", "TypeError",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateFunction(ctx, &sdklambda.CreateFunctionInput{
				FunctionName: new(tc.name), Runtime: lambdatypes.RuntimeNodejs22x, Handler: new("app.handler"),
				Role: new("arn:aws:iam::000000000000:role/lambda"),
				Code: &lambdatypes.FunctionCode{ZipFile: zipOf(t, "app.cjs", tc.source)},
			})
			if err != nil {
				t.Fatal(err)
			}

			out, err := c.InvokeWithResponseStream(ctx, &sdklambda.InvokeWithResponseStreamInput{
				FunctionName: new(tc.name),
			})
			if err != nil {
				t.Fatal(err)
			}

			stream := out.GetStream()
			defer func() { _ = stream.Close() }()
			body, completion := readLambdaStream(t, stream)
			switch {
			case body != tc.body:
				t.Errorf("body %q, want %q", body, tc.body)
			case tc.errorCode == "":
				if completion.ErrorCode != nil {
					t.Errorf("unexpected error: %+v", completion)
				}
			default:
				if completion.ErrorCode == nil || *completion.ErrorCode != tc.errorCode ||
					completion.ErrorDetails == nil || !strings.Contains(*completion.ErrorDetails, "failed") {
					t.Errorf("unexpected error: %+v", completion)
				}
			}
		})
	}

	dry, err := c.InvokeWithResponseStream(ctx, &sdklambda.InvokeWithResponseStreamInput{
		FunctionName: new("buffered"), InvocationType: lambdatypes.ResponseStreamingInvocationTypeDryRun,
	})
	if err != nil || dry.StatusCode != 204 {
		t.Fatalf("dry run: %v %v", dry, err)
	}

	if stream := dry.GetStream(); stream != nil {
		_ = stream.Close()
	}

	_, err = c.InvokeWithResponseStream(ctx, &sdklambda.InvokeWithResponseStreamInput{
		FunctionName: new("buffered"), InvocationType: "Event",
	})
	code(t, err, "InvalidParameterValueException")

	_, err = c.PutFunctionConcurrency(ctx, &sdklambda.PutFunctionConcurrencyInput{
		FunctionName: new("buffered"), ReservedConcurrentExecutions: new(int32(0)),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.InvokeWithResponseStream(ctx, &sdklambda.InvokeWithResponseStreamInput{FunctionName: new("buffered")})
	code(t, err, "TooManyRequestsException")
}
