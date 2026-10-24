package mlfixtures

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestStrictFixtureAnswers(t *testing.T) {
	type fixture struct {
		Answer *string `json:"answer"`
		Text   string  `json:"text"`
	}

	h := Handler(memory.New(nil, nil), "000000000000", "translate", func(f *fixture) error {
		if f.Text == "" || f.Answer == nil {
			return fmt.Errorf("text and answer required")
		}

		return nil
	})
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"text":"a","answer":"b"}`, 200},
		{`{"text":"a","answer":""}`, 200},
		{`{"text":"a","answer":null}`, 400},
		{`null`, 400},
		{`{"text":"a","unknown":"b"}`, 400},
		{`{"text":"a","answer":"b"} {}`, 400},
	} {
		r := httptest.NewRequest(
			"PUT",
			"http://localhost/_tinystack/translate/fixtures/test",
			strings.NewReader(tc.body),
		)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body)
		}
	}
}
