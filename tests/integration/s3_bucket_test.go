package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestS3DeleteObjects(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("batch")}); err != nil {
		t.Fatal(err)
	}

	var objects []s3types.ObjectIdentifier
	for i := range 3 {
		key := fmt.Sprintf("logs/%d.txt", i)
		putObject(t, c, "batch", key, "x", nil)
		objects = append(objects, s3types.ObjectIdentifier{Key: aws.String(key)})
	}

	// A key that does not exist is deleted too.
	objects = append(objects, s3types.ObjectIdentifier{Key: aws.String("missing")})
	out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("batch"), Delete: &s3types.Delete{Objects: objects},
	})
	if err != nil || len(out.Deleted) != 4 || len(out.Errors) != 0 {
		t.Fatalf("%+v %v", out, err)
	}

	listed, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("batch")})
	if err != nil || len(listed.Contents) != 0 {
		t.Fatalf("left: %+v %v", listed, err)
	}

	putObject(t, c, "batch", "kept", "x", nil)
	out, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("batch"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("kept")}}, Quiet: aws.Bool(true),
	}})
	if err != nil || len(out.Deleted) != 0 || len(out.Errors) != 0 {
		t.Fatalf("quiet: %+v %v", out, err)
	}

	// A versioned bucket hides each key behind a delete marker.
	if _, err = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("history")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String("history"),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})
	if err != nil {
		t.Fatal(err)
	}

	version := putVersion(t, c, "history", "a", "1")
	out, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("history"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("a")}},
	}})
	if err != nil || len(out.Deleted) != 1 || !aws.ToBool(out.Deleted[0].DeleteMarker) ||
		aws.ToString(out.Deleted[0].DeleteMarkerVersionId) == "" {
		t.Fatalf("marker: %+v %v", out, err)
	}

	marker := aws.ToString(out.Deleted[0].DeleteMarkerVersionId)
	out, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("history"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{
			{Key: aws.String("a"), VersionId: aws.String(marker)},
			{Key: aws.String("a"), VersionId: aws.String(version)},
		},
	}})
	if err != nil || len(out.Deleted) != 2 || !aws.ToBool(out.Deleted[0].DeleteMarker) ||
		aws.ToString(out.Deleted[1].VersionId) != version {
		t.Fatalf("versions: %+v %v", out, err)
	}

	if _, err = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("history")}); err != nil {
		t.Fatalf("the bucket still has versions: %v", err)
	}

	_, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("batch"), Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{}},
	})
	if code, status := s3Code(err); code != "MalformedXML" || status != 400 {
		t.Fatalf("no objects: %s %d %v", code, status, err)
	}

	_, err = c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("nothing"), Delete: &s3types.Delete{
		Objects: []s3types.ObjectIdentifier{{Key: aws.String("a")}},
	}})
	if code, _ := s3Code(err); code != "NoSuchBucket" {
		t.Fatalf("missing bucket: %v", err)
	}
}

func TestS3GetBucketLocation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	east := s3Client(s, "test", "us-east-1")
	if _, err := east.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("east")}); err != nil {
		t.Fatal(err)
	}

	out, err := east.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("east")})
	if err != nil || out.LocationConstraint != "" {
		t.Fatalf("us-east-1: %+v %v", out, err)
	}

	west := s3Client(s, "test", "eu-west-1")
	_, err = west.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String("west"),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraintEuWest1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err = west.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("west")})
	if err != nil || out.LocationConstraint != s3types.BucketLocationConstraintEuWest1 {
		t.Fatalf("eu-west-1: %+v %v", out, err)
	}

	// Any Region answers.
	out, err = east.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("west")})
	if err != nil || out.LocationConstraint != s3types.BucketLocationConstraintEuWest1 {
		t.Fatalf("from us-east-1: %+v %v", out, err)
	}

	_, err = s3Client(s, "team-b", "us-east-1").GetBucketLocation(ctx, &s3.GetBucketLocationInput{
		Bucket: aws.String("east"),
	})
	if code, status := s3Code(err); code != "AccessDenied" || status != 403 {
		t.Fatalf("another account: %s %d %v", code, status, err)
	}

	_, err = east.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String("nothing")})
	if code, _ := s3Code(err); code != "NoSuchBucket" {
		t.Fatalf("missing bucket: %v", err)
	}
}

func TestS3BucketPolicy(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("site")}); err != nil {
		t.Fatal(err)
	}

	_, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("site")})
	if code, status := s3Code(err); code != "NoSuchBucketPolicy" || status != 404 {
		t.Fatalf("no policy: %s %d %v", code, status, err)
	}

	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",` +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
	// A new bucket blocks public policies.
	_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("site"), Policy: aws.String(policy)})
	if code, status := s3Code(err); code != "AccessDenied" || status != 403 {
		t.Fatalf("blocked: %s %d %v", code, status, err)
	}

	if _, err = c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{
		Bucket: aws.String("site"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("site"), Policy: aws.String(policy),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("site")})
	if err != nil || aws.ToString(got.Policy) != policy {
		t.Fatalf("%+v %v", got, err)
	}

	status, err := c.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: aws.String("site")})
	if err != nil || !aws.ToBool(status.PolicyStatus.IsPublic) {
		t.Fatalf("status: %+v %v", status, err)
	}

	private := `{"Statement":{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:root"},` +
		`"Action":"s3:*","Resource":"*"}}`
	if _, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String("site"), Policy: aws.String(private),
	}); err != nil {
		t.Fatal(err)
	}

	status, err = c.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: aws.String("site")})
	if err != nil || aws.ToBool(status.PolicyStatus.IsPublic) {
		t.Fatalf("private status: %+v %v", status, err)
	}

	for _, bad := range []string{"not json", `{"Version":"2012-10-17"}`, `{"Statement":[]}`} {
		_, err = c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("site"), Policy: aws.String(bad)})
		if code, _ := s3Code(err); code != "MalformedPolicy" {
			t.Fatalf("%s: %v", bad, err)
		}
	}

	if _, err = c.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String("site")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("site")})
	if code, _ := s3Code(err); code != "NoSuchBucketPolicy" {
		t.Fatalf("deleted: %v", err)
	}
}

// preflight sends a browser's CORS preflight for a PUT to the web bucket.
func preflight(t *testing.T, url, origin, headers string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, url+"/web/upload.txt", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", headers)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	return resp
}

func TestS3BucketCors(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("web")}); err != nil {
		t.Fatal(err)
	}

	_, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String("web")})
	if code, status := s3Code(err); code != "NoSuchCORSConfiguration" || status != 404 {
		t.Fatalf("no configuration: %s %d %v", code, status, err)
	}

	rule := s3types.CORSRule{
		ID: aws.String("uploads"), AllowedOrigins: []string{"http://localhost:3000"},
		AllowedMethods: []string{"GET", "PUT"}, AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag"},
		MaxAgeSeconds: aws.Int32(600),
	}
	if _, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String("web"), CORSConfiguration: &s3types.CORSConfiguration{CORSRules: []s3types.CORSRule{rule}},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String("web")})
	if err != nil || len(got.CORSRules) != 1 || aws.ToString(got.CORSRules[0].ID) != "uploads" ||
		len(got.CORSRules[0].AllowedMethods) != 2 || aws.ToInt32(got.CORSRules[0].MaxAgeSeconds) != 600 {
		t.Fatalf("%+v %v", got, err)
	}

	resp := preflight(t, s.URL, "http://localhost:3000", "Content-Type, X-Amz-Meta-Owner")
	if resp.StatusCode != http.StatusOK ||
		resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" ||
		resp.Header.Get("Access-Control-Allow-Methods") != "GET, PUT" ||
		resp.Header.Get("Access-Control-Allow-Headers") != "content-type, x-amz-meta-owner" ||
		resp.Header.Get("Access-Control-Max-Age") != "600" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}

	if resp = preflight(t, s.URL, "http://evil.example", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("another origin: %d %v", resp.StatusCode, resp.Header)
	}

	putObject(t, c, "web", "upload.txt", "x", nil)
	req, err := http.NewRequest(http.MethodGet, s.URL+"/web/upload.txt", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Origin", "http://localhost:3000")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" ||
		resp.Header.Get("Access-Control-Expose-Headers") != "ETag" {
		t.Fatalf("GET: %d %v", resp.StatusCode, resp.Header)
	}

	for _, bad := range []s3types.CORSRule{
		{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"PATCH"}},
		{AllowedOrigins: []string{"http://*.*.example"}, AllowedMethods: []string{"GET"}},
	} {
		_, err = c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
			Bucket: aws.String(
				"web",
			),
			CORSConfiguration: &s3types.CORSConfiguration{CORSRules: []s3types.CORSRule{bad}},
		})
		if code, _ := s3Code(err); code != "InvalidRequest" {
			t.Fatalf("%+v: %v", bad, err)
		}
	}

	if _, err = c.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String("web")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String("web")})
	if code, _ := s3Code(err); code != "NoSuchCORSConfiguration" {
		t.Fatalf("deleted: %v", err)
	}

	if resp = preflight(t, s.URL, "http://localhost:3000", ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no configuration: %d", resp.StatusCode)
	}
}

func TestS3BucketEncryption(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("vault")}); err != nil {
		t.Fatal(err)
	}

	algorithm := func() (s3types.ServerSideEncryption, string) {
		t.Helper()
		out, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: aws.String("vault")})
		if err != nil || len(out.ServerSideEncryptionConfiguration.Rules) != 1 {
			t.Fatalf("%+v %v", out, err)
		}

		d := out.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault
		return d.SSEAlgorithm, aws.ToString(d.KMSMasterKeyID)
	}

	if got, _ := algorithm(); got != s3types.ServerSideEncryptionAes256 {
		t.Fatalf("default: %s", got)
	}

	encrypt := func(algorithm s3types.ServerSideEncryption) error {
		_, err := c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
			Bucket: aws.String("vault"),
			ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
				Rules: []s3types.ServerSideEncryptionRule{{
					ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
						SSEAlgorithm: algorithm, KMSMasterKeyID: aws.String("alias/app"),
					},
					BucketKeyEnabled: aws.Bool(true),
				}},
			},
		})
		return err
	}

	if err := encrypt(s3types.ServerSideEncryptionAwsKms); err != nil {
		t.Fatal(err)
	}

	if got, key := algorithm(); got != s3types.ServerSideEncryptionAwsKms || key != "alias/app" {
		t.Fatalf("KMS: %s %s", got, key)
	}

	if code, _ := s3Code(encrypt(s3types.ServerSideEncryptionAes256)); code != "InvalidArgument" {
		t.Fatal("SSE-S3 took a KMS key")
	}

	if _, err := c.DeleteBucketEncryption(
		ctx,
		&s3.DeleteBucketEncryptionInput{Bucket: aws.String("vault")},
	); err != nil {
		t.Fatal(err)
	}

	if got, _ := algorithm(); got != s3types.ServerSideEncryptionAes256 {
		t.Fatalf("deleted: %s", got)
	}
}

func TestS3BucketAccess(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("locked")}); err != nil {
		t.Fatal(err)
	}

	ownership, err := c.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{
		Bucket: aws.String("locked"),
	})
	if err != nil || len(ownership.OwnershipControls.Rules) != 1 ||
		ownership.OwnershipControls.Rules[0].ObjectOwnership != s3types.ObjectOwnershipBucketOwnerEnforced {
		t.Fatalf("new bucket ownership: %+v %v", ownership, err)
	}

	block, err := c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String("locked")})
	if err != nil || !aws.ToBool(block.PublicAccessBlockConfiguration.BlockPublicPolicy) ||
		!aws.ToBool(block.PublicAccessBlockConfiguration.RestrictPublicBuckets) {
		t.Fatalf("new bucket block: %+v %v", block, err)
	}

	if _, err = c.PutBucketOwnershipControls(ctx, &s3.PutBucketOwnershipControlsInput{
		Bucket: aws.String("locked"),
		OwnershipControls: &s3types.OwnershipControls{Rules: []s3types.OwnershipControlsRule{{
			ObjectOwnership: s3types.ObjectOwnershipObjectWriter,
		}}},
	}); err != nil {
		t.Fatal(err)
	}

	ownership, err = c.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{
		Bucket: aws.String("locked"),
	})
	if err != nil || ownership.OwnershipControls.Rules[0].ObjectOwnership != s3types.ObjectOwnershipObjectWriter {
		t.Fatalf("ownership: %+v %v", ownership, err)
	}

	if _, err = c.DeleteBucketOwnershipControls(ctx, &s3.DeleteBucketOwnershipControlsInput{
		Bucket: aws.String("locked"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{Bucket: aws.String("locked")})
	if code, status := s3Code(err); code != "OwnershipControlsNotFoundError" || status != 404 {
		t.Fatalf("deleted ownership: %s %d %v", code, status, err)
	}

	if _, err = c.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket:                         aws.String("locked"),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true)},
	}); err != nil {
		t.Fatal(err)
	}

	block, err = c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String("locked")})
	if err != nil || !aws.ToBool(block.PublicAccessBlockConfiguration.BlockPublicAcls) ||
		aws.ToBool(block.PublicAccessBlockConfiguration.BlockPublicPolicy) {
		t.Fatalf("block: %+v %v", block, err)
	}

	if _, err = c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{
		Bucket: aws.String("locked"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String("locked")})
	if code, status := s3Code(err); code != "NoSuchPublicAccessBlockConfiguration" || status != 404 {
		t.Fatalf("deleted block: %s %d %v", code, status, err)
	}
}

func TestS3BucketLifecycle(t *testing.T) {
	c := s3Client(server(t), "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("logs")}); err != nil {
		t.Fatal(err)
	}

	_, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String("logs"),
	})
	if code, status := s3Code(err); code != "NoSuchLifecycleConfiguration" || status != 404 {
		t.Fatalf("no configuration: %s %d %v", code, status, err)
	}

	put := func(rules ...s3types.LifecycleRule) error {
		_, putErr := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
			Bucket:                 aws.String("logs"),
			LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: rules},
		})
		return putErr
	}

	expire := s3types.LifecycleRule{
		ID: aws.String("expire"), Status: s3types.ExpirationStatusEnabled,
		Filter:     &s3types.LifecycleRuleFilter{Prefix: aws.String("tmp/")},
		Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(7)},
		Transitions: []s3types.Transition{{
			Days: aws.Int32(1), StorageClass: s3types.TransitionStorageClassStandardIa,
		}},
		AbortIncompleteMultipartUpload: &s3types.AbortIncompleteMultipartUpload{DaysAfterInitiation: aws.Int32(2)},
	}
	tagged := s3types.LifecycleRule{
		ID: aws.String("tagged"), Status: s3types.ExpirationStatusDisabled,
		Filter: &s3types.LifecycleRuleFilter{And: &s3types.LifecycleRuleAndOperator{
			Prefix: aws.String("data/"), Tags: []s3types.Tag{{Key: aws.String("keep"), Value: aws.String("no")}},
		}},
		NoncurrentVersionExpiration: &s3types.NoncurrentVersionExpiration{NoncurrentDays: aws.Int32(30)},
	}
	if err = put(expire, tagged); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String("logs"),
	})
	if err != nil || len(got.Rules) != 2 || aws.ToInt32(got.Rules[0].Expiration.Days) != 7 ||
		aws.ToString(got.Rules[0].Filter.Prefix) != "tmp/" ||
		got.Rules[0].Transitions[0].StorageClass != s3types.TransitionStorageClassStandardIa ||
		len(got.Rules[1].Filter.And.Tags) != 1 ||
		got.TransitionDefaultMinimumObjectSize != s3types.TransitionDefaultMinimumObjectSizeAllStorageClasses128k {
		t.Fatalf("%+v %v", got, err)
	}

	for _, bad := range []s3types.LifecycleRule{
		{ID: aws.String("none"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{}},
		{
			ID: aws.String("zero"), Status: s3types.ExpirationStatusEnabled, Filter: &s3types.LifecycleRuleFilter{},
			Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(0)},
		},
		expire, // the same ID twice
	} {
		rules := []s3types.LifecycleRule{bad}
		if aws.ToString(bad.ID) == "expire" {
			rules = append(rules, expire)
		}

		if code, _ := s3Code(put(rules...)); code != "InvalidArgument" {
			t.Fatalf("%s was accepted", aws.ToString(bad.ID))
		}
	}

	if _, err = c.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: aws.String("logs")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String("logs"),
	})
	if code, _ := s3Code(err); code != "NoSuchLifecycleConfiguration" {
		t.Fatalf("deleted: %v", err)
	}
}
