package lambda

import (
	"maps"
	"testing"
)

func TestBatchItemFailures(t *testing.T) {
	ids := map[string]bool{"1": true, "2": true}
	for _, tc := range []struct {
		response string
		failed   []string
		ok       bool
	}{
		{"", nil, true},
		{"null", nil, true},
		{`"done"`, nil, true},
		{`{}`, nil, true},
		{`{"batchItemFailures":null}`, nil, true},
		{`{"batchItemFailures":[]}`, nil, true},
		{`{"batchItemFailures":[{"itemIdentifier":"2"}]}`, []string{"2"}, true},
		{`{"batchItemFailures":[{"itemIdentifier":"3"}]}`, nil, false},
		{`{"batchItemFailures":[{"itemIdentifier":""}]}`, nil, false},
		{`{"batchItemFailures":[{"itemIdentifier":null}]}`, nil, false},
		{`{"batchItemFailures":[{"id":"1"}]}`, nil, false},
		{`{"batchItemFailures":"1"}`, nil, false},
		{`{"batchItemFailures":[`, nil, false},
	} {
		failed, ok := batchItemFailures([]byte(tc.response), ids)
		want := map[string]bool{}
		for _, id := range tc.failed {
			want[id] = true
		}

		if ok != tc.ok || !maps.Equal(failed, want) {
			t.Errorf("%s: got %v, %v", tc.response, failed, ok)
		}
	}
}
