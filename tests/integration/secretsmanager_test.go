package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func secretsClient(s *httptest.Server, key, region string) *secretsmanager.Client {
	return secretsmanager.New(secretsmanager.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// secretValue returns the string value that stage (or AWSCURRENT) selects.
func secretValue(t *testing.T, c *secretsmanager.Client, id, stage string) string {
	t.Helper()
	in := &secretsmanager.GetSecretValueInput{SecretId: aws.String(id)}
	if stage != "" {
		in.VersionStage = aws.String(stage)
	}

	out, err := c.GetSecretValue(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.SecretString)
}

func TestSecretsLifecycle(t *testing.T) {
	s := server(t)
	c := secretsClient(s, "test", "us-east-1")
	ctx := context.Background()

	created, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("app/db"), SecretString: aws.String(`{"password":"one"}`),
		Description: aws.String("database"), Tags: []smtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	arn := aws.ToString(created.ARN)
	if !regexp.MustCompile(`^arn:aws:secretsmanager:us-east-1:000000000000:secret:app/db-[a-zA-Z0-9]{6}$`).
		MatchString(arn) || created.VersionId == nil {
		t.Fatalf("created %+v", created)
	}

	_, err = c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: aws.String("app/db")})
	wantCode(t, err, "ResourceExistsException")

	// A secret is found by name, full ARN, or ARN without the random suffix.
	for _, id := range []string{"app/db", arn, strings.TrimSuffix(arn, arn[len(arn)-7:])} {
		described, describeErr := c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
		if describeErr != nil || aws.ToString(described.ARN) != arn ||
			aws.ToString(described.Description) != "database" || len(described.Tags) != 1 ||
			!slices.Equal(described.VersionIdsToStages[*created.VersionId], []string{"AWSCURRENT"}) {
			t.Fatalf("%s: got %+v, %v", id, described, describeErr)
		}
	}

	for _, id := range []string{arn[:len(arn)-6] + "XXXXXX", strings.Replace(arn, "000000000000", "111111111111", 1)} {
		_, err = c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
		wantCode(t, err, "ResourceNotFoundException")
	}

	// Secrets belong to an account and a region.
	for _, other := range []*secretsmanager.Client{
		secretsClient(s, "team-b", "us-east-1"), secretsClient(s, "test", "eu-west-1"),
	} {
		_, err = other.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String("app/db")})
		wantCode(t, err, "ResourceNotFoundException")
	}

	if got := secretValue(t, c, "app/db", ""); got != `{"password":"one"}` {
		t.Fatalf("value %s", got)
	}

	binary := []byte{0, 1, 2, 255}
	if _, err = c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("cert"), SecretBinary: binary,
	}); err != nil {
		t.Fatal(err)
	}

	cert, err := c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String("cert")})
	if err != nil || !bytes.Equal(cert.SecretBinary, binary) || cert.SecretString != nil {
		t.Fatalf("got %+v, %v", cert, err)
	}

	for _, in := range []*secretsmanager.CreateSecretInput{
		{Name: aws.String("both"), SecretString: aws.String("a"), SecretBinary: []byte("b")},
		{Name: aws.String("bad name!")},
		{Name: aws.String("kms"), KmsKeyId: aws.String("alias/mine")},
	} {
		_, err = c.CreateSecret(ctx, in)
		wantCode(t, err, "InvalidParameterException")
	}

	// A secret without a value has no current version.
	if _, err = c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: aws.String("empty")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String("empty")})
	wantCode(t, err, "ResourceNotFoundException")

	updated, err := c.UpdateSecret(ctx, &secretsmanager.UpdateSecretInput{
		SecretId: aws.String("empty"), Description: aws.String("filled"), SecretString: aws.String("now"),
	})
	if err != nil || updated.VersionId == nil || secretValue(t, c, "empty", "") != "now" {
		t.Fatalf("got %+v, %v", updated, err)
	}

	policy, err := c.GetResourcePolicy(ctx, &secretsmanager.GetResourcePolicyInput{SecretId: aws.String("app/db")})
	if err != nil || aws.ToString(policy.ARN) != arn || policy.ResourcePolicy != nil {
		t.Fatalf("got %+v, %v", policy, err)
	}

	if _, err = c.TagResource(ctx, &secretsmanager.TagResourceInput{
		SecretId: aws.String("app/db"), Tags: []smtypes.Tag{{Key: aws.String("env"), Value: aws.String("dev")}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(ctx, &secretsmanager.UntagResourceInput{
		SecretId: aws.String("app/db"), TagKeys: []string{"team"},
	}); err != nil {
		t.Fatal(err)
	}

	tagged, err := c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("app/db")})
	if err != nil || len(tagged.Tags) != 1 || aws.ToString(tagged.Tags[0].Key) != "env" {
		t.Fatalf("got %+v, %v", tagged, err)
	}
}

func TestSecretsVersions(t *testing.T) {
	s := server(t)
	c := secretsClient(s, "test", "us-east-1")
	ctx := context.Background()
	token := strings.Repeat("a", 32)
	created, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("rotating"), SecretString: aws.String("v1"), ClientRequestToken: aws.String(token),
	})
	if err != nil || aws.ToString(created.VersionId) != token {
		t.Fatalf("got %+v, %v", created, err)
	}

	v1 := token
	put, err := c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String("rotating"), SecretString: aws.String("v2"),
	})
	if err != nil || !slices.Equal(put.VersionStages, []string{"AWSCURRENT"}) {
		t.Fatalf("got %+v, %v", put, err)
	}

	v2 := aws.ToString(put.VersionId)
	if secretValue(t, c, "rotating", "") != "v2" || secretValue(t, c, "rotating", "AWSPREVIOUS") != "v1" {
		t.Fatal("AWSCURRENT did not move")
	}

	// A request token is idempotent for the same value.
	again, err := c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String("rotating"), SecretString: aws.String("v1"), ClientRequestToken: aws.String(token),
	})
	if err != nil || aws.ToString(again.VersionId) != v1 {
		t.Fatalf("got %+v, %v", again, err)
	}

	_, err = c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String("rotating"), SecretString: aws.String("other"), ClientRequestToken: aws.String(token),
	})
	wantCode(t, err, "ResourceExistsException")

	pending, err := c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String("rotating"), SecretString: aws.String("v3"), VersionStages: []string{"AWSPENDING"},
	})
	if err != nil {
		t.Fatal(err)
	}

	v3 := aws.ToString(pending.VersionId)
	if secretValue(t, c, "rotating", "") != "v2" || secretValue(t, c, "rotating", "AWSPENDING") != "v3" {
		t.Fatal("a pending version became current")
	}

	byID, err := c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String("rotating"), VersionId: aws.String(v1),
	})
	if err != nil || aws.ToString(byID.SecretString) != "v1" ||
		!slices.Equal(byID.VersionStages, []string{"AWSPREVIOUS"}) {
		t.Fatalf("got %+v, %v", byID, err)
	}

	_, err = c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String("rotating"), VersionId: aws.String(v1), VersionStage: aws.String("AWSCURRENT"),
	})
	wantCode(t, err, "ResourceNotFoundException")

	// Promoting the pending version needs the current holder named.
	_, err = c.UpdateSecretVersionStage(ctx, &secretsmanager.UpdateSecretVersionStageInput{
		SecretId: aws.String("rotating"), VersionStage: aws.String("AWSCURRENT"), MoveToVersionId: aws.String(v3),
	})
	wantCode(t, err, "InvalidParameterException")

	if _, err = c.UpdateSecretVersionStage(ctx, &secretsmanager.UpdateSecretVersionStageInput{
		SecretId: aws.String("rotating"), VersionStage: aws.String("AWSCURRENT"),
		MoveToVersionId: aws.String(v3), RemoveFromVersionId: aws.String(v2),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateSecretVersionStage(ctx, &secretsmanager.UpdateSecretVersionStageInput{
		SecretId: aws.String("rotating"), VersionStage: aws.String("AWSCURRENT"), RemoveFromVersionId: aws.String(v3),
	})
	wantCode(t, err, "InvalidParameterException")

	if _, err = c.UpdateSecretVersionStage(ctx, &secretsmanager.UpdateSecretVersionStageInput{
		SecretId: aws.String("rotating"), VersionStage: aws.String("AWSPENDING"), RemoveFromVersionId: aws.String(v3),
	}); err != nil {
		t.Fatal(err)
	}

	described, err := c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("rotating")})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]string{v2: {"AWSPREVIOUS"}, v3: {"AWSCURRENT"}}
	if !maps.EqualFunc(described.VersionIdsToStages, want, slices.Equal) {
		t.Fatalf("stages %v", described.VersionIdsToStages)
	}

	listed, err := c.ListSecretVersionIds(ctx, &secretsmanager.ListSecretVersionIdsInput{
		SecretId: aws.String("rotating"),
	})
	if err != nil || len(listed.Versions) != 2 {
		t.Fatalf("got %+v, %v", listed, err)
	}

	var ids []string
	in := &secretsmanager.ListSecretVersionIdsInput{
		SecretId: aws.String("rotating"), IncludeDeprecated: aws.Bool(true), MaxResults: aws.Int32(2),
	}
	for {
		out, listErr := c.ListSecretVersionIds(ctx, in)
		if listErr != nil {
			t.Fatal(listErr)
		}

		for _, v := range out.Versions {
			ids = append(ids, aws.ToString(v.VersionId))
		}

		if out.NextToken == nil {
			break
		}

		in.NextToken = out.NextToken
	}

	// Versions created in the same millisecond are ordered by ID, so compare the
	// pages as a set: each version must appear exactly once.
	slices.Sort(ids)
	versions := []string{v1, v2, v3}
	slices.Sort(versions)
	if !slices.Equal(ids, versions) {
		t.Fatalf("versions %v", ids)
	}
}

func TestSecretsList(t *testing.T) {
	s := server(t)
	c := secretsClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, name := range []string{"prod/db", "prod/api", "dev/db"} {
		if _, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
			Name: aws.String(name), Tags: []smtypes.Tag{{Key: aws.String("env"), Value: aws.String(name[:4])}},
		}); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Millisecond)
	}

	list := func(in *secretsmanager.ListSecretsInput) []string {
		t.Helper()
		var out []string
		for {
			got, err := c.ListSecrets(ctx, in)
			if err != nil {
				t.Fatal(err)
			}

			for _, e := range got.SecretList {
				out = append(out, aws.ToString(e.Name))
			}

			if got.NextToken == nil {
				return out
			}

			in.NextToken = got.NextToken
		}
	}

	if got := list(&secretsmanager.ListSecretsInput{MaxResults: aws.Int32(1)}); !slices.Equal(got,
		[]string{"prod/db", "prod/api", "dev/db"}) {
		t.Fatalf("ascending %v", got)
	}

	if got := list(
		&secretsmanager.ListSecretsInput{SortOrder: smtypes.SortOrderTypeDesc, MaxResults: aws.Int32(2)},
	); !slices.Equal(
		got,
		[]string{"dev/db", "prod/api", "prod/db"},
	) {
		t.Fatalf("descending %v", got)
	}

	for _, tc := range []struct {
		filters []smtypes.Filter
		want    []string
	}{
		{
			[]smtypes.Filter{{Key: smtypes.FilterNameStringTypeName, Values: []string{"prod/"}}},
			[]string{"prod/db", "prod/api"},
		},
		{[]smtypes.Filter{{Key: smtypes.FilterNameStringTypeName, Values: []string{"!prod/"}}}, []string{"dev/db"}},
		{[]smtypes.Filter{{Key: smtypes.FilterNameStringTypeTagValue, Values: []string{"dev/"}}}, []string{"dev/db"}},
		{
			[]smtypes.Filter{
				{Key: smtypes.FilterNameStringTypeAll, Values: []string{"prod"}},
				{Key: smtypes.FilterNameStringTypeName, Values: []string{"prod/a", "dev"}},
			},
			[]string{"prod/api"},
		},
	} {
		if got := list(&secretsmanager.ListSecretsInput{Filters: tc.filters}); !slices.Equal(got, tc.want) {
			t.Fatalf("%v: got %v", tc.filters, got)
		}
	}

	_, err := c.ListSecrets(ctx, &secretsmanager.ListSecretsInput{NextToken: aws.String("!")})
	wantCode(t, err, "InvalidNextTokenException")
}

func TestSecretsDeletion(t *testing.T) {
	store := &memory.Store{}
	s, stop := serverStore(t, testConfig(), &fakeRuntime{}, store)
	t.Cleanup(stop)
	c := secretsClient(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("old"), SecretString: aws.String("v"),
	}); err != nil {
		t.Fatal(err)
	}

	for _, in := range []*secretsmanager.DeleteSecretInput{
		{SecretId: aws.String("old"), RecoveryWindowInDays: aws.Int64(3)},
		{SecretId: aws.String("old"), RecoveryWindowInDays: aws.Int64(7), ForceDeleteWithoutRecovery: aws.Bool(true)},
	} {
		_, err := c.DeleteSecret(ctx, in)
		wantCode(t, err, "InvalidParameterException")
	}

	start := time.Now()
	deleted, err := c.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String("old"), RecoveryWindowInDays: aws.Int64(7),
	})
	if err != nil || aws.ToTime(deleted.DeletionDate).Before(start.Add(7*24*time.Hour-time.Minute)) {
		t.Fatalf("got %+v, %v", deleted, err)
	}

	// A scheduled deletion blocks retrieval, changes, and the name.
	_, err = c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String("old")})
	wantCode(t, err, "InvalidRequestException")

	_, err = c.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String("old"), SecretString: aws.String("x"),
	})
	wantCode(t, err, "InvalidRequestException")

	_, err = c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: aws.String("old")})
	wantCode(t, err, "InvalidRequestException")

	described, err := c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("old")})
	if err != nil || described.DeletedDate == nil {
		t.Fatalf("got %+v, %v", described, err)
	}

	listed, err := c.ListSecrets(ctx, &secretsmanager.ListSecretsInput{})
	if err != nil || len(listed.SecretList) != 0 {
		t.Fatalf("got %+v, %v", listed, err)
	}

	listed, err = c.ListSecrets(ctx, &secretsmanager.ListSecretsInput{IncludePlannedDeletion: aws.Bool(true)})
	if err != nil || len(listed.SecretList) != 1 {
		t.Fatalf("got %+v, %v", listed, err)
	}

	if _, err = c.RestoreSecret(ctx, &secretsmanager.RestoreSecretInput{SecretId: aws.String("old")}); err != nil {
		t.Fatal(err)
	}

	if secretValue(t, c, "old", "") != "v" {
		t.Fatal("restored secret lost its value")
	}

	// Once the recovery window ends, the secret is gone and its name is free.
	if _, err = c.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: aws.String("old")}); err != nil {
		t.Fatal(err)
	}

	key := "000000000000/us-east-1/secretsmanager/secret/old"
	err = store.Update(ctx, func(tx storage.WriteTx) error {
		data, _ := tx.Get(key)
		var record map[string]any
		if unmarshalErr := json.Unmarshal(data, &record); unmarshalErr != nil {
			return unmarshalErr
		}

		record["DeletionAt"] = time.Now().Add(-time.Second).UnixMilli()
		data, marshalErr := json.Marshal(record)
		tx.Put(key, data)
		return marshalErr
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("old")})
	wantCode(t, err, "ResourceNotFoundException")

	if _, err = c.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("old"), SecretString: aws.String("new"),
	}); err != nil {
		t.Fatal(err)
	}

	if secretValue(t, c, "old", "") != "new" {
		t.Fatal("recreated secret has the old value")
	}

	// Without recovery, a secret is deleted at once.
	if _, err = c.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String("old"), ForceDeleteWithoutRecovery: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String("old")})
	wantCode(t, err, "ResourceNotFoundException")

	var left []string
	err = store.View(ctx, func(tx storage.ReadTx) error {
		left = tx.Keys("000000000000/us-east-1/secretsmanager/")
		return nil
	})
	if err != nil || len(left) != 0 {
		t.Fatalf("left %v, %v", left, err)
	}
}

func TestSecretsPersist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	if _, err := secretsClient(s, "test", "us-east-1").CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String("kept"), SecretString: aws.String("value"),
	}); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	if got := secretValue(t, secretsClient(s, "test", "us-east-1"), "kept", ""); got != "value" {
		t.Fatalf("value %s", got)
	}
}
