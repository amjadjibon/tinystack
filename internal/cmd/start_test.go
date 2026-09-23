package cmd

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestStartAndCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lc net.ListenConfig
	probe, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := probe.Addr().(*net.TCPAddr).Port
	if guardErr := probe.Close(); guardErr != nil {
		t.Fatal(guardErr)
	}

	cmd := newRootCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"start", "--host", "127.0.0.1", "--port", strconv.Itoa(port)})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	ready := false
	for range 100 {
		req, err := http.NewRequestWithContext(
			ctx,
			"GET",
			"http://127.0.0.1:"+strconv.Itoa(port)+"/_tinystack/health",
			http.NoBody,
		)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err == nil {
			ready = resp.StatusCode == 200
			if guardErr := resp.Body.Close(); guardErr != nil {
				t.Fatal(guardErr)
			}

			if ready {
				break
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown timed out")
	}

	if !ready {
		t.Fatal("server never became ready")
	}
}

func TestInvalidOptions(t *testing.T) {
	for _, args := range [][]string{
		{"start", "--port", "0"},
		{"start", "--config", "does-not-exist"},
		{"start", "--port", "65536"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			cmd := newRootCommand()
			cmd.SetArgs(args)
			if guardErr := cmd.Execute(); guardErr == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
}
