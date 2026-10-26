package gateway

import (
	"net/http"
	"net/url"
	"testing"
)

func TestGreedyLabelBeforeLiteralSuffix(t *testing.T) {
	rt := route{
		method:   http.MethodGet,
		segments: []string{"v20180820", "mrap", "instances", "{Mrap+}", "routes"},
		greedy:   true,
		allowed:  map[string]bool{},
	}

	arn := "arn:aws:s3::000000000000:accesspoint"
	labels, ok := rt.match(
		http.MethodGet,
		[]string{"v20180820", "mrap", "instances", arn, "example.mrap", "routes"},
		url.Values{},
		http.Header{},
	)
	if !ok || labels["Mrap"] != arn+"/example.mrap" {
		t.Fatalf("labels=%v match=%v", labels, ok)
	}

	catchall := rt
	catchall.segments = []string{"v20180820", "mrap", "instances", "{Name+}"}
	if rt.score() <= catchall.score() {
		t.Fatal("literal suffix must outrank catchall")
	}

	if _, ok := rt.match(
		http.MethodGet,
		[]string{"v20180820", "mrap", "instances", arn, "example.mrap", "other"},
		url.Values{},
		http.Header{},
	); ok {
		t.Fatal("wrong suffix matched")
	}
}
