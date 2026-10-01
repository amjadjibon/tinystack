package k3s

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/services/eks"
)

// TestRuntime runs a cluster in k3s and calls its API through the endpoint;
// it only runs with TINYSTACK_DOCKER_TESTS=1.
func TestRuntime(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run clusters in k3s")
	}

	ctx := context.Background()
	// The token "good" is from account 111111111111, and any other from
	// 222222222222.
	r, err := New(func(req *http.Request, cluster string) (string, error) {
		switch {
		case cluster != "demo":
			return "", errors.New("wrong cluster")
		case req.Header.Get("Authorization") == "Bearer good":
			return "111111111111", nil
		default:
			return "222222222222", nil
		}
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(r.Close)
	if _, err = r.Start(ctx, eks.Cluster{Account: "111111111111", Name: "demo", Version: "1.29"}); err == nil {
		t.Fatal("started an unsupported version")
	}

	server, err := r.Start(ctx, eks.Cluster{Account: "111111111111", Name: "demo", Version: "1.34"})
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(server.CA))
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	get := func(token string) (int, string) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/version", http.NoBody)
		if reqErr != nil {
			t.Fatal(reqErr)
		}

		req.Header.Set("Authorization", "Bearer "+token)
		resp, getErr := client.Do(req)
		if getErr != nil {
			t.Fatal(getErr)
		}

		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := get("good"); code != http.StatusOK || !strings.Contains(body, "v1.34") {
		t.Fatalf("version = %d %s", code, body)
	}

	if code, _ := get("other"); code != http.StatusUnauthorized {
		t.Fatalf("other account = %d", code)
	}

	if running, runErr := r.Running(ctx, server.ID); runErr != nil || !running {
		t.Fatalf("running = %v, %v", running, runErr)
	}

	r.Remove(ctx, server.ID)
	if running, runErr := r.Running(ctx, server.ID); runErr != nil || running {
		t.Fatalf("running after remove = %v, %v", running, runErr)
	}
}
