package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cf "github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/cloudfrontkeyvaluestore"
	"github.com/aws/aws-sdk-go-v2/service/cloudfrontkeyvaluestore/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amjadjibon/tinystack/internal/config"
)

func kvsClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(strings.Replace(s.URL, "127.0.0.1", "localhost", 1)),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func TestCloudFrontKVSData(t *testing.T) {
	s := server(t)
	control := cloudfrontClient(s)
	data := kvsClient(s)
	ctx := context.Background()
	store, err := control.CreateKeyValueStore(ctx, &cf.CreateKeyValueStoreInput{Name: new("data")})
	if err != nil {
		t.Fatal(err)
	}

	arn := store.KeyValueStore.ARN
	desc, err := data.DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{KvsARN: arn})
	if err != nil || aws.ToString(desc.Status) != "READY" || *desc.ETag == *store.ETag {
		t.Fatalf("describe %+v %v", desc, err)
	}

	put, err := data.PutKey(ctx, &sdk.PutKeyInput{KvsARN: arn, IfMatch: desc.ETag, Key: new("a/b"), Value: new("one")})
	if err != nil || aws.ToInt32(put.ItemCount) != 1 || aws.ToInt64(put.TotalSizeInBytes) != 6 {
		t.Fatalf("put %+v %v", put, err)
	}

	_, err = data.PutKey(ctx, &sdk.PutKeyInput{KvsARN: arn, IfMatch: desc.ETag, Key: new("stale"), Value: new("no")})
	wantAccessAnalyzerError(t, err, "ConflictException")
	got, err := data.GetKey(ctx, &sdk.GetKeyInput{KvsARN: arn, Key: new("a/b")})
	if err != nil || aws.ToString(got.Value) != "one" {
		t.Fatalf("get %+v %v", got, err)
	}

	batch, err := data.UpdateKeys(
		ctx,
		&sdk.UpdateKeysInput{
			KvsARN:  arn,
			IfMatch: put.ETag,
			Puts: []types.PutKeyRequestListItem{
				{Key: new("b"), Value: new("two")},
				{Key: new("c"), Value: new("three")},
			},
			Deletes: []types.DeleteKeyRequestListItem{{Key: new("a/b")}},
		},
	)
	if err != nil || aws.ToInt32(batch.ItemCount) != 2 {
		t.Fatalf("batch %+v %v", batch, err)
	}

	page, err := data.ListKeys(ctx, &sdk.ListKeysInput{KvsARN: arn, MaxResults: new(int32(1))})
	if err != nil || len(page.Items) != 1 || page.NextToken == nil || *page.Items[0].Key != "b" {
		t.Fatalf("page %+v %v", page, err)
	}

	page, err = data.ListKeys(
		ctx,
		&sdk.ListKeysInput{KvsARN: arn, MaxResults: new(int32(1)), NextToken: page.NextToken},
	)
	if err != nil || len(page.Items) != 1 || *page.Items[0].Key != "c" || page.NextToken != nil {
		t.Fatalf("next %+v %v", page, err)
	}

	for _, pair := range [][2]string{
		{strings.Repeat("k", 513), "v"},
		{"k", strings.Repeat("v", 1025)},
		{strings.Repeat("é", 257), "v"},
	} {
		_, err = data.PutKey(
			ctx,
			&sdk.PutKeyInput{KvsARN: arn, IfMatch: batch.ETag, Key: new(pair[0]), Value: new(pair[1])},
		)
		wantAccessAnalyzerError(t, err, "ValidationException")
	}

	_, err = data.UpdateKeys(
		ctx,
		&sdk.UpdateKeysInput{
			KvsARN:  arn,
			IfMatch: batch.ETag,
			Puts: []types.PutKeyRequestListItem{
				{Key: new("valid"), Value: new("value")},
				{Key: new("invalid"), Value: new(strings.Repeat("x", 1025))},
			},
		},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = data.GetKey(ctx, &sdk.GetKeyInput{KvsARN: arn, Key: new("valid")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	deleted, err := data.DeleteKey(ctx, &sdk.DeleteKeyInput{KvsARN: arn, IfMatch: batch.ETag, Key: new("b")})
	if err != nil || aws.ToInt32(deleted.ItemCount) != 1 {
		t.Fatalf("delete %+v %v", deleted, err)
	}

	_, err = data.GetKey(ctx, &sdk.GetKeyInput{KvsARN: arn, Key: new("b")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = data.DescribeKeyValueStore(
		ctx,
		&sdk.DescribeKeyValueStoreInput{
			KvsARN: new(strings.Replace(*arn, "key-value-store/", "key-value-store/missing-", 1)),
		},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
}

func TestCloudFrontKVSImport(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	buckets := s3Client(s, "test", "us-east-1")
	_, err := buckets.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: new("kvs-import")})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, body, status string }{
		{"valid", `{"data":[{"key":"hello","value":"world"}]}`, "READY"},
		{"malformed", `{"data":false}`, "IMPORT_FAILURE"},
		{"duplicate", `{"data":[{"key":"a","value":"b"},{"key":"a","value":"c"}]}`, "IMPORT_FAILURE"},
	} {
		_, err = buckets.PutObject(
			ctx,
			&s3.PutObjectInput{Bucket: new("kvs-import"), Key: new(tc.name), Body: strings.NewReader(tc.body)},
		)
		if err != nil {
			t.Fatal(err)
		}

		store, createErr := cloudfrontClient(s).CreateKeyValueStore(ctx, &cf.CreateKeyValueStoreInput{
			Name: new(tc.name), ImportSource: &cftypes.ImportSource{
				SourceType: cftypes.ImportSourceTypeS3, SourceARN: new("arn:aws:s3:::kvs-import/" + tc.name),
			},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}

		desc, describeErr := kvsClient(
			s,
		).DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{KvsARN: store.KeyValueStore.ARN})
		if describeErr != nil || aws.ToString(desc.Status) != tc.status {
			t.Fatalf("import %+v %v", desc, describeErr)
		}

		if tc.status == "READY" && aws.ToInt32(desc.ItemCount) != 1 {
			t.Fatal(desc)
		}

		if tc.status != "READY" && aws.ToString(desc.FailureReason) == "" {
			t.Fatal(desc)
		}
	}

	_, err = cloudfrontClient(
		s,
	).CreateKeyValueStore(ctx, &cf.CreateKeyValueStoreInput{
		Name: new("missing"), ImportSource: &cftypes.ImportSource{
			SourceType: cftypes.ImportSourceTypeS3, SourceARN: new("arn:aws:s3:::kvs-import/missing"),
		},
	})
	wantAccessAnalyzerError(t, err, "InvalidArgument")
}

func TestCloudFrontKVSRestart(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	ctx := context.Background()
	store, err := cloudfrontClient(s).CreateKeyValueStore(ctx, &cf.CreateKeyValueStoreInput{Name: new("persistent")})
	if err != nil {
		t.Fatal(err)
	}

	desc, err := kvsClient(
		s,
	).DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{KvsARN: store.KeyValueStore.ARN})
	if err != nil {
		t.Fatal(err)
	}

	put, err := kvsClient(
		s,
	).PutKey(ctx, &sdk.PutKeyInput{
		KvsARN: store.KeyValueStore.ARN, IfMatch: desc.ETag, Key: new("key"), Value: new("persisted"),
	})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	desc, err = kvsClient(
		s,
	).DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{KvsARN: store.KeyValueStore.ARN})
	if err != nil || *desc.ETag != *put.ETag || aws.ToInt32(desc.ItemCount) != 1 {
		t.Fatalf("restart %+v %v", desc, err)
	}

	got, err := kvsClient(s).GetKey(ctx, &sdk.GetKeyInput{KvsARN: store.KeyValueStore.ARN, Key: new("key")})
	if err != nil || aws.ToString(got.Value) != "persisted" {
		t.Fatalf("restart key %+v %v", got, err)
	}
}

func TestCloudFrontKVSVerify(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	ctx := context.Background()
	store, err := cloudfrontClient(s).CreateKeyValueStore(ctx, &cf.CreateKeyValueStoreInput{Name: new("verified")})
	if err != nil {
		t.Fatal(err)
	}

	// The SDK signs every data-plane call with SigV4a; a verified one reaches the store.
	described, err := kvsClient(s).DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{
		KvsARN: store.KeyValueStore.ARN,
	})
	if err != nil || aws.ToString(described.KvsARN) != aws.ToString(store.KeyValueStore.ARN) {
		t.Fatalf("verified SigV4a call: %+v %v", described, err)
	}

	forged := sdk.New(sdk.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(strings.Replace(s.URL, "127.0.0.1", "localhost", 1)),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "wrong"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
	_, err = forged.DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{KvsARN: store.KeyValueStore.ARN})
	wantAccessAnalyzerError(t, err, "InvalidSignatureException")
}

func TestCloudFrontKVSStoreLimit(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	made, err := cloudfrontClient(s).CreateKeyValueStore(ctx, &cf.CreateKeyValueStoreInput{Name: new("size-limit")})
	if err != nil {
		t.Fatal(err)
	}

	c := kvsClient(s)
	arn := made.KeyValueStore.ARN
	desc, err := c.DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{KvsARN: arn})
	if err != nil {
		t.Fatal(err)
	}

	etag := desc.ETag
	puts := func(start, n int) []types.PutKeyRequestListItem {
		out := make([]types.PutKeyRequestListItem, n)
		for i := range n {
			out[i] = types.PutKeyRequestListItem{
				Key:   new(fmt.Sprintf("%0512d", start+i)),
				Value: new(strings.Repeat("x", 1024)),
			}
		}

		return out
	}

	_, err = c.UpdateKeys(ctx, &sdk.UpdateKeysInput{KvsARN: arn, IfMatch: etag, Puts: puts(0, 51)})
	wantAccessAnalyzerError(t, err, "ServiceQuotaExceededException")
	for i := range 68 {
		out, e := c.UpdateKeys(ctx, &sdk.UpdateKeysInput{KvsARN: arn, IfMatch: etag, Puts: puts(i*50, 50)})
		if e != nil {
			t.Fatal(e)
		}

		etag = out.ETag
	}

	_, err = c.UpdateKeys(ctx, &sdk.UpdateKeysInput{KvsARN: arn, IfMatch: etag, Puts: puts(3400, 14)})
	wantAccessAnalyzerError(t, err, "ServiceQuotaExceededException")
	after, err := c.DescribeKeyValueStore(ctx, &sdk.DescribeKeyValueStoreInput{KvsARN: arn})
	if err != nil || aws.ToInt32(after.ItemCount) != 3400 || *after.ETag != *etag {
		t.Fatalf("quota rollback %+v %v", after, err)
	}
}
