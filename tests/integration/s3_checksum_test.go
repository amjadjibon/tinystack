package integration_test

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- S3 defines SHA-1 object checksums.
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const checksumOn = s3types.ChecksumModeEnabled

const (
	algCrc32  = s3types.ChecksumAlgorithmCrc32
	algCrc32c = s3types.ChecksumAlgorithmCrc32c
	algSha1   = s3types.ChecksumAlgorithmSha1
	algSha256 = s3types.ChecksumAlgorithmSha256

	typComposite  = s3types.ChecksumTypeComposite
	typFullObject = s3types.ChecksumTypeFullObject
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func digest(h hash.Hash, data []byte) []byte {
	_, _ = h.Write(data)

	return h.Sum(nil)
}

func wantCRC32(data []byte) string { return b64(digest(crc32.NewIEEE(), data)) }

func wantCRC32C(data []byte) string {
	return b64(digest(crc32.New(crc32.MakeTable(crc32.Castagnoli)), data))
}

func wantCRC64NVME(data []byte) string {
	return b64(digest(crc64.New(crc64.MakeTable(0x9a6c9329ac4bc9b5)), data))
}

func wantSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return b64(sum[:])
}

func wantSHA1(data []byte) string {
	sum := sha1.Sum(data) // #nosec G401 -- S3 protocol checksum.
	return b64(sum[:])
}

// noAutoChecksums makes a client that sends checksums and asks for them only
// when an operation requires it.
func noAutoChecksums(o *s3.Options) {
	o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
}

func checksumBucket(t *testing.T, s *httptest.Server, opts ...func(*s3.Options)) (client *s3.Client, bucket *string) {
	t.Helper()

	client = s3Client(s, "test", "us-east-1", opts...)
	bucket = aws.String("sums")
	if _, err := client.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}

	return client, bucket
}

func TestS3StoredChecksums(t *testing.T) {
	s := server(t)
	c, bucket := checksumBucket(t, s, noAutoChecksums)
	ctx := context.Background()
	data := []byte("hello checksums")

	// An explicit CRC32 value is stored and echoed.
	put, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("crc"), Body: bytes.NewReader(data), ChecksumCRC32: aws.String(wantCRC32(data)),
	})
	if err != nil || aws.ToString(put.ChecksumCRC32) != wantCRC32(data) ||
		put.ChecksumType != typFullObject {
		t.Fatal(put, err)
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: aws.String("crc"), ChecksumMode: checksumOn,
	})
	if err != nil || aws.ToString(head.ChecksumCRC32) != wantCRC32(data) || head.ChecksumCRC64NVME != nil ||
		head.ChecksumType != typFullObject {
		t.Fatalf("%+v %v", head, err)
	}

	// Without ChecksumMode nothing is reported.
	head, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("crc")})
	if err != nil || head.ChecksumCRC32 != nil || head.ChecksumType != "" {
		t.Fatalf("%+v %v", head, err)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("crc")})
	if err != nil || got.ChecksumCRC32 != nil {
		t.Fatalf("%+v %v", got, err)
	}

	_ = got.Body.Close()

	// The algorithm alone makes the service compute the value.
	put, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("sha"), Body: bytes.NewReader(data), ChecksumAlgorithm: algSha256,
	})
	if err != nil || aws.ToString(put.ChecksumSHA256) != wantSHA256(data) {
		t.Fatal(put, err)
	}

	got, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: bucket, Key: aws.String("sha"), ChecksumMode: checksumOn,
	})
	if err != nil || aws.ToString(got.ChecksumSHA256) != wantSHA256(data) || got.ChecksumCRC32 != nil {
		t.Fatalf("%+v %v", got, err)
	}

	// The SDK validates the body against the reported checksum.
	if body, readErr := io.ReadAll(got.Body); readErr != nil || !bytes.Equal(body, data) {
		t.Fatal(string(body), readErr)
	}

	_ = got.Body.Close()

	// With neither, S3 stores CRC64NVME.
	put, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("plain"), Body: bytes.NewReader(data),
	})
	if err != nil || aws.ToString(put.ChecksumCRC64NVME) != wantCRC64NVME(data) {
		t.Fatal(put, err)
	}

	// A wrong value is rejected.
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("bad"), Body: bytes.NewReader(data),
		ChecksumCRC32: aws.String(wantCRC32([]byte("x"))),
	})
	if code, status := s3Code(err); code != "BadDigest" || status != 400 {
		t.Fatal(code, status, err)
	}

	// A ranged GET reports no checksum.
	got, err = c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: bucket, Key: aws.String("crc"), Range: aws.String("bytes=0-4"), ChecksumMode: checksumOn,
	})
	if err != nil || got.ChecksumCRC32 != nil || got.ChecksumCRC64NVME != nil || got.ChecksumType != "" {
		t.Fatalf("%+v %v", got, err)
	}

	if body, _ := io.ReadAll(got.Body); string(body) != "hello" {
		t.Fatal(string(body))
	}

	_ = got.Body.Close()
}

// TestS3DefaultSDKChecksums runs the SDK's own default flow: it sends a checksum,
// asks for the checksum back, and validates the body against it. The SDK's
// default algorithm is CRC64NVME, which S3 also stores when none is sent.
func TestS3DefaultSDKChecksums(t *testing.T) {
	s := server(t)
	c, bucket := checksumBucket(t, s)
	ctx := context.Background()
	data := []byte("sdk default flow")

	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("k"), Body: bytes.NewReader(data),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(got.Body)
	_ = got.Body.Close()
	if err != nil || !bytes.Equal(body, data) {
		t.Fatal(string(body), err)
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: aws.String("k"), ChecksumMode: checksumOn,
	})
	if err != nil || aws.ToString(head.ChecksumCRC64NVME) != wantCRC64NVME(data) {
		t.Fatalf("%+v %v", head, err)
	}
}

func TestS3MultipartChecksums(t *testing.T) {
	s := server(t)
	c, bucket := checksumBucket(t, s, noAutoChecksums)
	ctx := context.Background()
	first := bytes.Repeat([]byte("a"), 5<<20)
	second := []byte("tail")
	whole := append(append([]byte{}, first...), second...)

	// COMPOSITE CRC32: the checksum of the parts' raw digests, suffixed with the part count.
	key := aws.String("composite")
	created, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: key, ChecksumAlgorithm: algCrc32,
	})
	if err != nil || created.ChecksumAlgorithm != algCrc32 ||
		created.ChecksumType != typComposite {
		t.Fatalf("%+v %v", created, err)
	}

	var parts []s3types.CompletedPart
	var digests []byte
	for i, data := range [][]byte{first, second} {
		up, uploadErr := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: key, UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(data), ChecksumAlgorithm: algCrc32,
		})
		if uploadErr != nil || aws.ToString(up.ChecksumCRC32) != wantCRC32(data) {
			t.Fatalf("%+v %v", up, uploadErr)
		}

		raw, _ := base64.StdEncoding.DecodeString(wantCRC32(data))
		digests = append(digests, raw...)
		parts = append(parts, s3types.CompletedPart{
			PartNumber: aws.Int32(int32(i + 1)), ETag: up.ETag, ChecksumCRC32: up.ChecksumCRC32,
		})
	}

	listed, err := c.ListParts(ctx, &s3.ListPartsInput{Bucket: bucket, Key: key, UploadId: created.UploadId})
	if err != nil || len(listed.Parts) != 2 || listed.ChecksumAlgorithm != algCrc32 ||
		aws.ToString(listed.Parts[0].ChecksumCRC32) != wantCRC32(first) ||
		aws.ToString(listed.Parts[1].ChecksumCRC32) != wantCRC32(second) {
		t.Fatalf("%+v %v", listed, err)
	}

	// A part checksum that does not match the uploaded part fails the completion.
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: parts[0].ETag, ChecksumCRC32: aws.String(wantCRC32(second))}, parts[1],
		}},
	})
	if code, _ := s3Code(err); code != "InvalidPart" {
		t.Fatal(code, err)
	}

	done, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: key, UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	composite := wantCRC32(digests) + "-2"
	if err != nil || aws.ToString(done.ChecksumCRC32) != composite || done.ChecksumType != typComposite {
		t.Fatalf("%+v %v", done, err)
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key, ChecksumMode: checksumOn})
	if err != nil || aws.ToString(head.ChecksumCRC32) != composite || head.ChecksumType != typComposite {
		t.Fatalf("%+v %v", head, err)
	}

	// The SDK skips validating a composite value but must not fail.
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key, ChecksumMode: checksumOn})
	if err != nil {
		t.Fatal(err)
	}

	if body, readErr := io.ReadAll(got.Body); readErr != nil || !bytes.Equal(body, whole) {
		t.Fatal(len(body), readErr)
	}

	_ = got.Body.Close()

	// FULL_OBJECT CRC32C covers all the bytes.
	full := aws.String("full")
	created, err = c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: full, ChecksumAlgorithm: algCrc32c,
		ChecksumType: typFullObject,
	})
	if err != nil {
		t.Fatal(err)
	}

	parts = parts[:0]
	for i, data := range [][]byte{first, second} {
		up, uploadErr := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: bucket, Key: full, UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(data), ChecksumAlgorithm: algCrc32c,
		})
		if uploadErr != nil {
			t.Fatal(uploadErr)
		}

		parts = append(parts, s3types.CompletedPart{
			PartNumber: aws.Int32(int32(i + 1)), ETag: up.ETag, ChecksumCRC32C: up.ChecksumCRC32C,
		})
	}

	if _, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: full, UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		t.Fatal(err)
	}

	head, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: full, ChecksumMode: checksumOn})
	if err != nil || aws.ToString(head.ChecksumCRC32C) != wantCRC32C(whole) ||
		head.ChecksumType != typFullObject {
		t.Fatalf("%+v %v", head, err)
	}

	// An upload with no algorithm gets a CRC64NVME full-object checksum.
	plain := aws.String("plain")
	created, err = c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: plain})
	if err != nil {
		t.Fatal(err)
	}

	up, err := c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: bucket, Key: plain, UploadId: created.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(second),
	})
	if err != nil || aws.ToString(up.ChecksumCRC64NVME) != wantCRC64NVME(second) {
		t.Fatalf("%+v %v", up, err)
	}

	if _, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: bucket, Key: plain, UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: up.ETag}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	head, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: plain, ChecksumMode: checksumOn})
	if err != nil || aws.ToString(head.ChecksumCRC64NVME) != wantCRC64NVME(second) {
		t.Fatalf("%+v %v", head, err)
	}
}

func TestS3CopyAndVersionChecksums(t *testing.T) {
	s := server(t)
	c, bucket := checksumBucket(t, s, noAutoChecksums)
	ctx := context.Background()
	data := []byte("copy me")

	if _, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("src"), Body: bytes.NewReader(data), ChecksumAlgorithm: algCrc32c,
	}); err != nil {
		t.Fatal(err)
	}

	// A copy carries the checksum over.
	copied, err := c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("dst"), CopySource: aws.String("sums/src"),
	})
	if err != nil || aws.ToString(copied.CopyObjectResult.ChecksumCRC32C) != wantCRC32C(data) {
		t.Fatalf("%+v %v", copied, err)
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: aws.String("dst"), ChecksumMode: checksumOn,
	})
	if err != nil || aws.ToString(head.ChecksumCRC32C) != wantCRC32C(data) {
		t.Fatalf("%+v %v", head, err)
	}

	// Naming an algorithm recomputes it.
	copied, err = c.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("dst1"), CopySource: aws.String("sums/src"),
		ChecksumAlgorithm: algSha1,
	})
	if err != nil || aws.ToString(copied.CopyObjectResult.ChecksumSHA1) != wantSHA1(data) ||
		copied.CopyObjectResult.ChecksumCRC32C != nil {
		t.Fatalf("%+v %v", copied, err)
	}

	head, err = c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: aws.String("dst1"), ChecksumMode: checksumOn,
	})
	if err != nil || aws.ToString(head.ChecksumSHA1) != wantSHA1(data) || head.ChecksumCRC32C != nil {
		t.Fatalf("%+v %v", head, err)
	}

	// Each version keeps its own checksum.
	if _, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  bucket,
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}

	key := aws.String("versioned")
	v1, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: strings.NewReader("one"), ChecksumAlgorithm: algSha256,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: key, Body: strings.NewReader("two"), ChecksumAlgorithm: algCrc32,
	}); err != nil {
		t.Fatal(err)
	}

	old, err := c.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: bucket, Key: key, VersionId: v1.VersionId, ChecksumMode: checksumOn,
	})
	if err != nil || aws.ToString(old.ChecksumSHA256) != wantSHA256([]byte("one")) || old.ChecksumCRC32 != nil {
		t.Fatalf("%+v %v", old, err)
	}

	current, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key, ChecksumMode: checksumOn})
	if err != nil || aws.ToString(current.ChecksumCRC32) != wantCRC32([]byte("two")) || current.ChecksumSHA256 != nil {
		t.Fatalf("%+v %v", current, err)
	}
}
