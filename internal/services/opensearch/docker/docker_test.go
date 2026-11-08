package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/amjadjibon/tinystack/internal/services/opensearch"
)

func TestRunArgs(t *testing.T) {
	args := runArgs(&opensearch.Spec{
		Owner:    "store",
		ARN:      "arn:aws:es:us-east-1:000000000000:domain/logs",
		Name:     "tinystack-opensearch-abc",
		Volume:   "tinystack-opensearch-abc-data",
		Image:    "opensearchproject/opensearch:2.11.0",
		DataPath: "/usr/share/opensearch/data",
		Env:      []string{"discovery.type=single-node", "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m"},
	})

	want := []string{
		"run", "-d", "--name", "tinystack-opensearch-abc",
		"--label", "tinystack.opensearch=arn:aws:es:us-east-1:000000000000:domain/logs",
		"--label", "tinystack.opensearch.owner=store",
		"-p", "127.0.0.1::9200/tcp",
		"-e", "discovery.type=single-node", "-e", "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m",
		"-v", "tinystack-opensearch-abc-data:/usr/share/opensearch/data",
		"--", "opensearchproject/opensearch:2.11.0",
	}

	if !slices.Equal(args, want) {
		t.Fatalf("docker run arguments\n got %q\nwant %q", args, want)
	}
}

func TestProbe(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Local tools probe new listeners with GET /; only the readiness probe is checked.
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if r.URL.Path != "/_cluster/health" || r.URL.Query().Get("wait_for_status") != "yellow" {
			t.Errorf("unexpected probe request %s", r.URL)
		}

		w.WriteHeader(status)
	}))
	defer server.Close()

	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	port, err := strconv.ParseInt(address.Port(), 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	rt := New()
	if err = rt.Probe(context.Background(), port); err != nil {
		t.Fatalf("a healthy engine: %v", err)
	}

	// A cluster that has not reached yellow answers 408 when wait_for_status times out.
	status = http.StatusRequestTimeout
	if err = rt.Probe(context.Background(), port); err == nil {
		t.Fatal("expected a cluster that is not yellow to fail the probe")
	}

	server.Close()
	if err = rt.Probe(context.Background(), port); err == nil {
		t.Fatal("expected an engine that does not listen to fail the probe")
	}
}

func TestClosedRuntimeRefusesContainers(t *testing.T) {
	rt := New()
	rt.Close()
	if _, err := rt.Ensure(context.Background(), &opensearch.Spec{Name: "x"}); err == nil {
		t.Fatal("expected a closed runtime to refuse new containers")
	}
}
