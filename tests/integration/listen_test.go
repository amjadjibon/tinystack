package integration_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
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
	// Clients still call loopback, which TinyStack hands to containers as host.docker.internal; the listener's own
	// address ([::]) is not reachable from a container.
	s.URL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Listener.Addr().(*net.TCPAddr).Port))
	return s
}
