package integration_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
)

// startServer serves handler like httptest.NewServer. With Docker tests on Linux it listens on every interface,
// since containers reach TinyStack through host.docker.internal, which there is the Docker bridge, not loopback.
func startServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	if os.Getenv("TINYSTACK_DOCKER_TESTS") == "1" && runtime.GOOS == "linux" {
		_ = s.Listener.Close()
		listener, err := net.Listen("tcp", "0.0.0.0:0") // #nosec G102 -- containers on the bridge must connect.
		if err != nil {
			t.Fatal(err)
		}

		s.Listener = listener
	}

	s.Start()
	return s
}
