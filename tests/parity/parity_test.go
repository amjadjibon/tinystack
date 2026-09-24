package parity_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type step struct {
	Name      string          `json:"name"`
	Operation string          `json:"operation"`
	Request   json.RawMessage `json:"request"`
}

type scenario struct {
	Steps []step `json:"steps"`
}

type response struct {
	ContentType string          `json:"content_type"`
	ErrorCode   string          `json:"error_code,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	Status      int             `json:"status"`
}

type fixture struct {
	Responses  map[string]response `json:"responses"`
	Provenance string              `json:"provenance"`
}

//go:embed dynamodb/*.json
var fixtures embed.FS

func loadJSON(t *testing.T, name string, out any) {
	t.Helper()
	data, err := fixtures.ReadFile("dynamodb/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func requestBody(raw json.RawMessage, table string) []byte {
	return bytes.ReplaceAll(raw, []byte("__TABLE__"), []byte(table))
}

func call(
	ctx context.Context,
	client *http.Client,
	endpoint, table string,
	step step,
	signer *v4.Signer,
) (response, error) {
	body := requestBody(step.Request, table)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}

	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+step.Operation)
	if signer != nil {
		hash := sha256.Sum256(body)
		creds := aws.Credentials{
			AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
		}
		err = signer.SignHTTP(
			ctx,
			creds,
			req,
			hex.EncodeToString(hash[:]),
			"dynamodb",
			os.Getenv("AWS_REGION"),
			time.Now().UTC(),
		)
		if err != nil {
			return response{}, err
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}

	out := response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: data}
	if resp.StatusCode >= 400 {
		var body struct {
			Type string `json:"__type"`
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &body)
		out.ErrorCode = strings.Split(strings.Split(body.Type, "#")[len(strings.Split(body.Type, "#"))-1], ":")[0]
		if out.ErrorCode == "" {
			out.ErrorCode = body.Code
		}
	}

	return out, nil
}

func waitForTable(ctx context.Context, client *http.Client, endpoint, table string, signer *v4.Signer) error {
	for {
		got, err := call(
			ctx,
			client,
			endpoint,
			table,
			step{Operation: "DescribeTable", Request: json.RawMessage(`{"TableName":"__TABLE__"}`)},
			signer,
		)
		if err != nil {
			return err
		}
		var body struct {
			Table struct {
				TableStatus string `json:"TableStatus"`
			} `json:"Table"`
		}
		if got.Status == http.StatusOK &&
			json.Unmarshal(got.Body, &body) == nil &&
			body.Table.TableStatus == "ACTIVE" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func run(t *testing.T, endpoint, table string, signer *v4.Signer, compare bool, created *bool) map[string]response {
	t.Helper()
	var sc scenario
	loadJSON(t, "scenario.json", &sc)
	var expected fixture
	loadJSON(t, "expected.json", &expected)
	client := &http.Client{Timeout: 20 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out := map[string]response{}
	for _, s := range sc.Steps {
		got, err := call(ctx, client, endpoint, table, s, signer)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		if s.Name == "create-table" {
			if got.Status != http.StatusOK {
				t.Fatalf("create table: HTTP %d: %s", got.Status, got.Body)
			}
			*created = true
			if signer != nil {
				if err := waitForTable(ctx, client, endpoint, table, signer); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		if _, ok := expected.Responses[s.Name]; ok {
			out[s.Name] = got
			if compare {
				compareResponse(t, s.Name, got, expected.Responses[s.Name])
			}
		} else if got.Status != http.StatusOK {
			t.Fatalf("%s: HTTP %d: %s", s.Name, got.Status, got.Body)
		}
	}

	return out
}

func compareResponse(t *testing.T, name string, got, want response) {
	t.Helper()
	if got.Status != want.Status || got.ContentType != want.ContentType || got.ErrorCode != want.ErrorCode {
		t.Errorf(
			"%s wire: got HTTP %d %q error %q, want HTTP %d %q error %q",
			name,
			got.Status,
			got.ContentType,
			got.ErrorCode,
			want.Status,
			want.ContentType,
			want.ErrorCode,
		)
	}
	if len(want.Body) == 0 {
		return
	}

	var a, b any
	if err := json.Unmarshal(got.Body, &a); err != nil {
		t.Fatalf("%s body: %v", name, err)
	}

	if err := json.Unmarshal(want.Body, &b); err != nil {
		t.Fatalf("%s fixture: %v", name, err)
	}

	if !reflect.DeepEqual(a, b) {
		t.Errorf("%s body: got %s, want %s", name, got.Body, want.Body)
	}
}

func TestDynamoDBParity(t *testing.T) {
	cfg := config.Default()
	h, err := gateway.New(
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		api.Descriptor(dynamodb.New(&memory.Store{})),
	)
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(h)
	defer s.Close()

	var created bool
	run(t, s.URL, "tinystack-parity", nil, true, &created)
}

// TINYSTACK_PARITY_CAPTURE=1 opts into creating and deleting a real AWS table.
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and AWS_REGION must be explicit.
// The snapshot is written to the path named by TINYSTACK_PARITY_OUTPUT.
func TestCaptureDynamoDBAgainstAWS(t *testing.T) {
	if os.Getenv("TINYSTACK_PARITY_CAPTURE") != "1" {
		t.Skip("set TINYSTACK_PARITY_CAPTURE=1 to capture AWS")
	}
	for _, key := range []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_REGION",
		"TINYSTACK_PARITY_OUTPUT",
	} {
		if os.Getenv(key) == "" {
			t.Fatalf("%s is required", key)
		}
	}
	endpoint := "https://dynamodb." + os.Getenv("AWS_REGION") + ".amazonaws.com/"
	name := fmt.Sprintf("tinystack-parity-%d", time.Now().UnixNano())
	signer := v4.NewSigner()

	var created bool
	defer func() {
		if !created {
			return
		}
		got, err := call(
			context.Background(),
			&http.Client{Timeout: 20 * time.Second},
			endpoint,
			name,
			step{Operation: "DeleteTable", Request: json.RawMessage(`{"TableName":"__TABLE__"}`)},
			signer,
		)
		if err != nil || got.Status != http.StatusOK {
			t.Errorf("cleanup table %s: HTTP %d: %v %s", name, got.Status, err, got.Body)
		}
	}()

	responses := run(t, endpoint, name, signer, false, &created)
	snapshot := fixture{
		Provenance: "AWS DynamoDB " + os.Getenv("AWS_REGION") + " captured " + time.Now().UTC().Format(time.RFC3339),
		Responses:  responses,
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(os.Getenv("TINYSTACK_PARITY_OUTPUT"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
