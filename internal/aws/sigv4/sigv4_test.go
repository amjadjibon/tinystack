package sigv4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// TestVector checks the get-vanilla case of the AWS SigV4 test suite.
func TestVector(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.amazonaws.com/", http.NoBody)
	r.Header.Set("X-Amz-Date", "20150830T123600Z")
	a, err := Parse("AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31")
	if err != nil {
		t.Fatal(err)
	}

	empty := sha256.Sum256(nil)
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	err = Verify(r, a, "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", hex.EncodeToString(empty[:]), false, now)
	if err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	a, err := Parse("AWS4-HMAC-SHA256 Credential=key/20260924/eu-west-1/sqs/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc")
	if err != nil || a.AccessKey != "key" || a.Date != "20260924" || a.Region != "eu-west-1" || a.Service != "sqs" ||
		strings.Join(a.SignedHeaders, ";") != "host;x-amz-date" || a.Signature != "abc" {
		t.Fatalf("%+v %v", a, err)
	}

	for _, header := range []string{
		"",
		"AWS key:signature",
		"AWS4-HMAC-SHA256 Credential=key/2026/eu-west-1/sqs/aws4_request",
		"AWS4-HMAC-SHA256 Credential=key/20260924/eu-west-1/sqs",
		"AWS4-HMAC-SHA256 SignedHeaders=host, Signature=abc",
	} {
		if _, err = Parse(header); err == nil {
			t.Fatalf("parsed %q", header)
		}
	}
}

// sign sends a request signed by the AWS SDK signer to a server that verifies
// it with secret, and returns the verification error.
func sign(t *testing.T, s3 bool, at time.Time, secret string, tamper func(*http.Request)) error {
	t.Helper()
	body := `{"name": "a b"}`
	sum := sha256.Sum256([]byte(body))
	payloadHash := hex.EncodeToString(sum[:])
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, err := Parse(r.Header.Get("Authorization"))
		if err == nil {
			err = Verify(r, a, secret, payloadHash, s3, time.Now())
		}

		result <- err
	}))
	defer server.Close()

	path := "/2015-03-31/functions/arn%3Aaws%3Alambda%3Aus-east-1%3A000000000000%3Afunction%3Af/a%20b~c/"
	query := "?b=2&a=x%20y&a=1&slash=%2F&empty&plus=%2B"
	r, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		server.URL+path+query,
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("X-Amz-Meta-Spaced", "  one   two  ")
	r.Header.Add("X-Amz-Meta-Multi", "first")
	r.Header.Add("X-Amz-Meta-Multi", "second")
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = s3 })
	credentials := aws.Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}
	if err = signer.SignHTTP(context.Background(), credentials, r, payloadHash, "lambda", "us-east-1", at); err != nil {
		t.Fatal(err)
	}

	if tamper != nil {
		tamper(r)
	}

	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}

	_ = response.Body.Close()
	return <-result
}

func TestVerifySDKSignatures(t *testing.T) {
	for _, s3 := range []bool{false, true} {
		if err := sign(t, s3, time.Now(), "secret", nil); err != nil {
			t.Fatalf("s3=%v: %v", s3, err)
		}
	}

	for name, tc := range map[string]struct {
		at     time.Time
		want   error
		tamper func(*http.Request)
		secret string
	}{
		"wrong secret": {secret: "other", want: ErrMismatch},
		"old request":  {at: time.Now().Add(-MaxSkew - time.Minute), want: ErrExpired},
		"future request": {
			at: time.Now().Add(MaxSkew + time.Minute), want: ErrExpired,
		},
		"changed query": {tamper: func(r *http.Request) { r.URL.RawQuery += "&c=3" }, want: ErrMismatch},
		"changed path":  {tamper: func(r *http.Request) { r.URL.Path += "x"; r.URL.RawPath = "" }, want: ErrMismatch},
		"changed header": {
			tamper: func(r *http.Request) { r.Header.Set("X-Amz-Meta-Multi", "third") }, want: ErrMismatch,
		},
		"unsigned date": {
			tamper: func(r *http.Request) { r.Header.Del("X-Amz-Date") }, want: ErrMalformed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			secret, at := tc.secret, tc.at
			if secret == "" {
				secret = "secret"
			}

			if at.IsZero() {
				at = time.Now()
			}

			if err := sign(t, false, at, secret, tc.tamper); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRequiresHost(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.amazonaws.com/", http.NoBody)
	r.Header.Set("X-Amz-Date", "20150830T123600Z")
	a := &Authorization{
		AccessKey: "key", Date: "20150830", Region: "us-east-1", Service: "service",
		SignedHeaders: []string{"x-amz-date"}, Signature: "abc",
	}

	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	if err := Verify(r, a, "secret", "", false, now); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}

	a.SignedHeaders = []string{"host", "x-amz-date"}
	a.Date = "20150831"
	if err := Verify(r, a, "secret", "", false, now); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
}
