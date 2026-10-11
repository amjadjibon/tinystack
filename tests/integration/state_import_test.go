package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestStateImport_FullSDKRoundTrip(t *testing.T) {
	ctx := context.Background()
	sourceStorageDir := t.TempDir()

	readBody := func(t *testing.T, r io.ReadCloser) []byte {
		t.Helper()
		defer func() {
			if closeErr := r.Close(); closeErr != nil {
				t.Fatalf("close body: %v", closeErr)
			}
		}()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return data
	}

	// 1. Boot initial TinyStack instance on sourceStorageDir.
	s1, stop1 := persistent(t, sourceStorageDir, &fakeRuntime{})

	s3East1 := s3Client(s1, "test", "us-east-1")
	dynamoEast1 := client(s1, "test", "us-east-1")
	dynamoWest1 := client(s1, "test", "us-west-2")
	sqsClient1 := sqsClient(s1, "test")
	iamCli1 := iamClient(s1, "test", "us-east-1")

	// Global IAM Role.
	_, err := iamCli1.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String("import-test-role"),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})
	if err != nil {
		t.Fatalf("create iam role: %v", err)
	}

	// Multi-scope DynamoDB tables.
	_, err = create(ctx, dynamoEast1, "east_items")
	if err != nil {
		t.Fatalf("create east dynamodb table: %v", err)
	}
	_, err = create(ctx, dynamoWest1, "west_items")
	if err != nil {
		t.Fatalf("create west dynamodb table: %v", err)
	}

	_, err = dynamoEast1.PutItem(ctx, &sdkdynamo.PutItemInput{
		TableName: aws.String("east_items"),
		Item: map[string]types.AttributeValue{
			"PK":   &types.AttributeValueMemberS{Value: "user-east"},
			"Data": &types.AttributeValueMemberS{Value: "East Coast Record"},
		},
	})
	if err != nil {
		t.Fatalf("put east item: %v", err)
	}

	_, err = dynamoWest1.PutItem(ctx, &sdkdynamo.PutItemInput{
		TableName: aws.String("west_items"),
		Item: map[string]types.AttributeValue{
			"PK":   &types.AttributeValueMemberS{Value: "user-west"},
			"Data": &types.AttributeValueMemberS{Value: "West Coast Record"},
		},
	})
	if err != nil {
		t.Fatalf("put west item: %v", err)
	}

	// SQS queue and message.
	qURL1 := createQueue(t, sqsClient1, "import_test_queue", nil)
	sqsMsgBody := "persisted-message-roundtrip"
	_, err = sqsClient1.SendMessage(ctx, &sdksqs.SendMessageInput{
		QueueUrl:    &qURL1,
		MessageBody: aws.String(sqsMsgBody),
	})
	if err != nil {
		t.Fatalf("send sqs message: %v", err)
	}

	// S3 bucket and diverse objects:
	bucketName := "import-test-bucket"
	_, err = s3East1.CreateBucket(ctx, &sdks3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	helloPayload := []byte("Standard S3 payload content")
	_, err = s3East1.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket:   aws.String(bucketName),
		Key:      aws.String("hello.txt"),
		Body:     bytes.NewReader(helloPayload),
		Metadata: map[string]string{"custom-meta": "value-123"},
	})
	if err != nil {
		t.Fatalf("put hello.txt: %v", err)
	}

	emptyPayload := []byte("")
	_, err = s3East1.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("empty.bin"),
		Body:   bytes.NewReader(emptyPayload),
	})
	if err != nil {
		t.Fatalf("put empty.bin: %v", err)
	}

	binaryPayload := []byte{0x00, 0xFF, 0xFE, 0x42, 0xAA, 0x55, 0x00, 0x99}
	_, err = s3East1.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("binary.bin"),
		Body:   bytes.NewReader(binaryPayload),
	})
	if err != nil {
		t.Fatalf("put binary.bin: %v", err)
	}

	// Multipart completed:
	mpCreate, err := s3East1.CreateMultipartUpload(ctx, &sdks3.CreateMultipartUploadInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("multipart.dat"),
	})
	if err != nil {
		t.Fatalf("create multipart upload: %v", err)
	}

	part1Payload := bytes.Repeat([]byte("M"), 5*1024*1024)
	part1, err := s3East1.UploadPart(ctx, &sdks3.UploadPartInput{
		Bucket:     aws.String(bucketName),
		Key:        aws.String("multipart.dat"),
		UploadId:   mpCreate.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader(part1Payload),
	})
	if err != nil {
		t.Fatalf("upload part 1: %v", err)
	}

	part2Payload := []byte("trailing multipart chunk")
	part2, err := s3East1.UploadPart(ctx, &sdks3.UploadPartInput{
		Bucket:     aws.String(bucketName),
		Key:        aws.String("multipart.dat"),
		UploadId:   mpCreate.UploadId,
		PartNumber: aws.Int32(2),
		Body:       bytes.NewReader(part2Payload),
	})
	if err != nil {
		t.Fatalf("upload part 2: %v", err)
	}

	_, err = s3East1.CompleteMultipartUpload(ctx, &sdks3.CompleteMultipartUploadInput{
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

	// Pending multipart upload (uncompleted part retained):
	pendingCreate, err := s3East1.CreateMultipartUpload(ctx, &sdks3.CreateMultipartUploadInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("pending-multipart.dat"),
	})
	if err != nil {
		t.Fatalf("create pending multipart upload: %v", err)
	}

	pendingPartPayload := []byte("pending multipart part chunk body")
	pendingPartUpload, err := s3East1.UploadPart(ctx, &sdks3.UploadPartInput{
		Bucket:     aws.String(bucketName),
		Key:        aws.String("pending-multipart.dat"),
		UploadId:   pendingCreate.UploadId,
		PartNumber: aws.Int32(1),
		Body:       bytes.NewReader(pendingPartPayload),
	})
	if err != nil {
		t.Fatalf("upload pending part: %v", err)
	}
	pendingETag := pendingPartUpload.ETag

	// Versioning enabled on bucket:
	_, err = s3East1.PutBucketVersioning(ctx, &sdks3.PutBucketVersioningInput{
		Bucket: aws.String(bucketName),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	})
	if err != nil {
		t.Fatalf("put bucket versioning: %v", err)
	}

	v1Payload := []byte("version 1 initial payload")
	putV1, err := s3East1.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("versioned.txt"),
		Body:   bytes.NewReader(v1Payload),
	})
	if err != nil {
		t.Fatalf("put version 1: %v", err)
	}
	v1VersionID := putV1.VersionId

	v2Payload := []byte("version 2 updated payload")
	putV2, err := s3East1.PutObject(ctx, &sdks3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("versioned.txt"),
		Body:   bytes.NewReader(v2Payload),
	})
	if err != nil {
		t.Fatalf("put version 2: %v", err)
	}
	v2VersionID := putV2.VersionId

	// Stop initial server instance cleanly.
	stop1()

	// Inject legacy inline body directly into SQLite.
	legacyPayload := []byte("raw legacy inline object body in sqlite")
	legacyRawPayload := []byte("raw legacy unescaped direct record")
	store, err := sqlite.Open(ctx, sourceStorageDir)
	if err != nil {
		t.Fatalf("open sqlite for legacy body: %v", err)
	}

	// Determine bucket prefix dynamically from existing hello.txt record.
	var bucketPrefix string
	_ = store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			if strings.HasSuffix(k, "/o/hello.txt") {
				bucketPrefix = strings.TrimSuffix(k, "/o/hello.txt")
				break
			}
		}
		return nil
	})
	if bucketPrefix == "" {
		t.Fatalf("unable to determine bucket prefix from store")
	}

	legacyObj := map[string]any{
		"ETag":         `"legacy-etag-123"`,
		"ContentType":  "text/plain",
		"Size":         int64(len(legacyPayload)),
		"LastModified": time.Now().UnixMilli(),
	}
	legacyObjJSON, _ := json.Marshal(legacyObj)

	err = store.Update(ctx, func(tx storage.WriteTx) error {
		tx.Put(bucketPrefix+"/o/legacy.txt", legacyObjJSON)
		tx.Put(bucketPrefix+"/db/legacy.txt", legacyPayload)
		tx.Put(bucketPrefix+"/db/legacy-raw.txt", legacyRawPayload)
		return nil
	})
	_ = store.Close()
	if err != nil {
		t.Fatalf("insert legacy body: %v", err)
	}

	// 2. Export source to offline fixture archive.
	fixtureArchive := filepath.Join(t.TempDir(), "sdk-fixture.tar.gz")
	err = archive.Export(ctx, archive.ExportOptions{
		StorageDir: sourceStorageDir,
		OutputFile: fixtureArchive,
	})
	if err != nil {
		t.Fatalf("export fixture archive: %v", err)
	}

	// 3. Verify deterministic round-trip: import into pristine directory and immediately re-export.
	pristineRestoredDir := filepath.Join(t.TempDir(), "pristine-restored")
	err = archive.Import(ctx, archive.ImportOptions{
		InputFile:  fixtureArchive,
		StorageDir: pristineRestoredDir,
	})
	if err != nil {
		t.Fatalf("import pristine directory: %v", err)
	}

	pristineReexportedArchive := filepath.Join(t.TempDir(), "pristine-reexported.tar.gz")
	err = archive.Export(ctx, archive.ExportOptions{
		StorageDir: pristineRestoredDir,
		OutputFile: pristineReexportedArchive,
	})
	if err != nil {
		t.Fatalf("export pristine directory: %v", err)
	}

	mOrig, _, err := archive.InspectArchive(ctx, fixtureArchive)
	if err != nil {
		t.Fatalf("inspect original fixture: %v", err)
	}

	mPristine, _, err := archive.InspectArchive(ctx, pristineReexportedArchive)
	if err != nil {
		t.Fatalf("inspect pristine re-exported fixture: %v", err)
	}

	if mPristine.FormatVersion != mOrig.FormatVersion {
		t.Fatalf("pristine format version mismatch: got %d, want %d", mPristine.FormatVersion, mOrig.FormatVersion)
	}
	if mPristine.TinyStackVersion != mOrig.TinyStackVersion {
		t.Fatalf(
			"pristine tinystack version mismatch: got %s, want %s",
			mPristine.TinyStackVersion,
			mOrig.TinyStackVersion,
		)
	}
	if mPristine.Records != mOrig.Records {
		t.Fatalf("pristine records metadata mismatch: got %+v, want %+v", mPristine.Records, mOrig.Records)
	}
	if mPristine.Totals != mOrig.Totals {
		t.Fatalf("pristine totals mismatch: got %+v, want %+v", mPristine.Totals, mOrig.Totals)
	}
	if len(mPristine.Blobs) != len(mOrig.Blobs) {
		t.Fatalf("pristine blob count mismatch: got %d, want %d", len(mPristine.Blobs), len(mOrig.Blobs))
	}
	for i := range mOrig.Blobs {
		if mPristine.Blobs[i] != mOrig.Blobs[i] {
			t.Fatalf("pristine blob %d mismatch: got %+v, want %+v", i, mPristine.Blobs[i], mOrig.Blobs[i])
		}
	}

	origArchiveBytes, err := os.ReadFile(fixtureArchive)
	if err != nil {
		t.Fatalf("read original fixture archive: %v", err)
	}
	pristineArchiveBytes, err := os.ReadFile(pristineReexportedArchive)
	if err != nil {
		t.Fatalf("read pristine re-exported archive: %v", err)
	}
	if !bytes.Equal(origArchiveBytes, pristineArchiveBytes) {
		t.Fatalf("pristine re-exported archive deterministic byte mismatch: len got %d, want %d",
			len(pristineArchiveBytes), len(origArchiveBytes))
	}

	// 4. Import fixture archive into a brand new storage directory for live SDK server restart.
	restoredStorageDir := filepath.Join(t.TempDir(), "restored-sdk-store")
	err = archive.Import(ctx, archive.ImportOptions{
		InputFile:  fixtureArchive,
		StorageDir: restoredStorageDir,
	})
	if err != nil {
		t.Fatalf("import fixture archive: %v", err)
	}

	// Start fresh TinyStack server on restoredStorageDir.
	s2, stop2 := persistent(t, restoredStorageDir, &fakeRuntime{})

	s3East2 := s3Client(s2, "test", "us-east-1")
	dynamoEast2 := client(s2, "test", "us-east-1")
	dynamoWest2 := client(s2, "test", "us-west-2")
	sqsClient2 := sqsClient(s2, "test")
	iamCli2 := iamClient(s2, "test", "us-east-1")

	// Verify Global IAM role.
	getRoleOut, err := iamCli2.GetRole(ctx, &iam.GetRoleInput{
		RoleName: aws.String("import-test-role"),
	})
	if err != nil {
		t.Fatalf("get restored iam role: %v", err)
	}
	if aws.ToString(getRoleOut.Role.RoleName) != "import-test-role" {
		t.Fatalf("restored role name mismatch: got %s, want import-test-role", aws.ToString(getRoleOut.Role.RoleName))
	}

	// Verify DynamoDB East.
	getEastItem, err := dynamoEast2.GetItem(ctx, &sdkdynamo.GetItemInput{
		TableName: aws.String("east_items"),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "user-east"},
		},
	})
	if err != nil {
		t.Fatalf("get restored east item: %v", err)
	}
	if getEastItem.Item == nil {
		t.Fatalf("restored east item not found")
	}
	if nameAttr, ok := getEastItem.Item["Data"].(*types.AttributeValueMemberS); !ok ||
		nameAttr.Value != "East Coast Record" {
		t.Fatalf("restored east item Data mismatch: %v", getEastItem.Item["Data"])
	}

	// Verify DynamoDB West.
	getWestItem, err := dynamoWest2.GetItem(ctx, &sdkdynamo.GetItemInput{
		TableName: aws.String("west_items"),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "user-west"},
		},
	})
	if err != nil {
		t.Fatalf("get restored west item: %v", err)
	}
	if getWestItem.Item == nil {
		t.Fatalf("restored west item not found")
	}
	if nameAttr, ok := getWestItem.Item["Data"].(*types.AttributeValueMemberS); !ok ||
		nameAttr.Value != "West Coast Record" {
		t.Fatalf("restored west item Data mismatch: %v", getWestItem.Item["Data"])
	}

	// Verify SQS message.
	receiveOut, err := sqsClient2.ReceiveMessage(ctx, &sdksqs.ReceiveMessageInput{
		QueueUrl:            &qURL1,
		MaxNumberOfMessages: 10,
		VisibilityTimeout:   10,
	})
	if err != nil {
		t.Fatalf("receive restored sqs message: %v", err)
	}
	if len(receiveOut.Messages) == 0 {
		t.Fatalf("expected message in restored sqs queue, got 0")
	}
	if aws.ToString(receiveOut.Messages[0].Body) != sqsMsgBody {
		t.Fatalf("sqs message body mismatch: got %q, want %q", aws.ToString(receiveOut.Messages[0].Body), sqsMsgBody)
	}

	// Verify S3 standard object with custom metadata.
	getHello, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("hello.txt"),
	})
	if err != nil {
		t.Fatalf("get restored hello.txt: %v", err)
	}
	helloBody := readBody(t, getHello.Body)
	if !bytes.Equal(helloBody, helloPayload) {
		t.Fatalf("restored hello.txt body mismatch: got %q, want %q", helloBody, helloPayload)
	}
	if getHello.Metadata["custom-meta"] != "value-123" {
		t.Fatalf("restored hello.txt metadata mismatch: got %v", getHello.Metadata)
	}

	// Verify S3 empty object.
	getEmpty, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("empty.bin"),
	})
	if err != nil {
		t.Fatalf("get restored empty.bin: %v", err)
	}
	emptyBody := readBody(t, getEmpty.Body)
	if len(emptyBody) != 0 {
		t.Fatalf("restored empty.bin body mismatch: got %d bytes, want 0", len(emptyBody))
	}

	// Verify S3 binary object with NUL and non-ASCII bytes.
	getBinary, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("binary.bin"),
	})
	if err != nil {
		t.Fatalf("get restored binary.bin: %v", err)
	}
	binaryBody := readBody(t, getBinary.Body)
	if !bytes.Equal(binaryBody, binaryPayload) {
		t.Fatalf("restored binary.bin body mismatch: got %v, want %v", binaryBody, binaryPayload)
	}

	// Verify S3 completed multipart object.
	getMP, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("multipart.dat"),
	})
	if err != nil {
		t.Fatalf("get restored multipart.dat: %v", err)
	}
	mpBody := readBody(t, getMP.Body)
	expectedMP := append([]byte(nil), part1Payload...)
	expectedMP = append(expectedMP, part2Payload...)
	if !bytes.Equal(mpBody, expectedMP) {
		t.Fatalf("restored multipart.dat body mismatch: len got %d, want %d", len(mpBody), len(expectedMP))
	}

	// Verify S3 pending multipart upload retained parts with original ETag, and can be completed.
	listPartsOut, err := s3East2.ListParts(ctx, &sdks3.ListPartsInput{
		Bucket:   aws.String(bucketName),
		Key:      aws.String("pending-multipart.dat"),
		UploadId: pendingCreate.UploadId,
	})
	if err != nil {
		t.Fatalf("list parts on restored pending multipart: %v", err)
	}
	if len(listPartsOut.Parts) != 1 {
		t.Fatalf("restored pending multipart parts count: got %d, want 1", len(listPartsOut.Parts))
	}
	if aws.ToString(listPartsOut.Parts[0].ETag) != aws.ToString(pendingETag) {
		t.Fatalf("restored pending part ETag mismatch: got %v, want %v", listPartsOut.Parts[0].ETag, pendingETag)
	}
	if aws.ToInt64(listPartsOut.Parts[0].Size) != int64(len(pendingPartPayload)) {
		t.Fatalf(
			"restored pending part size: got %d, want %d",
			aws.ToInt64(listPartsOut.Parts[0].Size),
			len(pendingPartPayload),
		)
	}

	// Complete the restored pending multipart upload with the original part ETag.
	_, err = s3East2.CompleteMultipartUpload(ctx, &sdks3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucketName),
		Key:      aws.String("pending-multipart.dat"),
		UploadId: pendingCreate.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: []s3types.CompletedPart{
				{ETag: pendingETag, PartNumber: aws.Int32(1)},
			},
		},
	})
	if err != nil {
		t.Fatalf("complete restored pending multipart upload: %v", err)
	}

	// Read and verify the resulting completed object body.
	getCompletedPending, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("pending-multipart.dat"),
	})
	if err != nil {
		t.Fatalf("get completed pending multipart object: %v", err)
	}
	completedPendingBody := readBody(t, getCompletedPending.Body)
	if !bytes.Equal(completedPendingBody, pendingPartPayload) {
		t.Fatalf("completed pending multipart body mismatch: got %q, want %q", completedPendingBody, pendingPartPayload)
	}

	// Verify S3 versioned objects: current version (v2).
	getVCurrent, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("versioned.txt"),
	})
	if err != nil {
		t.Fatalf("get current versioned.txt: %v", err)
	}
	if aws.ToString(getVCurrent.VersionId) != aws.ToString(v2VersionID) {
		t.Fatalf("current version ID mismatch: got %v, want %v", getVCurrent.VersionId, v2VersionID)
	}
	currentBody := readBody(t, getVCurrent.Body)
	if !bytes.Equal(currentBody, v2Payload) {
		t.Fatalf("current versioned.txt mismatch: got %q, want %q", currentBody, v2Payload)
	}

	// Verify S3 versioned objects: noncurrent version (v1).
	getV1, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket:    aws.String(bucketName),
		Key:       aws.String("versioned.txt"),
		VersionId: v1VersionID,
	})
	if err != nil {
		t.Fatalf("get noncurrent v1 versioned.txt: %v", err)
	}
	if aws.ToString(getV1.VersionId) != aws.ToString(v1VersionID) {
		t.Fatalf("noncurrent version ID mismatch: got %v, want %v", getV1.VersionId, v1VersionID)
	}
	v1Body := readBody(t, getV1.Body)
	if !bytes.Equal(v1Body, v1Payload) {
		t.Fatalf("noncurrent v1 versioned.txt mismatch: got %q, want %q", v1Body, v1Payload)
	}

	// Verify S3 legacy inline body served via SDK GetObject.
	getLegacy, err := s3East2.GetObject(ctx, &sdks3.GetObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String("legacy.txt"),
	})
	if err != nil {
		t.Fatalf("get restored legacy.txt: %v", err)
	}
	legacyBody := readBody(t, getLegacy.Body)
	if !bytes.Equal(legacyBody, legacyPayload) {
		t.Fatalf("restored legacy.txt body mismatch: got %q, want %q", legacyBody, legacyPayload)
	}

	// 5. Verify refusal semantics: attempting to import into restoredStorageDir must fail.
	err = archive.Import(ctx, archive.ImportOptions{
		InputFile:  fixtureArchive,
		StorageDir: restoredStorageDir,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected refusal when re-importing into existing directory, got %v", err)
	}

	// Stop server.
	stop2()

	// Verify raw legacy record was preserved in SQLite.
	restoredStore, err := sqlite.Open(ctx, restoredStorageDir)
	if err != nil {
		t.Fatalf("open restored sqlite: %v", err)
	}
	_ = restoredStore.View(ctx, func(tx storage.ReadTx) error {
		val, ok := tx.Get(bucketPrefix + "/db/legacy-raw.txt")
		if !ok || !bytes.Equal(val, legacyRawPayload) {
			t.Fatalf("legacy raw record mismatch: got %q, want %q; all keys: %v", val, legacyRawPayload, tx.Keys(""))
		}
		return nil
	})
	_ = restoredStore.Close()
}
