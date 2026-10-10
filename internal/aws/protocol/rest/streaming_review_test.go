package rest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

type endlessChunkHeader struct{ bytes int }

func (r *endlessChunkHeader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = '0'
	}
	r.bytes += len(p)
	return len(p), nil
}

type truncatedUpload struct{ io.Reader }

func (r truncatedUpload) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestStreamBodyModelsTruncatedHTTPPayload(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/bucket/key", http.NoBody)
	r.Body = io.NopCloser(truncatedUpload{Reader: strings.NewReader("hello")})
	r.ContentLength = 10
	r.Header.Set("Content-Length", "10")
	stream, _, err := StreamBody(httptest.NewRecorder(), r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = io.Copy(io.Discard, stream)
	if e, ok := errors.AsType[*awserror.Error](err); !ok || e.Code != "IncompleteBody" {
		t.Fatalf("truncated HTTP upload should produce IncompleteBody, got %v", err)
	}
}

func TestChunkReaderRejectsUnboundedFramingLines(t *testing.T) {
	for _, prefix := range []string{"", "0\r\n"} {
		t.Run(prefix, func(t *testing.T) {
			r := &endlessChunkHeader{}
			reader := NewChunkReader(bufio.NewReader(io.MultiReader(
				strings.NewReader(prefix), io.LimitReader(r, 32<<20),
			)), make(http.Header))
			var payload [1024]byte
			_, err := reader.Read(payload[:])
			if err == nil {
				t.Fatal("unterminated framing accepted")
			}
			if r.bytes > 64<<10 {
				t.Fatalf("read %d framing bytes before rejecting an unbounded line", r.bytes)
			}
		})
	}
}

func TestStreamBodyRejectsMalformedDecodedContentLength(t *testing.T) {
	for _, val := range []string{"invalid", "-1", "-100", "9999999999999999999999999999999999999999"} {
		t.Run(val, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/bucket/key", strings.NewReader("0\r\n\r\n"))
			r.Header.Set("Content-Encoding", "aws-chunked")
			r.Header.Set("x-amz-decoded-content-length", val)
			_, _, err := StreamBody(httptest.NewRecorder(), r, 1<<20)
			if err == nil {
				t.Fatalf("expected error for x-amz-decoded-content-length %q, got nil", val)
			}
			var ae *awserror.Error
			if !errors.As(err, &ae) || ae.Code != "InvalidArgument" {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestChunkReaderRejectsExcessiveTrailers(t *testing.T) {
	t.Run("too many trailer headers", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString("0\r\n")
		for i := range 200 {
			fmt.Fprintf(&sb, "x-amz-trailer-%d: value%d\r\n", i, i)
		}
		sb.WriteString("\r\n")

		reader := NewChunkReader(bufio.NewReader(strings.NewReader(sb.String())), make(http.Header))
		var buf [128]byte
		_, err := reader.Read(buf[:])
		if err == nil {
			t.Fatal("expected error when trailer count exceeds limit")
		}
	})

	t.Run("excessive aggregate trailer size", func(t *testing.T) {
		var sb strings.Builder
		sb.WriteString("0\r\n")
		// Each line is ~1000 bytes, 70 lines = ~70 KiB > 64 KiB
		for i := range 70 {
			fmt.Fprintf(&sb, "x-amz-trailer-%02d: %s\r\n", i, strings.Repeat("a", 1000))
		}
		sb.WriteString("\r\n")

		reader := NewChunkReader(bufio.NewReader(strings.NewReader(sb.String())), make(http.Header))
		var buf [128]byte
		_, err := reader.Read(buf[:])
		if err == nil {
			t.Fatal("expected error when aggregate trailer size exceeds limit")
		}
	})
}

func TestStreamLimiterPreservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/bucket/key", strings.NewReader("hello world"))
	r.ContentLength = 11
	r.Header.Set("Content-Length", "11")

	stream, _, err := StreamBody(httptest.NewRecorder(), r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var buf [128]byte
	_, err = stream.Read(buf[:])
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestChunkReaderRejectsDataAfterTerminator(t *testing.T) {
	reader := NewChunkReader(bufio.NewReader(strings.NewReader("5\r\nhello\r\n0\r\n\r\nextra data")), make(http.Header))
	_, err := io.Copy(io.Discard, reader)
	if err == nil {
		t.Fatal("accepted bytes after the aws-chunked terminator")
	}
}

func TestStreamBodyRequiresDeclaredHTTPChecksumTrailer(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/bucket/key", strings.NewReader("hello"))
	// net/http removes the Trailer header and keeps its declarations here.
	r.Trailer = http.Header{"X-Amz-Checksum-Crc32": nil}
	stream, _, err := StreamBody(httptest.NewRecorder(), r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = io.Copy(io.Discard, stream)
	if ae, ok := errors.AsType[*awserror.Error](err); !ok || ae.Code != "InvalidArgument" {
		t.Fatalf("missing declared HTTP checksum trailer should fail, got %v", err)
	}
}
