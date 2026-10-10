package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

type spoolReadCounter struct {
	io.Reader
	reads  int
	closed bool
}

func (r *spoolReadCounter) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func (r *spoolReadCounter) Close() error {
	r.closed = true
	return nil
}

func TestSpoolAndHashStopsOnCancellation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := &spoolReadCounter{Reader: strings.NewReader("cancelled upload")}
	r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/bucket/key", body)
	c := &call{}
	defer c.close()
	_, err := new(Server).spoolAndHash(httptest.NewRecorder(), r, c, 1<<20, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled spool should return context.Canceled, got %v (body reads: %d)", err, body.reads)
	}
	if body.reads != 0 {
		t.Fatalf("read a cancelled body %d times", body.reads)
	}
	if !body.closed {
		t.Fatal("cancelled request body was not closed")
	}
}

type partialReader struct {
	err       error
	onRead    func()
	data      []byte
	offset    int
	failAfter int
	closed    bool
}

func (r *partialReader) Read(p []byte) (int, error) {
	if r.onRead != nil {
		r.onRead()
	}
	if r.offset >= r.failAfter && r.err != nil {
		return 0, r.err
	}
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *partialReader) Close() error {
	r.closed = true
	return nil
}

func TestSpoolAndHashCancellationAfterInput(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	ctx, cancel := context.WithCancel(context.Background())
	body := &partialReader{
		data:      []byte("hello some chunk of bytes to start spooling"),
		failAfter: 5,
		onRead: func() {
			cancel()
		},
	}
	r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/bucket/key", body)
	c := &call{}
	defer c.close()
	_, err := new(Server).spoolAndHash(httptest.NewRecorder(), r, c, 1<<20, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !body.closed {
		t.Fatal("body was not closed on cancellation after input")
	}
	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no temporary files after cancellation, found %d", len(entries))
	}
}

func TestSpoolAndHashCancellationAtEOF(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	body := &partialReader{onRead: cancel}
	r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/bucket/key", body)
	c := &call{}
	defer c.close()
	_, err := new(Server).spoolAndHash(httptest.NewRecorder(), r, c, 1<<20, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation at EOF should remain cancellation, got %v", err)
	}
}

func TestSpoolAndHashPartialReadFailure(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	readErrExpected := errors.New("connection reset by peer")
	body := &partialReader{
		data:      []byte("some initial data"),
		failAfter: 4,
		err:       readErrExpected,
	}
	r := httptest.NewRequest(http.MethodPut, "/bucket/key", body)
	c := &call{}
	defer c.close()
	_, err := new(Server).spoolAndHash(httptest.NewRecorder(), r, c, 1<<20, "")
	if err == nil || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("expected read error wrapped, got %v", err)
	}
	if !body.closed {
		t.Fatal("body was not closed on partial-read failure")
	}
	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected temporary spool file to be cleaned up on read error, found %d", len(entries))
	}
}

func TestSpoolAndHashShortHTTPBody(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	// Case 1: io.ErrUnexpectedEOF
	body1 := &partialReader{
		data:      []byte("partial"),
		failAfter: 3,
		err:       io.ErrUnexpectedEOF,
	}
	r1 := httptest.NewRequest(http.MethodPut, "/bucket/key", body1)
	r1.ContentLength = 100
	c1 := &call{}
	defer c1.close()
	_, err1 := new(Server).spoolAndHash(httptest.NewRecorder(), r1, c1, 1<<20, "")
	if e, ok := errors.AsType[*awserror.Error](err1); !ok || e.Code != "IncompleteBody" {
		t.Fatalf("expected IncompleteBody for ErrUnexpectedEOF, got %v", err1)
	}
	if !body1.closed {
		t.Fatal("body1 was not closed")
	}

	// Case 2: Content-Length mismatch at EOF
	body2 := &spoolReadCounter{Reader: strings.NewReader("short")}
	r2 := httptest.NewRequest(http.MethodPut, "/bucket/key", body2)
	r2.ContentLength = 50
	c2 := &call{}
	defer c2.close()
	_, err2 := new(Server).spoolAndHash(httptest.NewRecorder(), r2, c2, 1<<20, "")
	if e, ok := errors.AsType[*awserror.Error](err2); !ok || e.Code != "IncompleteBody" {
		t.Fatalf("expected IncompleteBody for Content-Length mismatch, got %v", err2)
	}
	if !body2.closed {
		t.Fatal("body2 was not closed")
	}

	entries, readErr := os.ReadDir(tempDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected spool files to be cleaned up, found %d", len(entries))
	}
}
