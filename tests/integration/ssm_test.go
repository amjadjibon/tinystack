package integration_test

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func ssmClient(s *httptest.Server, key, region string) *ssm.Client {
	return ssm.New(ssm.Options{
		Region:       region,
		BaseEndpoint: aws.String(s.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key, SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	})
}

// put stores a String parameter, overwriting it, and returns its version.
func put(t *testing.T, c *ssm.Client, name, value string) int64 {
	t.Helper()
	out, err := c.PutParameter(context.Background(), &ssm.PutParameterInput{
		Name: aws.String(name), Value: aws.String(value), Type: ssmtypes.ParameterTypeString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	return out.Version
}

func names(params []ssmtypes.Parameter) []string {
	out := make([]string, 0, len(params))
	for _, p := range params {
		out = append(out, aws.ToString(p.Name))
	}

	return out
}

func TestSSMParameters(t *testing.T) {
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	ctx := context.Background()

	created, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/db/url"), Value: aws.String("postgres://db"), Type: ssmtypes.ParameterTypeString,
		Description: aws.String("database"), Tags: []ssmtypes.Tag{{Key: aws.String("team"), Value: aws.String("core")}},
	})
	if err != nil || created.Version != 1 || created.Tier != ssmtypes.ParameterTierStandard {
		t.Fatalf("got %+v, %v", created, err)
	}

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/db/url"), Value: aws.String("x"), Type: ssmtypes.ParameterTypeString,
	})
	wantCode(t, err, "ParameterAlreadyExists")

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/db/url"), Value: aws.String("x"), Overwrite: aws.Bool(true),
		Tags: []ssmtypes.Tag{{Key: aws.String("a"), Value: aws.String("b")}},
	})
	wantCode(t, err, "ValidationException")

	// An overwrite keeps the settings it leaves out.
	updated, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/db/url"), Value: aws.String("postgres://db2"), Overwrite: aws.Bool(true),
	})
	if err != nil || updated.Version != 2 {
		t.Fatalf("got %+v, %v", updated, err)
	}

	got, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/app/db/url")})
	if err != nil {
		t.Fatal(err)
	}

	p := got.Parameter
	if aws.ToString(p.Value) != "postgres://db2" || p.Version != 2 || p.Type != ssmtypes.ParameterTypeString ||
		aws.ToString(p.ARN) != "arn:aws:ssm:us-east-1:000000000000:parameter/app/db/url" ||
		p.LastModifiedDate == nil || aws.ToString(p.DataType) != "text" {
		t.Fatalf("got %+v", p)
	}

	first, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/app/db/url:1")})
	if err != nil || aws.ToString(first.Parameter.Value) != "postgres://db" ||
		aws.ToString(first.Parameter.Selector) != ":1" || aws.ToString(first.Parameter.Name) != "/app/db/url" {
		t.Fatalf("got %+v, %v", first, err)
	}

	for _, name := range []string{"/app/db/url:3", "/app/db/url:missing"} {
		_, err = c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
		wantCode(t, err, "ParameterVersionNotFound")
	}

	_, err = c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/app/none")})
	wantCode(t, err, "ParameterNotFound")

	described, err := c.DescribeParameters(ctx, &ssm.DescribeParametersInput{})
	if err != nil || len(described.Parameters) != 1 || aws.ToString(described.Parameters[0].Description) != "database" {
		t.Fatalf("got %+v, %v", described, err)
	}

	if _, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("hosts"), Value: aws.String("a,b"), Type: ssmtypes.ParameterTypeStringList,
	}); err != nil {
		t.Fatal(err)
	}

	batch, err := c.GetParameters(ctx, &ssm.GetParametersInput{
		Names: []string{"hosts", "/app/db/url:1", "/app/none", "hosts:9", "bad name"},
	})
	if err != nil || !slices.Equal(names(batch.Parameters), []string{"hosts", "/app/db/url"}) ||
		batch.Parameters[0].Type != ssmtypes.ParameterTypeStringList ||
		!slices.Equal(batch.InvalidParameters, []string{"/app/none", "hosts:9", "bad name"}) {
		t.Fatalf("got %+v, %v", batch, err)
	}

	for _, name := range []string{"app/x", "/app/x/", "/app//x", "/aws/x", "ssm-x", "bad name"} {
		_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
			Name: aws.String(name), Value: aws.String("v"), Type: ssmtypes.ParameterTypeString,
		})
		wantCode(t, err, "ValidationException")
	}

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/" + strings.Repeat("a/", 16) + "x"), Value: aws.String("v"),
		Type: ssmtypes.ParameterTypeString,
	})
	wantCode(t, err, "HierarchyLevelLimitExceededException")

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String("untyped"), Value: aws.String("v")})
	wantCode(t, err, "ValidationException")

	// Parameters belong to an account and a region.
	for _, other := range []*ssm.Client{ssmClient(s, "team-b", "us-east-1"), ssmClient(s, "test", "eu-west-1")} {
		_, err = other.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("hosts")})
		wantCode(t, err, "ParameterNotFound")
	}

	deleted, err := c.DeleteParameters(ctx, &ssm.DeleteParametersInput{Names: []string{"hosts", "/app/none"}})
	if err != nil || !slices.Equal(deleted.DeletedParameters, []string{"hosts"}) ||
		!slices.Equal(deleted.InvalidParameters, []string{"/app/none"}) {
		t.Fatalf("got %+v, %v", deleted, err)
	}

	if _, err = c.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String("/app/db/url")}); err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String("/app/db/url")})
	wantCode(t, err, "ParameterNotFound")

	// A new parameter with a deleted one's name starts again at version 1.
	if v := put(t, c, "/app/db/url", "fresh"); v != 1 {
		t.Fatalf("version %d", v)
	}
}

func TestSSMPaths(t *testing.T) {
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, name := range []string{"/app/a", "/app/b", "/app/db/url", "/app/db/user", "/apple", "top"} {
		put(t, c, name, "v")
	}

	if _, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/app/db/pass"), Value: aws.String("secret"), Type: ssmtypes.ParameterTypeSecureString,
		Tags: []ssmtypes.Tag{{Key: aws.String("team"), Value: aws.String("db")}},
	}); err != nil {
		t.Fatal(err)
	}

	level, err := c.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{Path: aws.String("/app")})
	if err != nil || !slices.Equal(names(level.Parameters), []string{"/app/a", "/app/b"}) {
		t.Fatalf("got %+v, %v", level, err)
	}

	var all []string
	input := &ssm.GetParametersByPathInput{
		Path: aws.String("/app/"), Recursive: aws.Bool(true), MaxResults: aws.Int32(2), WithDecryption: aws.Bool(true),
	}
	for {
		out, pageErr := c.GetParametersByPath(ctx, input)
		if pageErr != nil {
			t.Fatal(pageErr)
		}

		all = append(all, names(out.Parameters)...)
		for _, p := range out.Parameters {
			if aws.ToString(p.Name) == "/app/db/pass" && aws.ToString(p.Value) != "secret" {
				t.Fatalf("decrypted %+v", p)
			}
		}

		if out.NextToken == nil {
			break
		}

		input.NextToken = out.NextToken
	}

	if !slices.Equal(all, []string{"/app/a", "/app/b", "/app/db/pass", "/app/db/url", "/app/db/user"}) {
		t.Fatalf("recursive %v", all)
	}

	secure, err := c.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path: aws.String("/app/db"), ParameterFilters: []ssmtypes.ParameterStringFilter{
			{Key: aws.String("Type"), Values: []string{"SecureString"}},
		},
	})
	if err != nil || !slices.Equal(names(secure.Parameters), []string{"/app/db/pass"}) {
		t.Fatalf("got %+v, %v", secure, err)
	}

	_, err = c.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{Path: aws.String("app")})
	wantCode(t, err, "ValidationException")

	_, err = c.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path: aws.String("/app"), ParameterFilters: []ssmtypes.ParameterStringFilter{
			{Key: aws.String("Name"), Values: []string{"/app/a"}},
		},
	})
	wantCode(t, err, "InvalidFilterKey")

	for _, tc := range []struct {
		filter *ssmtypes.ParameterStringFilter
		want   []string
	}{
		{&ssmtypes.ParameterStringFilter{Key: aws.String("Name"), Values: []string{"top"}}, []string{"top"}},
		{
			&ssmtypes.ParameterStringFilter{
				Key: aws.String("Name"), Option: aws.String("BeginsWith"), Values: []string{"/app/d"},
			},
			[]string{"/app/db/pass", "/app/db/url", "/app/db/user"},
		},
		{
			&ssmtypes.ParameterStringFilter{Key: aws.String("Name"), Option: aws.String("Contains"), Values: []string{"pl"}},
			[]string{"/apple"},
		},
		{&ssmtypes.ParameterStringFilter{Key: aws.String("Path"), Values: []string{"/"}}, []string{"/apple"}},
		{
			&ssmtypes.ParameterStringFilter{
				Key: aws.String("Path"), Option: aws.String("Recursive"), Values: []string{"/app/db"},
			},
			[]string{"/app/db/pass", "/app/db/url", "/app/db/user"},
		},
		{
			&ssmtypes.ParameterStringFilter{Key: aws.String("KeyId"), Values: []string{"alias/aws/ssm"}},
			[]string{"/app/db/pass"},
		},
		{&ssmtypes.ParameterStringFilter{Key: aws.String("tag:team"), Values: []string{"db"}}, []string{"/app/db/pass"}},
	} {
		out, describeErr := c.DescribeParameters(ctx, &ssm.DescribeParametersInput{
			ParameterFilters: []ssmtypes.ParameterStringFilter{*tc.filter},
		})
		var got []string
		if out != nil {
			for _, p := range out.Parameters {
				got = append(got, aws.ToString(p.Name))
			}
		}

		if describeErr != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s %v: got %v, %v", *tc.filter.Key, tc.filter.Values, got, describeErr)
		}
	}

	_, err = c.DescribeParameters(ctx, &ssm.DescribeParametersInput{
		ParameterFilters: []ssmtypes.ParameterStringFilter{
			{Key: aws.String("Name"), Option: aws.String("Recursive"), Values: []string{"x"}},
		},
	})
	wantCode(t, err, "InvalidFilterOption")

	_, err = c.DescribeParameters(ctx, &ssm.DescribeParametersInput{NextToken: aws.String("!")})
	wantCode(t, err, "InvalidNextToken")
}

func TestSSMSecureString(t *testing.T) {
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	ctx := context.Background()
	if _, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("token"), Value: aws.String("hunter2"), Type: ssmtypes.ParameterTypeSecureString,
	}); err != nil {
		t.Fatal(err)
	}

	hidden, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("token")})
	if err != nil || aws.ToString(hidden.Parameter.Value) == "hunter2" || aws.ToString(hidden.Parameter.Value) == "" {
		t.Fatalf("got %+v, %v", hidden, err)
	}

	shown, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("token"), WithDecryption: aws.Bool(true)})
	if err != nil || aws.ToString(shown.Parameter.Value) != "hunter2" {
		t.Fatalf("got %+v, %v", shown, err)
	}

	described, err := c.DescribeParameters(ctx, &ssm.DescribeParametersInput{})
	if err != nil || aws.ToString(described.Parameters[0].KeyId) != "alias/aws/ssm" {
		t.Fatalf("got %+v, %v", described, err)
	}

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("custom"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeSecureString,
		KeyId: aws.String("alias/mine"),
	})
	wantCode(t, err, "InvalidKeyId")

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("plain"), Value: aws.String("v"), Type: ssmtypes.ParameterTypeString,
		KeyId: aws.String("alias/aws/ssm"),
	})
	wantCode(t, err, "ValidationException")
}

func TestSSMValidation(t *testing.T) {
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	ctx := context.Background()

	_, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("port"), Value: aws.String("80a"), Type: ssmtypes.ParameterTypeString,
		AllowedPattern: aws.String(`\d+`),
	})
	wantCode(t, err, "ParameterPatternMismatchException")

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("port"), Value: aws.String("80"), Type: ssmtypes.ParameterTypeString,
		AllowedPattern: aws.String(`(`),
	})
	wantCode(t, err, "InvalidAllowedPatternException")

	large := strings.Repeat("x", 5000)
	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("large"), Value: &large, Type: ssmtypes.ParameterTypeString,
	})
	wantCode(t, err, "ValidationException")

	tiered, err := c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("large"), Value: &large, Type: ssmtypes.ParameterTypeString,
		Tier: ssmtypes.ParameterTierIntelligentTiering,
	})
	if err != nil || tiered.Tier != ssmtypes.ParameterTierAdvanced {
		t.Fatalf("got %+v, %v", tiered, err)
	}

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("large"), Value: aws.String("small"), Overwrite: aws.Bool(true),
		Tier: ssmtypes.ParameterTierStandard,
	})
	wantCode(t, err, "ValidationException")

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("ami"), Value: aws.String("ami-1"), Type: ssmtypes.ParameterTypeString,
		DataType: aws.String("aws:ec2:image"),
	})
	wantCode(t, err, "ValidationException")

	_, err = c.GetParameters(ctx, &ssm.GetParametersInput{Names: make([]string, 11)})
	wantCode(t, err, "ValidationException")
}

func TestSSMLabelsAndHistory(t *testing.T) {
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	ctx := context.Background()
	for _, v := range []string{"one", "two", "three"} {
		put(t, c, "/svc/mode", v)
	}

	labeled, err := c.LabelParameterVersion(ctx, &ssm.LabelParameterVersionInput{
		Name: aws.String("/svc/mode"), ParameterVersion: aws.Int64(1), Labels: []string{"prod", "aws-x", "9lives"},
	})
	if err != nil || labeled.ParameterVersion != 1 ||
		!slices.Equal(labeled.InvalidLabels, []string{"aws-x", "9lives"}) {
		t.Fatalf("got %+v, %v", labeled, err)
	}

	byLabel, err := c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/svc/mode:prod")})
	if err != nil || aws.ToString(byLabel.Parameter.Value) != "one" {
		t.Fatalf("got %+v, %v", byLabel, err)
	}

	// Labeling the current version moves the label.
	if _, err = c.LabelParameterVersion(ctx, &ssm.LabelParameterVersionInput{
		Name: aws.String("/svc/mode"), Labels: []string{"prod", "canary"},
	}); err != nil {
		t.Fatal(err)
	}

	history, err := c.GetParameterHistory(ctx, &ssm.GetParameterHistoryInput{Name: aws.String("/svc/mode")})
	if err != nil || len(history.Parameters) != 3 || aws.ToString(history.Parameters[0].Value) != "one" ||
		len(history.Parameters[0].Labels) != 0 ||
		!slices.Equal(history.Parameters[2].Labels, []string{"canary", "prod"}) {
		t.Fatalf("got %+v, %v", history, err)
	}

	paged, err := c.GetParameterHistory(ctx, &ssm.GetParameterHistoryInput{
		Name: aws.String("/svc/mode"), MaxResults: aws.Int32(2),
	})
	if err != nil || len(paged.Parameters) != 2 || paged.NextToken == nil {
		t.Fatalf("got %+v, %v", paged, err)
	}

	rest, err := c.GetParameterHistory(ctx, &ssm.GetParameterHistoryInput{
		Name: aws.String("/svc/mode"), NextToken: paged.NextToken,
	})
	if err != nil || len(rest.Parameters) != 1 || rest.Parameters[0].Version != 3 || rest.NextToken != nil {
		t.Fatalf("got %+v, %v", rest, err)
	}

	unlabeled, err := c.UnlabelParameterVersion(ctx, &ssm.UnlabelParameterVersionInput{
		Name: aws.String("/svc/mode"), ParameterVersion: aws.Int64(3), Labels: []string{"canary", "missing"},
	})
	if err != nil || !slices.Equal(unlabeled.RemovedLabels, []string{"canary"}) ||
		!slices.Equal(unlabeled.InvalidLabels, []string{"missing"}) {
		t.Fatalf("got %+v, %v", unlabeled, err)
	}

	_, err = c.LabelParameterVersion(ctx, &ssm.LabelParameterVersionInput{
		Name: aws.String("/svc/mode"), ParameterVersion: aws.Int64(7), Labels: []string{"x"},
	})
	wantCode(t, err, "ParameterVersionNotFound")

	many := []string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8", "l9", "l10"}
	_, err = c.LabelParameterVersion(ctx, &ssm.LabelParameterVersionInput{
		Name: aws.String("/svc/mode"), ParameterVersion: aws.Int64(3), Labels: many,
	})
	wantCode(t, err, "ParameterVersionLabelLimitExceeded")

	// A parameter keeps its 100 newest versions unless the oldest is labeled.
	put(t, c, "/svc/count", "v1")
	if _, err = c.LabelParameterVersion(ctx, &ssm.LabelParameterVersionInput{
		Name: aws.String("/svc/count"), ParameterVersion: aws.Int64(1), Labels: []string{"first"},
	}); err != nil {
		t.Fatal(err)
	}

	for range 99 {
		put(t, c, "/svc/count", "v")
	}

	_, err = c.PutParameter(ctx, &ssm.PutParameterInput{
		Name: aws.String("/svc/count"), Value: aws.String("v"), Overwrite: aws.Bool(true),
	})
	wantCode(t, err, "ParameterMaxVersionLimitExceeded")

	if _, err = c.UnlabelParameterVersion(ctx, &ssm.UnlabelParameterVersionInput{
		Name: aws.String("/svc/count"), ParameterVersion: aws.Int64(1), Labels: []string{"first"},
	}); err != nil {
		t.Fatal(err)
	}

	if v := put(t, c, "/svc/count", "v101"); v != 101 {
		t.Fatalf("version %d", v)
	}

	_, err = c.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/svc/count:1")})
	wantCode(t, err, "ParameterVersionNotFound")
}

func TestSSMTags(t *testing.T) {
	s := server(t)
	c := ssmClient(s, "test", "us-east-1")
	ctx := context.Background()
	put(t, c, "/tagged", "v")
	parameter := ssmtypes.ResourceTypeForTaggingParameter

	if _, err := c.AddTagsToResource(ctx, &ssm.AddTagsToResourceInput{
		ResourceType: parameter,
		ResourceId:   aws.String("/tagged"),
		Tags: []ssmtypes.Tag{
			{Key: aws.String("b"), Value: aws.String("2")},
			{Key: aws.String("a"), Value: aws.String("1")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.RemoveTagsFromResource(ctx, &ssm.RemoveTagsFromResourceInput{
		ResourceType: parameter, ResourceId: aws.String("/tagged"), TagKeys: []string{"b"},
	}); err != nil {
		t.Fatal(err)
	}

	listed, err := c.ListTagsForResource(ctx, &ssm.ListTagsForResourceInput{
		ResourceType: parameter, ResourceId: aws.String("/tagged"),
	})
	if err != nil || len(listed.TagList) != 1 || aws.ToString(listed.TagList[0].Key) != "a" {
		t.Fatalf("got %+v, %v", listed, err)
	}

	_, err = c.ListTagsForResource(ctx, &ssm.ListTagsForResourceInput{
		ResourceType: ssmtypes.ResourceTypeForTaggingManagedInstance, ResourceId: aws.String("/tagged"),
	})
	wantCode(t, err, "InvalidResourceType")

	_, err = c.ListTagsForResource(ctx, &ssm.ListTagsForResourceInput{
		ResourceType: parameter, ResourceId: aws.String("/missing"),
	})
	wantCode(t, err, "InvalidResourceId")

	tooMany := make([]ssmtypes.Tag, 50)
	for i := range tooMany {
		tooMany[i] = ssmtypes.Tag{Key: aws.String(strings.Repeat("k", i+1)), Value: aws.String("v")}
	}

	_, err = c.AddTagsToResource(ctx, &ssm.AddTagsToResourceInput{
		ResourceType: parameter, ResourceId: aws.String("/tagged"), Tags: tooMany,
	})
	wantCode(t, err, "TooManyTagsError")
}

func TestSSMPersists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := ssmClient(s, "test", "us-east-1")
	put(t, c, "/kept", "one")
	put(t, c, "/kept", "two")
	if _, err := c.LabelParameterVersion(ctx, &ssm.LabelParameterVersionInput{
		Name: aws.String("/kept"), ParameterVersion: aws.Int64(1), Labels: []string{"old"},
	}); err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	got, err := ssmClient(s, "test", "us-east-1").GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String("/kept:old"),
	})
	if err != nil || aws.ToString(got.Parameter.Value) != "one" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
