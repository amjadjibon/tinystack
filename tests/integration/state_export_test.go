package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdkdynamo "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	sdks3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	sdksqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/archive"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
)

func TestStateExport_FullSDKLifecycle(t *testing.T) {
	ctx := context.Background()
	storageDir := t.TempDir()

	// 1. Start persistent server.
	s, stop := persistent(t, storageDir, &fakeRuntime{})

	s3East := s3Client(s, "test", "us-east-1")
	dynamoEast := client(s, "test", "us-east-1")
	dynamoWest := client(s, "test", "us-west-2")
	sqsClient := sqsClient(s, "test")
	iamCli := iamClient(s, "test", "us-east-1")

	// 2. Global IAM scope: create IAM role.
	_, err := iamCli.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String("fixture-role"),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})
	if err != nil {
		t.Fatalf("create iam role: %v", err)
	}

	// 3. Multi-scope DynamoDB: us-east-1 and us-west-2 tables.
	_, err = create(ctx, dynamoEast, "east_items")
	if err != nil {
		t.Fatalf("create east dynamodb table: %v", err)
	}
	_, err = create(ctx, dynamoWest, "west_items")
	if err != nil {
		t.Fatalf("create west dynamodb table: %v", err)
	}

	_, err = dynamoEast.PutItem(ctx, &sdkdynamo.PutItemInput{
		TableName: aws.String("east_items"),
		Item: map[string]types.AttributeValue{
			"PK":   &types.AttributeValueMemberS{Value: "user-42"},
			"Name": &types.AttributeValueMemberS{Value: "Grace Hopper"},
		},
	})
	if err != nil {
		t.Fatalf("put east dynamodb item: %v", err)
	}

	// 4. S3 objects: standard, binary, empty, completed multipart, pending multipart, versioned.
	bucketName := "fixture-bucket"
	_, err = s3East.CreateBucket(ctx, &sdks3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	helloPayload := []byte("Hello TinyStack Fixture Data")
	_, err = s3East.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket:   aws.String(bucketName),
		Key:      aws.String("hello.txt"),
		Body:     bytes.NewReader(helloPayload),
		Metadata: map[string]string{"custom-meta": "value-123"},
	})
	if err != nil {
		t.Fatalf("put hello.txt: %v", err)
	}

	emptyPayload := []byte("")
	_, err = s3East.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("empty.bin"),
		Body:   bytes.NewReader(emptyPayload),
	})
	if err != nil {
		t.Fatalf("put empty.bin: %v", err)
	}

	binaryPayload := []byte{0x00, 0xFF, 0xFE, 0x42, 0xAA, 0x55, 0x12, 0x34}
	_, err = s3East.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("binary.bin"),
		Body:   bytes.NewReader(binaryPayload),
	})
	if err != nil {
		t.Fatalf("put binary.bin: %v", err)
	}

	// Completed multipart upload: S3 requires non-final parts to be >= 5 MiB.
	mpCreate, err := s3East.CreateMultipartUpload(ctx, &sdks3.CreateMultipartUploadInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("multipart.dat"),
	})
	if err != nil {
		t.Fatalf("create multipart upload: %v", err)
	}

	part1Payload := bytes.Repeat([]byte("A"), 5*1024*1024)
	part1, err := s3East.UploadPart(ctx, &sdks3.UploadPartInput{
		Bucket:     aws.String(bucketName),
		Key:        aws.String("multipart.dat"),
		UploadId:   mpCreate.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader(part1Payload),
	})
	if err != nil {
		t.Fatalf("upload part 1: %v", err)
	}

	part2Payload := []byte("multipart chunk two payload")
	part2, err := s3East.UploadPart(ctx, &sdks3.UploadPartInput{
		Bucket:     aws.String(bucketName),
		Key:        aws.String("multipart.dat"),
		UploadId:   mpCreate.UploadId,
		PartNumber: aws.Int32(2),
		Body:       bytes.NewReader(part2Payload),
	})
	if err != nil {
		t.Fatalf("upload part 2: %v", err)
	}

	_, err = s3East.CompleteMultipartUpload(ctx, &sdks3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucketName),
		Key:      aws.String("multipart.dat"),
		UploadId: mpCreate.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{
				{ETag: part1.ETag, PartNumber: aws.Int32(1)},
				{ETag: part2.ETag, PartNumber: aws.Int32(2)},
			},
		},
	})
	if err != nil {
		t.Fatalf("complete multipart upload: %v", err)
	}

	// Pending multipart upload (uncompleted part retained in storage).
	pendingCreate, err := s3East.CreateMultipartUpload(ctx, &sdks3.CreateMultipartUploadInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("pending-multipart.dat"),
	})
	if err != nil {
		t.Fatalf("create pending multipart upload: %v", err)
	}

	pendingPartPayload := []byte("pending multipart part chunk body")
	_, err = s3East.UploadPart(ctx, &sdks3.UploadPartInput{
		Bucket:     aws.String(bucketName),
		Key:        aws.String("pending-multipart.dat"),
		UploadId:   pendingCreate.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader(pendingPartPayload),
	})
	if err != nil {
		t.Fatalf("upload pending part: %v", err)
	}

	// Versioning and non-default versions.
	_, err = s3East.PutBucketVersioning(ctx, &sdks3.PutBucketVersioningInput{
		Bucket: aws.String(bucketName),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	})
	if err != nil {
		t.Fatalf("put bucket versioning: %v", err)
	}

	v1Payload := []byte("first version content")
	putV1, err := s3East.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("versioned.txt"),
		Body:   bytes.NewReader(v1Payload),
	})
	if err != nil {
		t.Fatalf("put version 1: %v", err)
	}

	v2Payload := []byte("second updated version content")
	putV2, err := s3East.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("versioned.txt"),
		Body:   bytes.NewReader(v2Payload),
	})
	if err != nil {
		t.Fatalf("put version 2: %v", err)
	}

	// 5. Populate SQS.
	qURL := createQueue(t, sqsClient, "fixture_queue", nil)
	_, err = sqsClient.SendMessage(ctx, &sdksqs.SendMessageInput{
		QueueUrl:    &qURL,
		MessageBody: aws.String("persisted-message-in-queue"),
	})
	if err != nil {
		t.Fatalf("send sqs message: %v", err)
	}

	// 6. Stop the App and server. This closes sqlite cleanly.
	stop()

	// 7. Inject a legacy inline object body directly into SQLite to verify legacy inline support.
	legacyPayload := []byte("raw legacy unescaped inline object body")
	store, err := sqlite.Open(ctx, storageDir)
	if err != nil {
		t.Fatalf("open sqlite to insert legacy body: %v", err)
	}

	err = store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put("000000000000/us-east-1/s3/fixture-bucket/d/legacy.txt", legacyPayload)
		return nil
	})
	_ = store.Close()
	if err != nil {
		t.Fatalf("insert legacy inline body: %v", err)
	}

	// 8. Perform offline export.
	exportOut := filepath.Join(t.TempDir(), "exported-fixture.tar.gz")
	err = archive.Export(ctx, archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: exportOut,
	})
	if err != nil {
		t.Fatalf("offline export failed: %v", err)
	}

	// 9. Inspect the exported archive and verify contents.
	manifest, records, err := archive.InspectArchive(ctx, exportOut)
	if err != nil {
		t.Fatalf("inspect archive: %v", err)
	}

	if manifest.FormatVersion != 1 {
		t.Fatalf("expected format version 1, got %d", manifest.FormatVersion)
	}

	if manifest.Records.Count <= 0 {
		t.Fatalf("expected non-empty records count, got %d", manifest.Records.Count)
	}

	if manifest.Totals.BlobCount <= 0 {
		t.Fatalf("expected non-empty blob count, got %d", manifest.Totals.BlobCount)
	}

	// Find the records and verify their contents and blobs.
	recordMap := make(map[string][]byte)
	for _, r := range records {
		recordMap[r.Key] = r.Value
	}

	manifestBlobMap := make(map[string]archive.BlobMeta)
	for _, bm := range manifest.Blobs {
		manifestBlobMap[bm.ID] = bm
	}

	// Verify Multi-Scope records: global IAM, us-east-1, us-west-2.
	var foundGlobalRole, foundEastTable, foundWestTable bool
	for k := range recordMap {
		if strings.Contains(k, "fixture-role") {
			foundGlobalRole = true
		}
		if strings.Contains(k, "east_items") {
			foundEastTable = true
		}
		if strings.Contains(k, "west_items") {
			foundWestTable = true
		}
	}
	if !foundGlobalRole {
		t.Fatalf("global IAM role record not found in exported records")
	}
	if !foundEastTable {
		t.Fatalf("us-east-1 DynamoDB table record not found in exported records")
	}
	if !foundWestTable {
		t.Fatalf("us-west-2 DynamoDB table record not found in exported records")
	}

	// Verify SQS record was exported.
	var foundSQS bool
	for k := range recordMap {
		if strings.Contains(k, "fixture_queue") {
			foundSQS = true
			break
		}
	}
	if !foundSQS {
		t.Fatalf("sqs queue record not found in exported records")
	}

	// Verify legacy inline record was exported with exact byte payload.
	legacyVal, hasLegacy := recordMap["000000000000/us-east-1/s3/fixture-bucket/d/legacy.txt"]
	if !hasLegacy || !bytes.Equal(legacyVal, legacyPayload) {
		t.Fatalf("legacy inline body mismatch: got %q, want %q", legacyVal, legacyPayload)
	}

	// Verify standard blob reference and file content.
	var helloBlob storage.Blob
	var foundHelloBlobRef bool
	for k, v := range recordMap {
		if strings.HasSuffix(k, "hello.txt") {
			if blob, isBlob := storage.BlobReference(v); isBlob {
				helloBlob = blob
				foundHelloBlobRef = true
				break
			}
		}
	}
	if !foundHelloBlobRef {
		t.Fatalf("hello.txt blob reference not found in records")
	}
	if helloBlob.Size != int64(len(helloPayload)) {
		t.Fatalf("hello.txt blob size mismatch: got %d, want %d", helloBlob.Size, len(helloPayload))
	}

	expectedHelloHash := sha256.Sum256(helloPayload)
	expectedHelloHashHex := hex.EncodeToString(expectedHelloHash[:])
	bm, ok := manifestBlobMap[helloBlob.ID]
	if !ok {
		t.Fatalf("hello blob %s missing from manifest", helloBlob.ID)
	}
	if bm.SHA256 != expectedHelloHashHex {
		t.Fatalf("hello blob hash mismatch: got %s, want %s", bm.SHA256, expectedHelloHashHex)
	}

	// Verify empty payload hash (size 0).
	// Standard empty sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	var foundEmptyBlob bool
	for k, v := range recordMap {
		if strings.HasSuffix(k, "empty.bin") {
			if blob, isBlob := storage.BlobReference(v); isBlob {
				foundEmptyBlob = true
				if blob.Size != 0 {
					t.Fatalf("empty blob size mismatch: got %d, want 0", blob.Size)
				}
				bmEmpty, exists := manifestBlobMap[blob.ID]
				if !exists {
					t.Fatalf("empty blob %s missing from manifest", blob.ID)
				}
				if bmEmpty.SHA256 != emptySHA256 {
					t.Fatalf("empty blob hash mismatch: got %s, want %s", bmEmpty.SHA256, emptySHA256)
				}
				break
			}
		}
	}
	if !foundEmptyBlob {
		t.Fatalf("empty.bin blob reference not found in records")
	}

	// Verify binary payload hash with non-ASCII and NUL bytes.
	expectedBinaryHash := sha256.Sum256(binaryPayload)
	expectedBinaryHashHex := hex.EncodeToString(expectedBinaryHash[:])
	var foundBinaryBlob bool
	for k, v := range recordMap {
		if strings.HasSuffix(k, "binary.bin") {
			if blob, isBlob := storage.BlobReference(v); isBlob {
				foundBinaryBlob = true
				if blob.Size != int64(len(binaryPayload)) {
					t.Fatalf("binary blob size mismatch: got %d, want %d", blob.Size, len(binaryPayload))
				}
				bmBinary, exists := manifestBlobMap[blob.ID]
				if !exists {
					t.Fatalf("binary blob %s missing from manifest", blob.ID)
				}
				if bmBinary.SHA256 != expectedBinaryHashHex {
					t.Fatalf("binary blob hash mismatch: got %s, want %s", bmBinary.SHA256, expectedBinaryHashHex)
				}
				break
			}
		}
	}
	if !foundBinaryBlob {
		t.Fatalf("binary.bin blob reference not found in records")
	}

	// Verify pending multipart upload part record and blob in manifest.
	expectedPendingHash := sha256.Sum256(pendingPartPayload)
	expectedPendingHashHex := hex.EncodeToString(expectedPendingHash[:])
	var foundPendingPartBlob bool
	for k, v := range recordMap {
		if strings.Contains(k, *pendingCreate.UploadId) && strings.Contains(k, "/pb/") {
			if blob, isBlob := storage.BlobReference(v); isBlob {
				foundPendingPartBlob = true
				bmPending, exists := manifestBlobMap[blob.ID]
				if !exists {
					t.Fatalf("pending part blob %s missing from manifest", blob.ID)
				}
				if bmPending.Size != int64(len(pendingPartPayload)) {
					t.Fatalf(
						"pending part blob size mismatch: got %d, want %d",
						bmPending.Size,
						len(pendingPartPayload),
					)
				}
				if bmPending.SHA256 != expectedPendingHashHex {
					t.Fatalf("pending part hash mismatch: got %s, want %s", bmPending.SHA256, expectedPendingHashHex)
				}
				break
			}
		}
	}
	if !foundPendingPartBlob {
		t.Fatalf("pending multipart part blob reference not found in records")
	}

	// Verify versioned object non-default versions.
	if putV1.VersionId == nil || putV2.VersionId == nil {
		t.Fatalf("expected non-nil version IDs from S3 PutObject")
	}
	var foundV1Blob, foundV2Blob bool
	expectedV1Hash := sha256.Sum256(v1Payload)
	expectedV1HashHex := hex.EncodeToString(expectedV1Hash[:])
	expectedV2Hash := sha256.Sum256(v2Payload)
	expectedV2HashHex := hex.EncodeToString(expectedV2Hash[:])

	// V1 is the noncurrent version; its blob reference is keyed by version ID in ".../bb/<v1Id>/...".
	// V2 is the current version; its blob reference is keyed in ".../db/<key>".
	for k, v := range recordMap {
		if strings.Contains(k, *putV1.VersionId) {
			if blob, isBlob := storage.BlobReference(v); isBlob {
				foundV1Blob = true
				bmV1, exists := manifestBlobMap[blob.ID]
				if !exists || bmV1.SHA256 != expectedV1HashHex {
					t.Fatalf("version 1 blob missing or hash mismatch: %+v", bmV1)
				}
			}
		}
		if strings.HasSuffix(k, "/db/versioned.txt") {
			if blob, isBlob := storage.BlobReference(v); isBlob {
				foundV2Blob = true
				bmV2, exists := manifestBlobMap[blob.ID]
				if !exists || bmV2.SHA256 != expectedV2HashHex {
					t.Fatalf("version 2 blob missing or hash mismatch: %+v", bmV2)
				}
			}
		}
	}
	if !foundV1Blob {
		t.Fatalf("version 1 noncurrent blob (%s) not found in exported records", *putV1.VersionId)
	}
	if !foundV2Blob {
		t.Fatalf("version 2 current blob (%s) not found in exported records", *putV2.VersionId)
	}

	// Verify non-default metadata is preserved in the S3 object metadata record.
	var foundMeta bool
	for k, v := range recordMap {
		if strings.Contains(k, "hello.txt") && strings.Contains(string(v), "value-123") {
			foundMeta = true
			break
		}
	}
	if !foundMeta {
		t.Fatalf("custom S3 metadata missing from object record")
	}

	// 10. Now test uncompressed tar export as well.
	tarOut := filepath.Join(t.TempDir(), "exported-fixture.tar")
	err = archive.Export(ctx, archive.ExportOptions{
		StorageDir: storageDir,
		OutputFile: tarOut,
	})
	if err != nil {
		t.Fatalf("uncompressed tar export failed: %v", err)
	}

	manifestTar, _, err := archive.InspectArchive(ctx, tarOut)
	if err != nil {
		t.Fatalf("inspect uncompressed tar archive: %v", err)
	}

	if manifestTar.Records.SHA256 != manifest.Records.SHA256 {
		t.Fatalf("tar vs tar.gz records hash mismatch: %s vs %s",
			manifestTar.Records.SHA256, manifest.Records.SHA256)
	}
}
