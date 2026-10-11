package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type failWriter struct {
	err       error
	written   int
	failAfter int
}

func (fw *failWriter) Write(p []byte) (int, error) {
	if fw.written >= fw.failAfter {
		return 0, fw.err
	}
	remaining := fw.failAfter - fw.written
	if len(p) > remaining {
		fw.written += remaining
		return remaining, fw.err
	}
	fw.written += len(p)
	return len(p), nil
}

type shortWriter struct {
	shortAfter int
	written    int
}

func (sw *shortWriter) Write(p []byte) (int, error) {
	if sw.written >= sw.shortAfter && len(p) > 1 {
		// Return fewer bytes than requested with a nil error to trigger io.ErrShortWrite
		sw.written += 1
		return 1, nil
	}
	sw.written += len(p)
	return len(p), nil
}

type failReader struct {
	err       error
	data      []byte
	offset    int
	failAfter int
}

func (fr *failReader) Read(p []byte) (int, error) {
	if fr.offset >= fr.failAfter {
		return 0, fr.err
	}
	remaining := fr.failAfter - fr.offset
	toRead := len(p)
	if toRead > remaining {
		toRead = remaining
	}
	copy(p, fr.data[fr.offset:fr.offset+toRead])
	fr.offset += toRead
	return toRead, nil
}

func TestCopyBufferContext_Success(t *testing.T) {
	data := []byte("hello context copy buffer streaming world")
	src := bytes.NewReader(data)
	var dst bytes.Buffer

	buf := make([]byte, 8)
	n, err := copyBufferContext(context.Background(), &dst, src, buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("expected %d bytes copied, got %d", len(data), n)
	}
	if !bytes.Equal(dst.Bytes(), data) {
		t.Fatalf("data mismatch: expected %q, got %q", string(data), dst.String())
	}
}

func TestCopyBufferContext_PreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	data := []byte("some payload data")
	src := bytes.NewReader(data)
	var dst bytes.Buffer

	n, err := copyBufferContext(ctx, &dst, src, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes copied on pre-cancelled context, got %d", n)
	}
}

func TestCopyBufferContext_CancellationDuringCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	data := bytes.Repeat([]byte("A"), 64)
	src := bytes.NewReader(data)

	var dst bytes.Buffer
	buf := make([]byte, 16)

	// Custom writer that triggers cancel after receiving the first chunk
	var chunks int
	tw := &testChunkWriter{
		onWrite: func(p []byte) {
			chunks++
			if chunks == 1 {
				cancel()
			}
		},
		dst: &dst,
	}

	n, err := copyBufferContext(ctx, tw, src, buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if n != 16 {
		t.Fatalf("expected 16 bytes copied before cancellation, got %d", n)
	}
}

type testChunkWriter struct {
	onWrite func(p []byte)
	dst     io.Writer
}

func (cw *testChunkWriter) Write(p []byte) (int, error) {
	cw.onWrite(p)
	return cw.dst.Write(p)
}

func TestCopyBufferContext_WriteError(t *testing.T) {
	data := bytes.Repeat([]byte("Z"), 32)
	src := bytes.NewReader(data)

	simulatedErr := errors.New("simulated destination disk write error")
	fw := &failWriter{failAfter: 8, err: simulatedErr}
	buf := make([]byte, 16)

	n, err := copyBufferContext(context.Background(), fw, src, buf)
	if !errors.Is(err, simulatedErr) {
		t.Fatalf("expected %v, got %v", simulatedErr, err)
	}
	if n != 8 {
		t.Fatalf("expected 8 bytes copied, got %d", n)
	}
}

func TestCopyBufferContext_ShortWrite(t *testing.T) {
	data := bytes.Repeat([]byte("W"), 32)
	src := bytes.NewReader(data)

	sw := &shortWriter{shortAfter: 0}
	buf := make([]byte, 16)

	_, err := copyBufferContext(context.Background(), sw, src, buf)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
}

func TestCopyBufferContext_ReadError(t *testing.T) {
	data := bytes.Repeat([]byte("R"), 32)
	readErr := errors.New("simulated source read error")
	fr := &failReader{data: data, failAfter: 8, err: readErr}

	var dst bytes.Buffer
	buf := make([]byte, 16)

	n, err := copyBufferContext(context.Background(), &dst, fr, buf)
	if err == nil || !errors.Is(err, readErr) {
		t.Fatalf("expected %v, got %v", readErr, err)
	}
	if n != 8 {
		t.Fatalf("expected 8 bytes copied before read error, got %d", n)
	}
}
