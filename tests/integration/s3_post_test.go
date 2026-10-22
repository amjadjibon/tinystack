package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amjadjibon/tinystack/internal/config"
)

type postResponseXML struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func executePostForm(
	t *testing.T,
	postURL string,
	fields map[string]string,
	fileName string,
	fileContent []byte,
	fileContentType string,
) (resp *http.Response, respBody string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	for k, v := range fields {
		if fieldErr := mw.WriteField(k, v); fieldErr != nil {
			t.Fatal(fieldErr)
		}
	}

	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName)}
	if fileContentType != "" {
		h["Content-Type"] = []string{fileContentType}
	}
	part, partErr := mw.CreatePart(h)
	if partErr != nil {
		t.Fatal(partErr)
	}
	if _, writeErr := part.Write(fileContent); writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr := mw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(http.MethodPost, postURL, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	var doErr error
	resp, doErr = client.Do(req)
	if doErr != nil {
		t.Fatal(doErr)
	}

	data, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	respBody = string(data)

	return resp, respBody
}

func TestS3PostUpload(t *testing.T) {
	s := server(t)
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()

	bucket := "post-upload-bucket"
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}

	presignClient := s3.NewPresignClient(c)

	// 1. Success 204 default
	presigned, err := presignClient.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("simple.txt"),
	})
	if err != nil {
		t.Fatalf("PresignPostObject: %v", err)
	}

	resp, respBody := executePostForm(
		t,
		presigned.URL,
		presigned.Values,
		"simple.txt",
		[]byte("hello s3"),
		"text/plain",
	)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d: %s", resp.StatusCode, respBody)
	}

	// Verify object exists
	getOut, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("simple.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	data, _ := io.ReadAll(getOut.Body)
	_ = getOut.Body.Close()
	if string(data) != "hello s3" {
		t.Fatalf("expected 'hello s3', got %q", string(data))
	}

	// 2. ${filename} substitution
	presignedFilename, err := presignClient.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("uploads/${filename}"),
	}, func(o *s3.PresignPostOptions) {
		o.Conditions = append(o.Conditions, []any{"starts-with", "$key", "uploads/"})
	})
	if err != nil {
		t.Fatalf("PresignPostObject: %v", err)
	}

	resp, respBody = executePostForm(
		t,
		presignedFilename.URL,
		presignedFilename.Values,
		"document.pdf",
		[]byte("pdf data"),
		"application/pdf",
	)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.StatusCode, respBody)
	}

	getDoc, err := c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("uploads/document.pdf"),
	})
	if err != nil {
		t.Fatalf("GetObject uploads/document.pdf: %v", err)
	}
	docData, _ := io.ReadAll(getDoc.Body)
	_ = getDoc.Body.Close()
	if string(docData) != "pdf data" {
		t.Fatalf("expected 'pdf data', got %q", string(docData))
	}

	// 3. success_action_status 201 returns XML PostResponse
	presigned201, err := presignClient.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("item201.txt"),
	}, func(o *s3.PresignPostOptions) {
		o.Conditions = append(o.Conditions, map[string]any{"success_action_status": "201"})
	})
	if err != nil {
		t.Fatalf("PresignPostObject: %v", err)
	}

	presigned201.Values["success_action_status"] = "201"
	resp, respBody = executePostForm(
		t,
		presigned201.URL,
		presigned201.Values,
		"item201.txt",
		[]byte("status 201 content"),
		"text/plain",
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", resp.StatusCode, respBody)
	}

	var postResp postResponseXML
	if unmarshalErr := xml.Unmarshal([]byte(respBody), &postResp); unmarshalErr != nil {
		t.Fatalf("failed to decode PostResponse XML: %v (body: %s)", unmarshalErr, respBody)
	}
	if postResp.Bucket != bucket || postResp.Key != "item201.txt" || postResp.ETag == "" {
		t.Fatalf("unexpected PostResponse: %+v", postResp)
	}

	// 4. success_action_redirect returns 303 See Other
	presignedRedir, err := presignClient.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("item-redirect.txt"),
	}, func(o *s3.PresignPostOptions) {
		o.Conditions = append(
			o.Conditions,
			[]any{"starts-with", "$success_action_redirect", "http://example.com/callback"},
		)
	})
	if err != nil {
		t.Fatalf("PresignPostObject: %v", err)
	}

	presignedRedir.Values["success_action_redirect"] = "http://example.com/callback"
	resp, _ = executePostForm(
		t,
		presignedRedir.URL,
		presignedRedir.Values,
		"item-redirect.txt",
		[]byte("redirect data"),
		"text/plain",
	)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 See Other, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "http://example.com/callback") {
		t.Fatalf("expected redirect to example.com/callback, got %q", loc)
	}
	parsedLoc, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if parsedLoc.Query().Get("bucket") != bucket ||
		parsedLoc.Query().Get("key") != "item-redirect.txt" ||
		parsedLoc.Query().Get("etag") == "" {
		t.Fatalf("unexpected redirect query parameters: %v", parsedLoc.RawQuery)
	}

	// 5. Condition failure returns AccessDenied (403)
	presignedCondFail, err := presignClient.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String("cond-fail.txt"),
	}, func(o *s3.PresignPostOptions) {
		o.Conditions = append(o.Conditions, []any{"eq", "$Content-Type", "image/png"})
	})
	if err != nil {
		t.Fatalf("PresignPostObject: %v", err)
	}

	presignedCondFail.Values["Content-Type"] = "text/plain"
	resp, respBody = executePostForm(
		t,
		presignedCondFail.URL,
		presignedCondFail.Values,
		"cond-fail.txt",
		[]byte("data"),
		"text/plain",
	)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(respBody, "AccessDenied") {
		t.Fatalf("expected 403 AccessDenied for condition failure, got %d: %s", resp.StatusCode, respBody)
	}

	// 6. Expired policy returns AccessDenied (403)
	expiredPolicyJSON := fmt.Sprintf(`{
		"expiration": %q,
		"conditions": [
			{"bucket": %q},
			{"key": "expired.txt"}
		]
	}`, time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339), bucket)
	expiredB64 := base64.StdEncoding.EncodeToString([]byte(expiredPolicyJSON))

	expiredFields := map[string]string{
		"key":    "expired.txt",
		"policy": expiredB64,
	}
	resp, respBody = executePostForm(t, s.URL+"/"+bucket, expiredFields, "expired.txt", []byte("data"), "text/plain")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(respBody, "AccessDenied") {
		t.Fatalf("expected 403 AccessDenied for expired policy, got %d: %s", resp.StatusCode, respBody)
	}

	// 7. content-length-range failure
	policyRangeJSON := fmt.Sprintf(`{
		"expiration": %q,
		"conditions": [
			{"bucket": %q},
			["starts-with", "$key", ""],
			["content-length-range", 10, 20]
		]
	}`, time.Now().Add(1*time.Hour).UTC().Format(time.RFC3339), bucket)
	rangeB64 := base64.StdEncoding.EncodeToString([]byte(policyRangeJSON))

	// Too small (< 10 bytes)
	tooSmallFields := map[string]string{
		"key":    "toosmall.txt",
		"policy": rangeB64,
	}
	resp, respBody = executePostForm(t, s.URL+"/"+bucket, tooSmallFields, "toosmall.txt", []byte("short"), "text/plain")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(respBody, "EntityTooSmall") {
		t.Fatalf("expected 400 EntityTooSmall, got %d: %s", resp.StatusCode, respBody)
	}

	// Too large (> 20 bytes)
	tooLargeFields := map[string]string{
		"key":    "toolarge.txt",
		"policy": rangeB64,
	}
	resp, respBody = executePostForm(
		t,
		s.URL+"/"+bucket,
		tooLargeFields,
		"toolarge.txt",
		[]byte("this content is way longer than twenty bytes"),
		"text/plain",
	)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(respBody, "EntityTooLarge") {
		t.Fatalf("expected 400 EntityTooLarge, got %d: %s", resp.StatusCode, respBody)
	}
}

func TestS3PostUploadVerifiesPolicySignature(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "test"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := s3Client(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("signed-posts")}); err != nil {
		t.Fatal(err)
	}

	presign := func(key string) *s3.PresignedPostRequest {
		post, err := s3.NewPresignClient(c).PresignPostObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("signed-posts"), Key: aws.String(key),
		})
		if err != nil {
			t.Fatal(err)
		}

		return post
	}

	post := presign("ok.txt")
	if resp, body := executePostForm(t, post.URL, post.Values, "ok.txt", []byte("ok"), ""); resp.StatusCode != 204 {
		t.Fatalf("signed upload: %d %s", resp.StatusCode, body)
	}

	post = presign("tampered.txt")
	for name := range post.Values {
		if strings.EqualFold(name, "x-amz-signature") {
			post.Values[name] = strings.Repeat("0", 64)
		}
	}

	resp, body := executePostForm(t, post.URL, post.Values, "tampered.txt", []byte("x"), "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("tampered signature: %d %s", resp.StatusCode, body)
	}

	// A field the signed policy does not cover is rejected.
	post = presign("extra.txt")
	post.Values["x-amz-meta-owner"] = "mallory"
	resp, body = executePostForm(t, post.URL, post.Values, "extra.txt", []byte("x"), "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Extra input fields") {
		t.Fatalf("extra field: %d %s", resp.StatusCode, body)
	}

	// An unsigned form is refused.
	resp, body = executePostForm(t, s.URL+"/signed-posts", map[string]string{"key": "anon.txt"}, "anon.txt",
		[]byte("x"), "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned form: %d %s", resp.StatusCode, body)
	}

	// Fields TinyStack does not apply fail instead of being ignored.
	post = presign("acl.txt")
	post.Values["acl"] = "public-read"
	resp, body = executePostForm(t, post.URL, post.Values, "acl.txt", []byte("x"), "")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Extra input fields") {
		t.Fatalf("uncovered acl: %d %s", resp.StatusCode, body)
	}
}
