package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestS3StreamingPutAndMultipartSDK verifies that AWS SDK PutObject and
// multipart uploads stream cleanly through the service and storage pipeline.
func TestS3StreamingPutAndMultipartSDK(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	bucket := new("streaming-sdk")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	const size = 6 << 20
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	sum := sha256.Sum256(data)
	b64Sum := base64.StdEncoding.EncodeToString(sum[:])

	// PutObject with SHA256 checksum
	putOut, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            bucket,
		Key:               new("put-stream.bin"),
		Body:              bytes.NewReader(data),
		ChecksumAlgorithm: s3types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if putOut.ChecksumSHA256 == nil || *putOut.ChecksumSHA256 != b64Sum {
		t.Fatalf("expected SHA256 %s, got %v", b64Sum, putOut.ChecksumSHA256)
	}

	getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket:       bucket,
		Key:          new("put-stream.bin"),
		ChecksumMode: s3types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer getOut.Body.Close()
	got, err := io.ReadAll(getOut.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("retrieved object content mismatch")
	}

	// Multipart UploadPart streaming
	createOut, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:            bucket,
		Key:               new("mp-stream.bin"),
		ChecksumAlgorithm: s3types.ChecksumAlgorithmSha256,
		ChecksumType:      s3types.ChecksumTypeComposite,
	})
	if err != nil {
		t.Fatal(err)
	}

	part1Out, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     bucket,
		Key:        new("mp-stream.bin"),
		UploadId:   createOut.UploadId,
		PartNumber: new(int32(1)),
		Body:       bytes.NewReader(data[:5<<20]),
	})
	if err != nil {
		t.Fatal(err)
	}

	part2Out, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     bucket,
		Key:        new("mp-stream.bin"),
		UploadId:   createOut.UploadId,
		PartNumber: new(int32(2)),
		Body:       bytes.NewReader(data[5<<20:]),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   bucket,
		Key:      new("mp-stream.bin"),
		UploadId: createOut.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{
				{PartNumber: new(int32(1)), ETag: part1Out.ETag},
				{PartNumber: new(int32(2)), ETag: part2Out.ETag},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	getMp, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: bucket,
		Key:    new("mp-stream.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer getMp.Body.Close()
	gotMp, err := io.ReadAll(getMp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotMp, data) {
		t.Fatal("retrieved multipart object content mismatch")
	}
}

// TestS3StreamingBoundedMemory verifies that streaming large payloads through
// PutObject does not allocate memory proportional to the upload payload size.
func TestS3StreamingBoundedMemory(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	bucket := new("memory-bound")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	const payloadSize = 25 << 20 // 25 MiB
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		s.URL+"/memory-bound/large.bin",
		io.LimitReader(&deterministicReader{}, payloadSize),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown length / chunked transfer: forces streaming
	req.ContentLength = -1

	// Force GC and measure base memory
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}

	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	// Incurred heap allocations during 25 MiB upload must be bounded and
	// far below the payload size (under 8 MiB total heap allocation).
	allocated := m2.TotalAlloc - m1.TotalAlloc
	t.Logf("25 MiB streamed upload allocated %d heap bytes", allocated)
	if allocated > 8<<20 {
		t.Fatalf("heap allocations (%d bytes) exceeded expected bounded buffer during %d-byte streaming upload",
			allocated, payloadSize)
	}

	// Verify the object is intact and matches the deterministic sequence
	getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: bucket,
		Key:    new("large.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer getOut.Body.Close()

	hasher := sha256.New()
	readBytes, err := io.Copy(hasher, getOut.Body)
	if err != nil {
		t.Fatal(err)
	}
	if readBytes != payloadSize {
		t.Fatalf("expected %d bytes, got %d", payloadSize, readBytes)
	}

	expectedHasher := sha256.New()
	_, _ = io.CopyN(expectedHasher, &deterministicReader{}, payloadSize)
	if !bytes.Equal(hasher.Sum(nil), expectedHasher.Sum(nil)) {
		t.Fatal("SHA256 of retrieved streaming object did not match expected")
	}
}

// TestS3StreamingVerifyMode verifies that SigV4 verify mode correctly checks
// signed SHA256 hashes without loading the entire body into memory, handles
// error and cancellation paths without leaking spool files, and respects streaming limitations.
func TestS3StreamingVerifyMode(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	s := verifyingServer(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	bucket := new("verify-mode")
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	assertNoSpools := func(sub string) {
		t.Helper()
		files, err := filepath.Glob(filepath.Join(os.TempDir(), "tinystack-upload-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("[%s] leaked spool files: %v", sub, files)
		}
	}

	t.Run("signed PUT with hexadecimal hash", func(t *testing.T) {
		body := bytes.Repeat([]byte("signed payload bytes for PUT verification "), 1000)
		sum := sha256.Sum256(body)
		hexHash := hex.EncodeToString(sum[:])

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPut,
			s.URL+"/verify-mode/signed-put.txt",
			bytes.NewReader(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Content-Sha256", hexHash)
		signedWithHeader(t, req, time.Now())

		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
		}

		getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
			Bucket: bucket,
			Key:    aws.String("signed-put.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer getOut.Body.Close()
		gotData, err := io.ReadAll(getOut.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotData, body) {
			t.Fatal("retrieved object content does not match original")
		}
		assertNoSpools("signed PUT")
	})

	t.Run("signed UploadPart with hexadecimal hash", func(t *testing.T) {
		mp, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: bucket,
			Key:    aws.String("mp-signed.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}

		partData := bytes.Repeat([]byte("multipart part 1 content"), 200000)
		partSum := sha256.Sum256(partData)
		partHex := hex.EncodeToString(partSum[:])

		partURL := fmt.Sprintf("%s/verify-mode/mp-signed.bin?partNumber=1&uploadId=%s", s.URL, *mp.UploadId)
		partReq, err := http.NewRequestWithContext(ctx, http.MethodPut, partURL, bytes.NewReader(partData))
		if err != nil {
			t.Fatal(err)
		}
		partReq.Header.Set("X-Amz-Content-Sha256", partHex)
		signedWithHeader(t, partReq, time.Now())

		partResp, err := s.Client().Do(partReq)
		if err != nil {
			t.Fatal(err)
		}
		defer partResp.Body.Close()
		if partResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(partResp.Body)
			t.Fatalf("expected 200 for UploadPart, got %d: %s", partResp.StatusCode, string(b))
		}
		etag := partResp.Header.Get("ETag")
		if etag == "" {
			t.Fatal("missing ETag in UploadPart response")
		}

		_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket:   bucket,
			Key:      aws.String("mp-signed.bin"),
			UploadId: mp.UploadId,
			MultipartUpload: &s3types.CompletedMultipartUpload{
				Parts: []s3types.CompletedPart{
					{
						ETag:       aws.String(etag),
						PartNumber: aws.Int32(1),
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("CompleteMultipartUpload failed: %v", err)
		}

		getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
			Bucket: bucket,
			Key:    aws.String("mp-signed.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer getOut.Body.Close()
		gotData, err := io.ReadAll(getOut.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotData, partData) {
			t.Fatal("multipart object content does not match original")
		}
		assertNoSpools("signed UploadPart")
	})

	t.Run("payload hash mismatch produces XAmzContentSHA256Mismatch", func(t *testing.T) {
		body := []byte("mismatched payload content")
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPut,
			s.URL+"/verify-mode/mismatch.txt",
			bytes.NewReader(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Content-Sha256", strings.Repeat("f", 64))
		signedWithHeader(t, req, time.Now())

		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "XAmzContentSHA256Mismatch") {
			t.Fatalf("expected XAmzContentSHA256Mismatch in response, got %s", string(b))
		}
		assertNoSpools("hash mismatch")
	})

	t.Run("bad signature rejected and cleans spool", func(t *testing.T) {
		body := []byte("payload with corrupted signature")
		sum := sha256.Sum256(body)
		hexHash := hex.EncodeToString(sum[:])

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPut,
			s.URL+"/verify-mode/bad-sig.txt",
			bytes.NewReader(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Content-Sha256", hexHash)
		signedWithHeader(t, req, time.Now())

		auth := req.Header.Get("Authorization")
		idx := strings.Index(auth, "Signature=")
		if idx < 0 {
			t.Fatal("missing signature in authorization header")
		}
		req.Header.Set("Authorization", auth[:idx]+"Signature="+strings.Repeat("0", 64))

		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden, got %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "SignatureDoesNotMatch") {
			t.Fatalf("expected SignatureDoesNotMatch, got %s", string(b))
		}
		assertNoSpools("bad signature")
	})

	t.Run("bounded heap allocation on large signed body", func(t *testing.T) {
		const payloadSize = 15 << 20 // 15 MiB
		hasher := sha256.New()
		_, _ = io.CopyN(hasher, &deterministicReader{}, payloadSize)
		largeHex := hex.EncodeToString(hasher.Sum(nil))

		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPut,
			s.URL+"/verify-mode/large-signed.bin",
			io.LimitReader(&deterministicReader{}, payloadSize),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = payloadSize
		req.Header.Set("X-Amz-Content-Sha256", largeHex)
		signedWithHeader(t, req, time.Now())

		runtime.GC()
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)

		resp, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(b))
		}

		var m2 runtime.MemStats
		runtime.ReadMemStats(&m2)

		allocated := m2.TotalAlloc - m1.TotalAlloc
		t.Logf("15 MiB signed upload allocated %d heap bytes", allocated)
		if allocated > 8<<20 {
			t.Fatalf("heap allocations (%d bytes) exceeded 8 MiB during %d-byte signed streaming upload",
				allocated, payloadSize)
		}

		getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
			Bucket: bucket,
			Key:    aws.String("large-signed.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer getOut.Body.Close()
		checkHasher := sha256.New()
		copied, err := io.Copy(checkHasher, getOut.Body)
		if err != nil {
			t.Fatal(err)
		}
		if copied != payloadSize {
			t.Fatalf("expected %d bytes, got %d", payloadSize, copied)
		}
		if hex.EncodeToString(checkHasher.Sum(nil)) != largeHex {
			t.Fatal("SHA256 of retrieved object did not match expected")
		}
		assertNoSpools("large signed body")
	})

	t.Run("presigned PUT in verify mode", func(t *testing.T) {
		presigner := s3.NewPresignClient(c)
		upload, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket: bucket,
			Key:    aws.String("presigned-obj.txt"),
		}, s3.WithPresignExpires(time.Minute))
		if err != nil {
			t.Fatal(err)
		}

		pReq, err := http.NewRequestWithContext(ctx, upload.Method, upload.URL, strings.NewReader("presigned content"))
		if err != nil {
			t.Fatal(err)
		}
		for k, vs := range upload.SignedHeader {
			pReq.Header[k] = vs
		}

		pResp, err := s.Client().Do(pReq)
		if err != nil {
			t.Fatal(err)
		}
		defer pResp.Body.Close()
		if pResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(pResp.Body)
			t.Fatalf("expected 200 for presigned PUT, got %d: %s", pResp.StatusCode, string(b))
		}
		assertNoSpools("presigned PUT")
	})

	t.Run("verify mode unsigned aws-chunked checksum trailers", func(t *testing.T) {
		// "hello world" -> CRC32 is 0x0d4a1185 -> base64 "DUoRhQ=="
		validChunks := "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:DUoRhQ==\r\n\r\n"
		cReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodPut,
			s.URL+"/verify-mode/chunked-trailers.txt",
			strings.NewReader(validChunks),
		)
		if err != nil {
			t.Fatal(err)
		}
		cReq.Header.Set("Content-Encoding", "aws-chunked")
		cReq.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
		cReq.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		signedWithHeader(t, cReq, time.Now())

		cResp, err := s.Client().Do(cReq)
		if err != nil {
			t.Fatal(err)
		}
		defer cResp.Body.Close()
		if cResp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(cResp.Body)
			t.Fatalf("expected 200, got %d: %s", cResp.StatusCode, string(b))
		}

		getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
			Bucket: bucket,
			Key:    aws.String("chunked-trailers.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer getOut.Body.Close()
		gotData, err := io.ReadAll(getOut.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotData) != "hello world" {
			t.Fatalf("expected 'hello world', got %q", string(gotData))
		}
		assertNoSpools("unsigned aws-chunked trailers")
	})

	t.Run("signed streaming rejected as NotImplemented", func(t *testing.T) {
		stReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodPut,
			s.URL+"/verify-mode/streaming-sig.bin",
			strings.NewReader("0\r\n\r\n"),
		)
		if err != nil {
			t.Fatal(err)
		}
		stReq.Header.Set("Content-Encoding", "aws-chunked")
		stReq.Header.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
		signedWithHeader(t, stReq, time.Now())

		stResp, err := s.Client().Do(stReq)
		if err != nil {
			t.Fatal(err)
		}
		defer stResp.Body.Close()
		if stResp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501 NotImplemented, got %d", stResp.StatusCode)
		}
		b, _ := io.ReadAll(stResp.Body)
		if !strings.Contains(string(b), "NotImplemented") {
			t.Fatalf("expected NotImplemented code in response, got %s", string(b))
		}
		assertNoSpools("signed streaming rejection")
	})
}
