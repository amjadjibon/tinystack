package restjson

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/eventstream"
)

// wrapper matches gateway instrumentation: flushing must follow Unwrap.
type streamWrapper struct{ http.ResponseWriter }

func (w streamWrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type flushRecorder struct {
	*httptest.ResponseRecorder
	frames int
}

func (w *flushRecorder) Flush() { w.frames++; w.ResponseRecorder.Flush() }

func TestWriteEventStream(t *testing.T) {
	stream := eventstream.Stream{
		eventstream.Event("event", "delta", []byte(`{"text":"hello"}`)),
		eventstream.Event("exception", "modelStreamErrorException", []byte(`{"message":"failed"}`)),
	}
	output := &struct{ Body *eventstream.Stream }{Body: &stream}
	binding := &catalog.Binding{
		StatusCode: 200,
		Output:     map[string]catalog.MemberBinding{"Body": {Location: "payload", Name: "body"}},
	}
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	Write(streamWrapper{recorder}, binding, output)
	if recorder.frames != 2 || recorder.Header().Get("Content-Type") != "application/vnd.amazon.eventstream" ||
		recorder.Header().Get("Content-Length") != "" {
		t.Fatalf("frames=%d headers=%v", recorder.frames, recorder.Header())
	}

	decoder := sdk.NewDecoder()
	for _, want := range stream {
		got, err := decoder.Decode(recorder.Body, nil)
		if err != nil || !bytes.Equal(got.Payload, want.Payload) ||
			got.Headers.Get(":message-type").String() != want.Headers.Get(":message-type").String() {
			t.Fatalf("event: %#v %v", got, err)
		}
	}

	if _, err := decoder.Decode(recorder.Body, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
}
