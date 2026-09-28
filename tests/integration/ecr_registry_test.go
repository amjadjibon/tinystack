package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// registryCall is one Docker Registry API request with Basic credentials.
type registryCall struct {
	t        *testing.T
	base     string
	user     string
	password string
}

func (rc *registryCall) do(method, path, contentType string, body []byte, header ...string) *http.Response {
	rc.t.Helper()
	target := path
	if !strings.HasPrefix(path, "http") {
		target = rc.base + path
	}

	req, err := http.NewRequestWithContext(context.Background(), method, target, bytes.NewReader(body))
	if err != nil {
		rc.t.Fatal(err)
	}

	if rc.user != "" {
		req.SetBasicAuth(rc.user, rc.password)
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		rc.t.Fatal(err)
	}

	rc.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// registryCode returns a registry error response's first error code.
func registryCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Errors []struct{ Code string } `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Errors) == 0 {
		t.Fatalf("error body: %v %+v", err, body)
	}

	return body.Errors[0].Code
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s = %d %s, want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body, want)
	}
}

// login returns the registry credentials GetAuthorizationToken issues.
func login(ctx context.Context, t *testing.T, c *ecr.Client) (user, password, endpoint string) {
	t.Helper()
	out, err := c.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		t.Fatal(err)
	}

	data := out.AuthorizationData[0]
	decoded, err := base64.StdEncoding.DecodeString(aws.ToString(data.AuthorizationToken))
	if err != nil {
		t.Fatal(err)
	}

	user, password, _ = strings.Cut(string(decoded), ":")
	if data.ExpiresAt == nil {
		t.Fatal("no expiry")
	}

	return user, password, aws.ToString(data.ProxyEndpoint)
}

func TestECRRegistry(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ecrClient(s, "test", "us-east-1")
	createRepository(ctx, t, c, "team/app")
	createRepository(ctx, t, c, "base")

	anonymous := &registryCall{t: t, base: s.URL}
	resp := anonymous.do(http.MethodGet, "/v2/", "", nil)
	expectStatus(t, resp, http.StatusUnauthorized)
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Fatalf("challenge = %q", resp.Header.Get("WWW-Authenticate"))
	}

	user, password, endpoint := login(ctx, t, c)
	if user != "AWS" || endpoint != s.URL {
		t.Fatalf("login = %s %s", user, endpoint)
	}

	wrong := &registryCall{t: t, base: s.URL, user: "AWS", password: "nope"}
	expectStatus(t, wrong.do(http.MethodGet, "/v2/", "", nil), http.StatusUnauthorized)
	rc := &registryCall{t: t, base: s.URL, user: user, password: password}
	resp = rc.do(http.MethodGet, "/v2/", "", nil)
	expectStatus(t, resp, http.StatusOK)
	if resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatal("no API version header")
	}

	// A monolithic upload of the config.
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := imageDigest(config)
	resp = rc.do(
		http.MethodPost,
		"/v2/team/app/blobs/uploads/?digest="+configDigest,
		"application/octet-stream",
		config,
	)
	expectStatus(t, resp, http.StatusCreated)
	if resp.Header.Get("Docker-Content-Digest") != configDigest {
		t.Fatalf("config digest = %q", resp.Header.Get("Docker-Content-Digest"))
	}

	// A chunked upload of the layer, as docker push sends it.
	layer := bytes.Repeat([]byte("layer bytes "), 100)
	layerDigest := imageDigest(layer)
	expectStatus(t, rc.do(http.MethodHead, "/v2/team/app/blobs/"+layerDigest, "", nil), http.StatusNotFound)
	resp = rc.do(http.MethodPost, "/v2/team/app/blobs/uploads/", "", nil)
	expectStatus(t, resp, http.StatusAccepted)
	location := resp.Header.Get("Location")
	resp = rc.do(http.MethodPatch, location, "application/octet-stream", layer[:500], "Content-Range", "0-499")
	expectStatus(t, resp, http.StatusAccepted)
	if resp.Header.Get("Range") != "0-499" {
		t.Fatalf("range = %q", resp.Header.Get("Range"))
	}

	resp = rc.do(http.MethodPatch, location, "application/octet-stream", layer[:10], "Content-Range", "100-109")
	expectStatus(t, resp, http.StatusRequestedRangeNotSatisfiable)
	resp = rc.do(http.MethodPatch, location, "application/octet-stream", layer[500:])
	expectStatus(t, resp, http.StatusAccepted)
	resp = rc.do(http.MethodPut, location+"?digest="+imageDigest([]byte("other")), "", nil)
	expectStatus(t, resp, http.StatusBadRequest)
	if registryCode(t, resp) != "DIGEST_INVALID" {
		t.Fatal("wrong digest accepted")
	}

	resp = rc.do(http.MethodPost, "/v2/team/app/blobs/uploads/", "", nil)
	location = resp.Header.Get("Location")
	expectStatus(t, rc.do(http.MethodPatch, location, "application/octet-stream", layer), http.StatusAccepted)
	expectStatus(t, rc.do(http.MethodPut, location+"?digest="+layerDigest, "", nil), http.StatusCreated)
	resp = rc.do(http.MethodHead, "/v2/team/app/blobs/"+layerDigest, "", nil)
	expectStatus(t, resp, http.StatusOK)
	if resp.ContentLength != int64(len(layer)) {
		t.Fatalf("blob length = %d", resp.ContentLength)
	}

	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json", "digest": configDigest, "size": len(config),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip", "digest": layerDigest, "size": len(layer),
		}},
	})
	manifestType := "application/vnd.docker.distribution.manifest.v2+json"
	resp = rc.do(http.MethodPut, "/v2/team/app/manifests/v1", manifestType, manifest)
	expectStatus(t, resp, http.StatusCreated)
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest != imageDigest(manifest) {
		t.Fatalf("manifest digest = %q", digest)
	}

	// Pushing it again is fine, as docker push repeats.
	expectStatus(t, rc.do(http.MethodPut, "/v2/team/app/manifests/v1", manifestType, manifest), http.StatusCreated)

	resp = rc.do(http.MethodGet, "/v2/team/app/manifests/v1", "", nil, "Accept", manifestType)
	expectStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, manifest) || resp.Header.Get("Content-Type") != manifestType ||
		resp.Header.Get("Docker-Content-Digest") != digest {
		t.Fatalf("manifest = %s %q", body, resp.Header.Get("Content-Type"))
	}

	expectStatus(t, rc.do(http.MethodHead, "/v2/team/app/manifests/"+digest, "", nil), http.StatusOK)
	resp = rc.do(http.MethodGet, "/v2/team/app/blobs/"+layerDigest, "", nil)
	expectStatus(t, resp, http.StatusOK)
	if body, _ = io.ReadAll(resp.Body); !bytes.Equal(body, layer) {
		t.Fatal("layer differs")
	}

	missing, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": manifestType,
		"config": map[string]any{"digest": configDigest, "size": 1},
		"layers": []map[string]any{{"digest": imageDigest([]byte("absent")), "size": 1}},
	})
	resp = rc.do(http.MethodPut, "/v2/team/app/manifests/broken", manifestType, missing)
	expectStatus(t, resp, http.StatusBadRequest)
	if registryCode(t, resp) != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatal("manifest with a missing layer accepted")
	}

	resp = rc.do(http.MethodGet, "/v2/team/app/tags/list", "", nil)
	expectStatus(t, resp, http.StatusOK)
	var tags struct {
		Name string
		Tags []string
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil || tags.Name != "team/app" ||
		len(tags.Tags) != 1 || tags.Tags[0] != "v1" {
		t.Fatalf("tags = %+v, %v", tags, err)
	}

	// The ECR API sees what the registry stored, including the pull.
	details, err := c.DescribeImages(ctx, &ecr.DescribeImagesInput{RepositoryName: aws.String("team/app")})
	if err != nil {
		t.Fatal(err)
	}

	if len(details.ImageDetails) != 1 || aws.ToString(details.ImageDetails[0].ImageDigest) != digest ||
		aws.ToInt64(details.ImageDetails[0].ImageSizeInBytes) != int64(len(layer)) ||
		details.ImageDetails[0].LastRecordedPullTime == nil {
		t.Fatalf("details = %+v", details.ImageDetails)
	}

	// Mounting copies a blob from another repository.
	resp = rc.do(http.MethodPost, "/v2/base/blobs/uploads/?mount="+layerDigest+"&from=team/app", "", nil)
	expectStatus(t, resp, http.StatusCreated)
	expectStatus(t, rc.do(http.MethodHead, "/v2/base/blobs/"+layerDigest, "", nil), http.StatusOK)
	resp = rc.do(http.MethodPost, "/v2/base/blobs/uploads/?mount="+configDigest+"&from=missing", "", nil)
	expectStatus(t, resp, http.StatusAccepted)

	resp = rc.do(http.MethodGet, "/v2/nothing/tags/list", "", nil)
	expectStatus(t, resp, http.StatusNotFound)
	if registryCode(t, resp) != "NAME_UNKNOWN" {
		t.Fatal("unknown repository listed")
	}

	resp = rc.do(http.MethodGet, "/v2/team/app/manifests/v9", "", nil)
	expectStatus(t, resp, http.StatusNotFound)
	if registryCode(t, resp) != "MANIFEST_UNKNOWN" {
		t.Fatal("unknown tag served")
	}

	// A password works only in its own region.
	west := ecrClient(s, "test", "us-west-2")
	createRepository(ctx, t, west, "team/app")
	_, westPassword, _ := login(ctx, t, west)
	westCall := &registryCall{t: t, base: s.URL, user: "AWS", password: westPassword}
	resp = westCall.do(http.MethodGet, "/v2/team/app/tags/list", "", nil)
	expectStatus(t, resp, http.StatusOK)
	if err = json.NewDecoder(resp.Body).Decode(&tags); err != nil || len(tags.Tags) != 0 {
		t.Fatalf("us-west-2 tags = %+v, %v", tags, err)
	}

	expectStatus(t, rc.do(http.MethodDelete, "/v2/team/app/manifests/"+digest, "", nil), http.StatusAccepted)
	listed, err := c.ListImages(ctx, &ecr.ListImagesInput{
		RepositoryName: aws.String("team/app"), Filter: &ecrtypes.ListImagesFilter{TagStatus: ecrtypes.TagStatusAny},
	})
	if err != nil || len(listed.ImageIds) != 0 {
		t.Fatalf("images after delete = %+v, %v", listed, err)
	}
}
