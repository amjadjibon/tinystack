package docker

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/services/rds"
)

func TestMongoReadinessCommand(t *testing.T) {
	for _, version := range []string{"3.6", "4.0", "5.0"} {
		shell := "mongo"
		if version == "5.0" {
			shell = "mongosh"
		}

		args := mongoCommand(
			&rds.Spec{Image: "mongo:" + version, Name: "test", Username: "master", Password: "password"},
			"db.adminCommand({ping:1})",
		)
		if args[2] != shell || !slices.Contains(args, "--authenticationDatabase") ||
			!slices.Contains(args, "password") {
			t.Fatal(args)
		}
	}
}

func TestGremlinReadiness(t *testing.T) {
	for _, gremlinCode := range []int{200, 500} {
		t.Run(strconv.Itoa(gremlinCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Local tools probe new listeners with GET /; only the readiness query is checked.
				if r.Method == http.MethodGet && r.URL.Path == "/" {
					w.WriteHeader(http.StatusNotFound)
					return
				}

				if r.Method != http.MethodPost || r.URL.Path != "/gremlin" ||
					r.Header.Get("Content-Type") != "application/json" {
					t.Error("invalid readiness request", r.Method, r.URL, r.Header)
				}

				var query map[string]string
				if err := json.NewDecoder(r.Body).Decode(&query); err != nil || query["gremlin"] != "g.inject(1)" {
					t.Error(query, err)
				}

				_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]int{"code": gremlinCode}})
			}))
			defer server.Close()
			_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}

			number, err := strconv.ParseInt(port, 10, 64)
			if err != nil {
				t.Fatal(err)
			}

			err = probeGremlin(t.Context(), number)
			if (err == nil) != (gremlinCode == 200) {
				t.Fatal("incorrect readiness", gremlinCode, err)
			}
		})
	}
}
