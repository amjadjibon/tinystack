package docker

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

func TestPostStreamsRIEResponse(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != invocations {
			http.NotFound(w, req)
			return
		}

		if req.Header.Get("X-Amz-Client-Context") != base64.StdEncoding.EncodeToString([]byte(`{"custom":{}}`)) {
			http.Error(w, "missing client context", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set(lambda.RuntimeResponseMode, "Streaming")
		w.Header().Set("Trailer", lambda.StreamErrorType+", "+lambda.StreamErrorBody)
		_, _ = io.WriteString(w, "first")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
			w.Header().Set(lambda.StreamErrorType, "Runtime.CustomError")
			w.Header().
				Set(lambda.StreamErrorBody, base64.StdEncoding.EncodeToString([]byte(`{"errorMessage":"failed"}`)))
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	r := New()
	fn := &lambda.Function{Timeout: time.Second, ClientContext: `{"custom":{}}`}
	c := &container{endpoint: strings.TrimPrefix(server.URL, "http://")}
	result, healthy, err := r.post(ctx, fn, c, []byte("{}"), func(contentType string, body io.Reader) error {
		var first [5]byte
		if _, err := io.ReadFull(body, first[:]); err != nil {
			return err
		}

		if contentType != "text/plain" || string(first[:]) != "first" {
			t.Errorf("unexpected first chunk %q with content type %q", first, contentType)
		}

		close(release)
		_, err := io.Copy(io.Discard, body)
		return err
	})
	if err != nil || !healthy || !result.Error || !strings.Contains(string(result.Payload), "Runtime.CustomError") {
		t.Fatalf("stream result: %s, healthy=%t, err=%v", result.Payload, healthy, err)
	}
}
