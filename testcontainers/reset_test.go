package tinystack_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	tinystack "github.com/amjadjibon/tinystack/testcontainers"
)

// endpointContainer supplies a real HTTP server in place of Docker for reset's
// request, error-response and cancellation checks.
type endpointContainer struct {
	testcontainers.Container
	endpoint string
}

func (c endpointContainer) PortEndpoint(_ context.Context, port, protocol string) (string, error) {
	if port != "4566/tcp" || protocol != "http" {
		return "", fmt.Errorf("unexpected endpoint: %s %s", protocol, port)
	}

	return c.endpoint, nil
}

func TestReset(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/_tinystack/reset", r.URL.RequestURI())
				w.WriteHeader(status)
				_, _ = w.Write([]byte("reset response"))
			}))
			defer server.Close()
			ctr := &tinystack.Container{Container: endpointContainer{endpoint: server.URL}}
			err := ctr.Reset(t.Context())
			if status == http.StatusOK {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "500 Internal Server Error: reset response")
			}

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, ctr.Reset(ctx), context.Canceled)
		})
	}
}
