package integration_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/localtls"
)

type tlsReadyHandler struct {
	slog.Handler
	endpoints chan string
}

//nolint:gocritic // slog.Handler requires a value Record.
func (h *tlsReadyHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "https ready" {
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "endpoint" {
				h.endpoints <- attr.Value.String()
			}

			return true
		})
	}

	return h.Handler.Handle(ctx, record)
}

func TestHTTPSLocalCA(t *testing.T) {
	cfg := config.Default()
	// The main listener requires a nonzero port; reserve an ephemeral one
	// briefly, then hand it to app.Run. The HTTPS listener binds port 0 directly.
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	cfg.Server.Port = reservation.Addr().(*net.TCPAddr).Port
	if err = reservation.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.Server.ConsolePort = 0
	cfg.TLS.Enabled = true
	cfg.TLS.Address = "127.0.0.1:0"
	cfg.Storage.Mode = config.StoragePersistent
	cfg.Storage.Path = t.TempDir()
	cfg.Lambda.Executor = config.ExecutorLocal
	cfg.Init.Dir = ""
	cfg.Init.LocalStackDir = ""
	endpoints := make(chan string, 1)
	logger := slog.New(&tlsReadyHandler{Handler: slog.NewTextHandler(io.Discard, nil), endpoints: endpoints})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, cfg, logger); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case appErr := <-done:
			if appErr != nil {
				t.Errorf("app shutdown: %v", appErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("app did not shut down")
		}
	})
	var endpoint string
	select {
	case endpoint = <-endpoints:
	case appErr := <-done:
		t.Fatalf("startup: %v", appErr)
	case <-time.After(10 * time.Second):
		t.Fatal("HTTPS did not start")
	}

	authority, err := localtls.Load(cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority.PEM) {
		t.Fatal("invalid exported CA")
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			ServerName: "sqs.us-east-1.amazonaws.com",
			MinVersion: tls.VersionTLS12,
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, strings.TrimPrefix(endpoint, "https://"))
		},
	}

	t.Cleanup(transport.CloseIdleConnections)
	// No endpoint override: the SDK keeps its real AWS Host and HTTPS scheme.
	client := sqs.NewFromConfig(aws.Config{
		Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient: &http.Client{Transport: transport, Timeout: 5 * time.Second},
	})
	output, err := client.CreateQueue(t.Context(), &sqs.CreateQueueInput{QueueName: new("https-local-ca")})
	if err != nil {
		t.Fatal(err)
	}

	if output.QueueUrl == nil || !strings.Contains(*output.QueueUrl, "https-local-ca") {
		t.Fatalf("unexpected CreateQueue output: %+v", output)
	}
}
