package bedrockruntime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestOllamaConcurrentPull(t *testing.T) {
	var ready atomic.Bool
	var pulls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}

		switch r.URL.Path {
		case "/api/pull":
			pulls.Add(1)
			ready.Store(true)
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/chat":
			if !ready.Load() {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"error":"missing"}`)
				return
			}

			fmt.Fprint(
				w,
				`{"message":{"content":"done"},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":1}`,
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	s := &Service{}
	cfg := config.DefaultBedrock().Ollama
	cfg.URL = fake.URL
	s.UseOllama(cfg, "test")
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			out := &reply{}
			err := s.infer(
				context.Background(),
				"anthropic.claude-test",
				out,
				&ollamaRequest{Options: map[string]any{"stop": []string{"END"}}},
			)
			if err != nil {
				t.Error(err)
				return
			}

			if out.Stop != stopEndTurn || out.Input != 3 || out.Output != 1 {
				t.Errorf("reply: %+v", out)
			}
		})
	}

	wg.Wait()
	if pulls.Load() != 1 {
		t.Fatalf("pulls=%d", pulls.Load())
	}
}

func TestOllamaTimeout(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}

		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
		case <-timer.C:
		}
	}))
	defer fake.Close()
	cfg := config.DefaultBedrock().Ollama
	cfg.URL = fake.URL
	cfg.Timeout = 20 * time.Millisecond
	s := &Service{}
	s.UseOllama(cfg, "test")
	_, _, err := s.ollama.call(context.Background(), "anthropic.claude-test", &ollamaRequest{})
	if err == nil {
		t.Fatal("timeout succeeded")
	}
}
