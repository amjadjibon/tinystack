package metadata_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/metadata"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

type instanceData struct {
	LaunchTime      time.Time
	Profile         *profileData
	MetadataOptions *metaOptionsData
	ID              string
	Type            string
	ImageID         string
	Zone            string
	PrivateIP       string
	State           string
	UserData        string
}

type profileData struct {
	ARN       string
	ProfileID string
}

type metaOptionsData struct {
	HttpTokens string
}

func setupMetadataService(t *testing.T) (*metadata.Service, storage.Store) {
	t.Helper()

	store := memory.New(nil, nil)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}

	// Seed instance
	err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		inst := instanceData{
			ID:         "i-0123456789abcdef0",
			Type:       "t3.micro",
			ImageID:    "ami-12345678",
			Zone:       "us-east-1a",
			PrivateIP:  "10.0.0.42",
			State:      "running",
			UserData:   base64.StdEncoding.EncodeToString([]byte("echo hello")),
			LaunchTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Profile: &profileData{
				ARN:       "arn:aws:iam::123456789012:instance-profile/test-profile",
				ProfileID: "AIPATEST123456",
			},
			MetadataOptions: &metaOptionsData{
				HttpTokens: "optional",
			},
		}

		if err := storage.Write(tx, "aws/ec2/instances/"+inst.ID, scope); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key("ec2", "instance", inst.ID), inst)
	})
	if err != nil {
		t.Fatalf("failed to seed instance: %v", err)
	}

	assumeRoleFn := func(
		ctx context.Context,
		principal, roleARN, sessionName string,
		seconds int64,
	) (accessKey, secret, token string, expires time.Time, err error) {
		return "ASIA_TEST_KEY", "TEST_SECRET", "TEST_TOKEN", time.Now().Add(time.Hour), nil
	}

	instanceProfilesFn := func(ctx context.Context, account, ref string) (iam.InstanceProfile, bool, error) {
		return iam.InstanceProfile{
			ARN:     "arn:aws:iam::123456789012:instance-profile/test-profile",
			ID:      "AIPATEST123456",
			RoleARN: "arn:aws:iam::123456789012:role/test-role",
		}, true, nil
	}

	svc := metadata.New(
		ec2.New(store), ecs.New(store),
		metadata.WithAssumeRole(assumeRoleFn),
		metadata.WithInstanceProfiles(instanceProfilesFn),
	)

	return svc, store
}

func TestIMDSv2TokenIssuance(t *testing.T) {
	svc, _ := setupMetadataService(t)
	handler := svc.IMDSHandler()

	// 1. Missing TTL header
	req := httptest.NewRequest(http.MethodPut, "/latest/api/token", http.NoBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing TTL header, got %d", rr.Code)
	}

	// 2. TTL too small (0)
	req = httptest.NewRequest(http.MethodPut, "/latest/api/token", http.NoBody)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "0")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for TTL=0, got %d", rr.Code)
	}

	// 3. TTL too large (21601)
	req = httptest.NewRequest(http.MethodPut, "/latest/api/token", http.NoBody)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21601")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for TTL=21601, got %d", rr.Code)
	}

	// 4. Valid TTL
	req = httptest.NewRequest(http.MethodPut, "/latest/api/token", http.NoBody)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	token := rr.Body.String()
	if token == "" {
		t.Fatalf("expected non-empty token")
	}

	if rr.Header().Get("X-aws-ec2-metadata-token-ttl-seconds") != "300" {
		t.Fatalf("expected TTL header 300, got %s", rr.Header().Get("X-aws-ec2-metadata-token-ttl-seconds"))
	}

	// 5. Use token to get metadata
	getReq := httptest.NewRequest(http.MethodGet, "/latest/meta-data/instance-id", http.NoBody)
	getReq.Header.Set("X-aws-ec2-metadata-token", token)
	getRR := httptest.NewRecorder()
	handler.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d: %s", getRR.Code, getRR.Body.String())
	}

	if getRR.Body.String() != "i-0123456789abcdef0" {
		t.Fatalf("got %s, want i-0123456789abcdef0", getRR.Body.String())
	}

	// 6. Bad token -> 401
	badReq := httptest.NewRequest(http.MethodGet, "/latest/meta-data/instance-id", http.NoBody)
	badReq.Header.Set("X-aws-ec2-metadata-token", "invalid-token")
	badRR := httptest.NewRecorder()
	handler.ServeHTTP(badRR, badReq)
	if badRR.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad token, got %d", badRR.Code)
	}
}

func TestIMDSv1HttpTokensEnforcement(t *testing.T) {
	svc, store := setupMetadataService(t)
	handler := svc.IMDSHandler()

	// 1. With HttpTokens=optional: IMDSv1 GET allowed without token
	req := httptest.NewRequest(http.MethodGet, "/latest/meta-data/instance-id", http.NoBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for IMDSv1 when optional, got %d: %s", rr.Code, rr.Body.String())
	}

	// 2. Change instance to HttpTokens=required
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	_ = store.Update(context.Background(), func(tx storage.WriteTx) error {
		inst, ok, _ := storage.Read[instanceData](tx, scope.Key("ec2", "instance", "i-0123456789abcdef0"))
		if ok {
			inst.MetadataOptions.HttpTokens = "required"
			return storage.Write(tx, scope.Key("ec2", "instance", "i-0123456789abcdef0"), inst)
		}

		return nil
	})

	// 3. IMDSv1 GET now rejected with 401
	req = httptest.NewRequest(http.MethodGet, "/latest/meta-data/instance-id", http.NoBody)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for IMDSv1 when required, got %d", rr.Code)
	}
}

func TestIMDSPaths(t *testing.T) {
	svc, _ := setupMetadataService(t)
	handler := svc.IMDSHandler()

	paths := map[string]string{
		"/latest/meta-data/instance-type":            "t3.micro",
		"/latest/meta-data/ami-id":                   "ami-12345678",
		"/latest/meta-data/local-ipv4":               "10.0.0.42",
		"/latest/meta-data/placement/region":         "us-east-1",
		"/latest/meta-data/placement/":               "availability-zone\nregion\n",
		"/latest/meta-data/iam/security-credentials": "test-role",
		"/latest/user-data":                          "echo hello",
	}

	for p, want := range paths {
		req := httptest.NewRequest(http.MethodGet, p, http.NoBody)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("path %s returned status %d: %s", p, rr.Code, rr.Body.String())
			continue
		}

		if rr.Body.String() != want {
			t.Errorf("path %s: got %q, want %q", p, rr.Body.String(), want)
		}
	}

	// Dynamic instance identity document
	req := httptest.NewRequest(http.MethodGet, "/latest/dynamic/instance-identity/document", http.NoBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("instance-identity doc returned %d", rr.Code)
	}

	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("failed to decode identity document: %v", err)
	}

	if doc["instanceId"] != "i-0123456789abcdef0" {
		t.Errorf("got instanceId %v, want i-0123456789abcdef0", doc["instanceId"])
	}

	// IAM security credentials for role
	req = httptest.NewRequest(http.MethodGet, "/latest/meta-data/iam/security-credentials/test-role", http.NoBody)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("iam security credentials returned %d: %s", rr.Code, rr.Body.String())
	}

	var creds map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
		t.Fatalf("failed to parse credentials JSON: %v", err)
	}

	if creds["AccessKeyId"] != "ASIA_TEST_KEY" {
		t.Errorf("got AccessKeyId %v, want ASIA_TEST_KEY", creds["AccessKeyId"])
	}

	// Mismatched role -> 404
	req = httptest.NewRequest(http.MethodGet, "/latest/meta-data/iam/security-credentials/other-role", http.NoBody)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for wrong role, got %d", rr.Code)
	}
}

func TestECSMetadataAndCredentials(t *testing.T) {
	svc, store := setupMetadataService(t)
	token := "ecs-test-token-12345"

	memLimit := int64(512)
	cpuLimit := int64(256)
	rec := ecs.ContainerMetadata{
		Cluster:       "my-cluster",
		TaskID:        "task-123",
		TaskARN:       "arn:aws:ecs:us-east-1:123456789012:task/my-cluster/task-123",
		Family:        "web",
		Revision:      "1",
		ContainerName: "app",
		Image:         "nginx:alpine",
		TaskRoleARN:   "arn:aws:iam::123456789012:role/task-role",
		CreatedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		StartedAt:     time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
		CPU:           new(cpuLimit),
		Memory:        new(memLimit),
	}

	if err := store.Update(context.Background(), func(tx storage.WriteTx) error {
		key := "123456789012/us-east-1/ecs/task/my-cluster/task-123"
		if err := storage.Write(tx, "aws/ecs/metadata/"+token, key); err != nil {
			return err
		}

		return storage.Write(tx, key, struct {
			Metadata map[string]ecs.ContainerMetadata
			Last     string
		}{Last: "RUNNING", Metadata: map[string]ecs.ContainerMetadata{token: rec}})
	}); err != nil {
		t.Fatal(err)
	}

	handler := svc.ECSHandler()

	// 1. Container metadata
	req := httptest.NewRequest(http.MethodGet, "/v4/"+token, http.NoBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("v4/token returned %d: %s", rr.Code, rr.Body.String())
	}

	var containerMeta map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &containerMeta); err != nil {
		t.Fatalf("failed to decode container meta: %v", err)
	}

	if containerMeta["Name"] != "app" {
		t.Errorf("got container Name %v, want app", containerMeta["Name"])
	}

	// 2. Task metadata
	req = httptest.NewRequest(http.MethodGet, "/v4/"+token+"/task", http.NoBody)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("v4/token/task returned %d: %s", rr.Code, rr.Body.String())
	}

	var taskMeta map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &taskMeta); err != nil {
		t.Fatalf("failed to decode task meta: %v", err)
	}

	if taskMeta["Cluster"] != "my-cluster" {
		t.Errorf("got cluster %v, want my-cluster", taskMeta["Cluster"])
	}

	// 3. Stats
	req = httptest.NewRequest(http.MethodGet, "/v4/"+token+"/stats", http.NoBody)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("v4/token/stats returned %d", rr.Code)
	}

	// 4. Task Stats
	req = httptest.NewRequest(http.MethodGet, "/v4/"+token+"/task/stats", http.NoBody)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("v4/token/task/stats returned %d", rr.Code)
	}

	// 5. Container credentials with Authorization header
	req = httptest.NewRequest(http.MethodGet, "/credentials", http.NoBody)
	req.Header.Set("Authorization", token)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("credentials returned %d: %s", rr.Code, rr.Body.String())
	}

	var creds map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &creds); err != nil {
		t.Fatalf("failed to decode credentials JSON: %v", err)
	}

	if creds["AccessKeyId"] != "ASIA_TEST_KEY" {
		t.Errorf("got AccessKeyId %v, want ASIA_TEST_KEY", creds["AccessKeyId"])
	}

	// 6. Invalid token -> 401
	badReq := httptest.NewRequest(http.MethodGet, "/credentials", http.NoBody)
	badReq.Header.Set("Authorization", "invalid-token")
	badRR := httptest.NewRecorder()
	handler.ServeHTTP(badRR, badReq)
	if badRR.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for bad token, got %d", badRR.Code)
	}
}

// failingStore simulates a storage backend failure without hiding it as a missing resource.
type failingStore struct{ storage.Store }

func (f failingStore) View(context.Context, func(storage.ReadTx) error) error {
	return errors.New("storage unavailable")
}

func TestMetadataStorageErrors(t *testing.T) {
	store := failingStore{Store: &memory.Store{}}
	s := metadata.New(ec2.New(store), ecs.New(store))
	for _, tc := range []struct {
		handler http.Handler
		path    string
	}{
		{s.IMDSHandler(), "/latest/meta-data/instance-id"},
		{s.ECSHandler(), "/_tinystack/ecs/v4/token"},
		{s.ECSHandler(), "/_tinystack/ecs/credentials/token"},
	} {
		rr := httptest.NewRecorder()
		tc.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, http.NoBody))
		if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "storage unavailable") {
			t.Fatalf("%s: %d %s", tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestIMDSProfileLookupError(t *testing.T) {
	_, store := setupMetadataService(t)
	s := metadata.New(
		ec2.New(store),
		ecs.New(store),
		metadata.WithInstanceProfiles(func(context.Context, string, string) (iam.InstanceProfile, bool, error) {
			return iam.InstanceProfile{}, false, errors.New("profile backend unavailable")
		}),
	)
	rr := httptest.NewRecorder()
	s.IMDSHandler().
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/latest/meta-data/iam/security-credentials/role", http.NoBody))
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "profile backend unavailable") {
		t.Fatalf("profile error: %d %s", rr.Code, rr.Body.String())
	}
}

func TestIMDSSelectionPrecedence(t *testing.T) {
	_, store := setupMetadataService(t)
	s := metadata.New(ec2.New(store), ecs.New(store), metadata.WithDefaultInstanceID("i-missing"))
	id := "i-0123456789abcdef0"
	for _, tc := range []struct {
		path   string
		header string
		status int
	}{
		{"/latest/meta-data/instance-id", "", http.StatusNotFound},
		{"/latest/meta-data/instance-id", id, http.StatusOK},
		{"/_tinystack/imds/" + id + "/latest/meta-data/instance-id", "i-wrong", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, http.NoBody)
		req.Header.Set("X-TinyStack-Instance-Id", tc.header)
		rr := httptest.NewRecorder()
		s.IMDSHandler().ServeHTTP(rr, req)
		if rr.Code != tc.status || (tc.status == http.StatusOK && rr.Body.String() != id) {
			t.Fatalf("selection: %d %s", rr.Code, rr.Body.String())
		}
	}
}
