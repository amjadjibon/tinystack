package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amjadjibon/tinystack/internal/aws/sigv4"
)

func signedChunkRequest(ctx context.Context, t *testing.T, endpoint, damage string, trailer bool) *http.Request {
	t.Helper()

	chunks := []string{"first chunk", "second chunk", ""}
	data := strings.Join(chunks, "")
	length := 2 // final CRLF
	for _, chunk := range chunks {
		length += len(strconv.FormatInt(int64(len(chunk)), 16)) + len(";chunk-signature=") + 64 + 2 + len(chunk)
		if chunk != "" {
			length += 2
		}
	}

	checksumLine := "x-amz-checksum-crc32:" + wantCRC32([]byte(data))
	mode := sigv4.StreamingPayload
	if trailer {
		mode = sigv4.StreamingPayloadTrailer
		length += len(checksumLine) + 2 + len("x-amz-trailer-signature:") + 64 + 2
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	r.ContentLength = int64(length)
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Content-Sha256", mode)
	r.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(data)))
	if trailer {
		r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	}

	at := time.Now().UTC()
	signedWithHeader(t, r, at)
	_, seed, _ := strings.Cut(r.Header.Get("Authorization"), "Signature=")
	seedBytes, err := hex.DecodeString(seed)
	if err != nil {
		t.Fatal(err)
	}

	credentials := aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}
	signer := v4.NewStreamSigner(credentials, "s3", "us-east-1", seedBytes)
	var wire strings.Builder
	var last string
	for _, chunk := range chunks {
		sig, err := signer.GetSignature(ctx, nil, []byte(chunk), at)
		if err != nil {
			t.Fatal(err)
		}

		last = hex.EncodeToString(sig)
		signature := last
		if damage == "final" && chunk == "" {
			signature = strings.Repeat("0", 64)
		}

		fmt.Fprintf(&wire, "%x;chunk-signature=%s\r\n", len(chunk), signature)
		if chunk != "" {
			wire.WriteString(chunk + "\r\n")
		}
	}

	if trailer {
		date := at.Format("20060102")
		sum := sha256.Sum256([]byte(checksumLine + "\n"))
		input := fmt.Sprintf("AWS4-HMAC-SHA256-TRAILER\n%s\n%s/us-east-1/s3/aws4_request\n%s\n%x",
			at.Format("20060102T150405Z"), date, last, sum)
		key := sigv4.SigningKey("test", date, "us-east-1", "s3")
		signature := hex.EncodeToString(sigv4.HMACSHA256(key, input))
		if damage == "trailer" {
			signature = strings.Repeat("0", 64)
		}

		fmt.Fprintf(&wire, "%s\r\nx-amz-trailer-signature:%s\r\n", checksumLine, signature)
	}

	wire.WriteString("\r\n")
	body := wire.String()
	if damage == "body" {
		body = strings.Replace(body, "first chunk", "FIRST CHUNK", 1)
	}

	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

func TestS3SignedChunkedUploads(t *testing.T) {
	for _, mode := range []string{"verify", "default"} {
		t.Run(mode, func(t *testing.T) {
			var srv *httptest.Server
			if mode == "verify" {
				srv = verifyingServer(t)
			} else {
				srv = server(t)
			}

			c, bucket := checksumBucket(t, srv, noAutoChecksums)
			ctx := context.Background()
			for _, tc := range []struct {
				name, damage string
				trailer      bool
			}{
				{name: "plain"},
				{name: "with-trailer", trailer: true},
				{name: "body-signature", damage: "body"},
				{name: "final-signature", damage: "final"},
				{name: "trailer-signature", damage: "trailer", trailer: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := signedChunkRequest(ctx, t, srv.URL+"/"+*bucket+"/"+tc.name, tc.damage, tc.trailer)
					status, res := send(t, srv, r)
					if mode == "verify" && tc.damage != "" {
						if status != http.StatusForbidden || !strings.Contains(res, "SignatureDoesNotMatch") {
							t.Fatalf("expected signature failure, got %d %s", status, res)
						}

						_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: new(tc.name)})
						if _, code := s3Code(err); code != http.StatusNotFound {
							t.Fatalf("failed request left an object: %v", err)
						}

						return
					}

					if status != http.StatusOK {
						t.Fatalf("upload returned %d %s", status, res)
					}

					out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: new(tc.name)})
					if err != nil {
						t.Fatal(err)
					}

					defer out.Body.Close()
					got, err := io.ReadAll(out.Body)
					if err != nil {
						t.Fatal(err)
					}

					want := "first chunksecond chunk"
					if tc.damage == "body" {
						want = "FIRST CHUNKsecond chunk"
					}

					if string(got) != want {
						t.Fatalf("decoded body = %q, want %q", got, want)
					}
				})
			}
		})
	}
}
