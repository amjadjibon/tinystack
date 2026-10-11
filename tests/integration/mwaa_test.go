package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/mwaa"
	"github.com/aws/aws-sdk-go-v2/service/mwaa/types"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/amjadjibon/tinystack/internal/storage/archive"
)

func mwaaClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       region,
			BaseEndpoint: new(s.URL),
			Credentials:  creds(key),
			Retryer:      aws.NopRetryer{},
			APIOptions: []func(*middleware.Stack) error{func(stack *middleware.Stack) error {
				return stack.Initialize.Add(
					middleware.InitializeMiddlewareFunc(
						"DisableHostPrefix",
						func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
							middleware.InitializeOutput, middleware.Metadata, error,
						) {
							return next.HandleInitialize(smithyhttp.DisableEndpointHostPrefix(ctx, true), in)
						},
					),
					middleware.Before,
				)
			}},
		},
	)
}

func mwaaInput(t *testing.T, s *httptest.Server, region string) *sdk.CreateEnvironmentInput {
	t.Helper()
	network := mskInput(t, s, region).BrokerNodeGroupInfo
	c := s3.New(
		s3.Options{
			Region:       region,
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
			UsePathStyle: true,
		},
	)
	bucket := "mwaa-source-" + region
	in := &s3.CreateBucketInput{Bucket: new(bucket)}
	if region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(region),
		}
	}
	ctx := context.Background()
	if _, e := c.CreateBucket(ctx, in); e != nil {
		t.Fatal(e)
	}
	if _, e := c.PutBucketVersioning(
		ctx,
		&s3.PutBucketVersioningInput{
			Bucket:                  new(bucket),
			VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
		},
	); e != nil {
		t.Fatal(e)
	}
	return &sdk.CreateEnvironmentInput{
		Name:             new("records-environment"),
		AirflowVersion:   new("2.11.0"),
		EnvironmentClass: new("mw1.medium"),
		ExecutionRoleArn: new(
			"arn:aws:iam::000000000000:role/path/mwaa",
		),
		SourceBucketArn: new("arn:aws:s3:::" + bucket),
		DagS3Path:       new("dags/team"),
		NetworkConfiguration: &types.NetworkConfiguration{
			SubnetIds:        network.ClientSubnets,
			SecurityGroupIds: network.SecurityGroups,
		},
		MinWorkers: new(int32(3)),
		MaxWorkers: new(int32(17)),
		Schedulers: new(int32(4)),
		Tags:       map[string]string{"owner": "local"},
	}
}

func mwaaGet(ctx context.Context, t *testing.T, c *sdk.Client, name *string) *types.Environment {
	t.Helper()
	o, e := c.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: name})
	if e != nil {
		t.Fatal(e)
	}
	return o.Environment
}

func mwaaStatus(t *testing.T, e error, name string, status int) {
	t.Helper()
	code(t, e, name)
	var r *smithyhttp.ResponseError
	if !errors.As(e, &r) || r.HTTPStatusCode() != status {
		t.Fatalf("want %s/%d got %v", name, status, e)
	}
}

func TestMWAASDKLifecycleTypedErrorsScopeRGTAAndSigning(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprint(verify), func(t *testing.T) {
			s := server(t)
			if verify {
				s = verifyingServer(t)
			}
			c := mwaaClient(s, "test", "us-east-1")
			ctx := context.Background()
			in := mwaaInput(t, s, "us-east-1")
			out, e := c.CreateEnvironment(ctx, in)
			if e != nil {
				t.Fatal(e)
			}
			before := mwaaGet(ctx, t, c, in.Name)
			if *out.Arn != "arn:aws:airflow:us-east-1:000000000000:environment/records-environment" ||
				before.Status != "" ||
				before.LastUpdate != nil ||
				before.WebserverUrl != nil ||
				before.CeleryExecutorQueue != nil ||
				before.DatabaseVpcEndpointService != nil ||
				before.WebserverVpcEndpointService != nil ||
				before.CreatedAt == nil ||
				*before.MaxWorkers != 17 ||
				*before.Schedulers != 4 ||
				*before.AirflowVersion != "2.11.0" ||
				!reflect.DeepEqual(before.NetworkConfiguration, in.NetworkConfiguration) {
				t.Fatal(before)
			}
			_, e = c.CreateEnvironment(ctx, in)
			var duplicate *types.ValidationException
			if !errors.As(e, &duplicate) {
				t.Fatal("typed duplicate", e)
			}
			mwaaStatus(t, e, "ValidationException", 400)
			updated, e := c.UpdateEnvironment(
				ctx,
				&sdk.UpdateEnvironmentInput{Name: in.Name, DagS3Path: new("new/prefix"), MinWorkers: new(int32(5))},
			)
			if e != nil || *updated.Arn != *out.Arn {
				t.Fatal(updated, e)
			}
			after := mwaaGet(ctx, t, c, in.Name)
			if *after.DagS3Path != "new/prefix" || *after.MinWorkers != 5 ||
				!after.CreatedAt.Equal(*before.CreatedAt) ||
				after.LastUpdate != nil {
				t.Fatal("update metadata", after)
			}
			_, e = c.UpdateEnvironment(
				ctx,
				&sdk.UpdateEnvironmentInput{
					Name:       in.Name,
					MinWorkers: new(int32(18)),
					DagS3Path:  new("must-not-write"),
				},
			)
			mwaaStatus(t, e, "ValidationException", 400)
			if !reflect.DeepEqual(after, mwaaGet(ctx, t, c, in.Name)) {
				t.Fatal("failed candidate wrote")
			}
			_, e = c.TagResource(
				ctx,
				&sdk.TagResourceInput{ResourceArn: out.Arn, Tags: map[string]string{"merged": "value"}},
			)
			if e != nil {
				t.Fatal(e)
			}
			tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: out.Arn})
			if e != nil || len(tags.Tags) != 2 {
				t.Fatal(tags, e)
			}
			_, e = c.UntagResource(
				ctx,
				&sdk.UntagResourceInput{ResourceArn: out.Arn, TagKeys: []string{"merged", "absent"}},
			)
			if e != nil {
				t.Fatal(e)
			}
			list, e := c.ListEnvironments(ctx, &sdk.ListEnvironmentsInput{MaxResults: new(int32(1))})
			if e != nil || !reflect.DeepEqual(list.Environments, []string{*in.Name}) || list.NextToken != nil {
				t.Fatal(list, e)
			}
			for _, foreign := range []*sdk.Client{mwaaClient(s, "test", "us-west-2"), mwaaClient(s, "team-b", "us-east-1")} {
				if verify {
					continue
				}
				_, e = foreign.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: in.Name})
				var missing *types.ResourceNotFoundException
				if !errors.As(e, &missing) {
					t.Fatal(e)
				}
				mwaaStatus(t, e, "ResourceNotFoundException", 404)
				_, e = foreign.DeleteEnvironment(ctx, &sdk.DeleteEnvironmentInput{Name: in.Name})
				mwaaStatus(t, e, "ResourceNotFoundException", 404)
			}
			rg := resourcegroupstaggingapi.New(
				resourcegroupstaggingapi.Options{
					Region:       "us-east-1",
					BaseEndpoint: new(s.URL),
					Credentials:  creds("test"),
					Retryer:      aws.NopRetryer{},
				},
			)
			tagged, e := rg.TagResources(
				ctx,
				&resourcegroupstaggingapi.TagResourcesInput{
					ResourceARNList: []string{*out.Arn},
					Tags:            map[string]string{"rgta": "real"},
				},
			)
			if e != nil || len(tagged.FailedResourcesMap) != 0 {
				t.Fatal(tagged, e)
			}
			mappings, e := rg.GetResources(
				ctx,
				&resourcegroupstaggingapi.GetResourcesInput{ResourceARNList: []string{*out.Arn}},
			)
			if e != nil || len(mappings.ResourceTagMappingList) != 1 {
				t.Fatal(mappings, e)
			}
			removed, e := rg.UntagResources(
				ctx,
				&resourcegroupstaggingapi.UntagResourcesInput{
					ResourceARNList: []string{*out.Arn},
					TagKeys:         []string{"rgta"},
				},
			)
			if e != nil || len(removed.FailedResourcesMap) != 0 {
				t.Fatal(removed, e)
			}
			if verify {
				_, e = mwaaClient(s, "wrong", "us-east-1").ListEnvironments(ctx, &sdk.ListEnvironmentsInput{})
				code(t, e, "InvalidSignatureException")
			}
			_, e = c.DeleteEnvironment(ctx, &sdk.DeleteEnvironmentInput{Name: in.Name})
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.DeleteEnvironment(ctx, &sdk.DeleteEnvironmentInput{Name: in.Name})
			mwaaStatus(t, e, "ResourceNotFoundException", 404)
		})
	}
}

func mwaaWire(t *testing.T, s *httptest.Server, verb, path, body string) (status int, document map[string]any) {
	t.Helper()
	r, e := http.NewRequestWithContext(context.Background(), verb, s.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(
		"Authorization",
		"AWS4-HMAC-SHA256 Credential=test/20261011/us-east-1/airflow/aws4_request, SignedHeaders=host, Signature=abcd",
	)
	res, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if e = json.Unmarshal(raw, &out); e != nil {
			t.Fatal(string(raw), e)
		}
	}
	return res.StatusCode, out
}

func mwaaWireInput(in *sdk.CreateEnvironmentInput) map[string]any {
	return map[string]any{
		"AirflowVersion":   *in.AirflowVersion,
		"EnvironmentClass": *in.EnvironmentClass,
		"ExecutionRoleArn": *in.ExecutionRoleArn,
		"SourceBucketArn":  *in.SourceBucketArn,
		"DagS3Path":        *in.DagS3Path,
		"NetworkConfiguration": map[string]any{
			"SubnetIds":        in.NetworkConfiguration.SubnetIds,
			"SecurityGroupIds": in.NetworkConfiguration.SecurityGroupIds,
		},
		"MinWorkers": 3,
		"MaxWorkers": 17,
		"Schedulers": 4,
		"Tags":       map[string]string{"owner": "local"},
	}
}

func TestMWAARawPinnedRESTBindingsStrictSelectionAndRuntimeExclusion(t *testing.T) {
	s := server(t)
	in := mwaaInput(t, s, "us-east-1")
	body := mwaaWireInput(in)
	raw, _ := json.Marshal(body)
	path := "/environments/records-environment"
	status, out := mwaaWire(t, s, "PUT", path, string(raw))
	if status != 200 || out["Arn"] == nil {
		t.Fatal(status, out)
	}
	arn := out["Arn"].(string)
	status, out = mwaaWire(t, s, "GET", path, "")
	if status != 200 {
		t.Fatal(status, out)
	}
	env := out["Environment"].(map[string]any)
	if env["CreatedAt"] == nil || env["Status"] != nil || env["LastUpdate"] != nil || env["WebserverUrl"] != nil {
		t.Fatal(env)
	}
	for _, value := range []any{false, nil, map[string]any{}} {
		for _, member := range []string{
			"KmsKey",
			"AirflowConfigurationOptions",
			"LoggingConfiguration",
			"PluginsS3Path",
			"PluginsS3ObjectVersion",
			"RequirementsS3Path",
			"RequirementsS3ObjectVersion",
			"StartupScriptS3Path",
			"StartupScriptS3ObjectVersion",
			"WebserverAccessMode",
			"WeeklyMaintenanceWindowStart",
			"EndpointManagement",
			"MinWebservers",
			"MaxWebservers",
			"Name",
			"ClientToken",
		} {
			candidate := mwaaWireInput(in)
			candidate[member] = value
			raw, _ = json.Marshal(candidate)
			status, _ = mwaaWire(t, s, "PUT", "/environments/Excluded", string(raw))
			if status != 501 {
				t.Fatal("excluded create", member, value, status)
			}
		}
		candidate := mwaaWireInput(in)
		candidate["NetworkConfiguration"].(map[string]any)["VpcId"] = value
		raw, _ = json.Marshal(candidate)
		status, _ = mwaaWire(t, s, "PUT", "/environments/Excluded", string(raw))
		if status != 501 {
			t.Fatal("nested exclusion", value, status)
		}
		for _, member := range []string{
			"AirflowVersion",
			"NetworkConfiguration",
			"Tags",
			"WorkerReplacementStrategy",
			"KmsKey",
			"LoggingConfiguration",
			"AirflowConfigurationOptions",
			"PluginsS3Path",
			"WeeklyMaintenanceWindowStart",
		} {
			raw, _ = json.Marshal(map[string]any{member: value, "DagS3Path": "must-not-write"})
			status, _ = mwaaWire(t, s, "PATCH", path, string(raw))
			if status != 501 {
				t.Fatal("excluded update", member, value, status)
			}
		}
	}
	for _, member := range []string{
		"ExecutionRoleArn",
		"SourceBucketArn",
		"DagS3Path",
		"NetworkConfiguration",
		"AirflowVersion",
	} {
		candidate := mwaaWireInput(in)
		delete(candidate, member)
		raw, _ = json.Marshal(candidate)
		status, out = mwaaWire(t, s, "PUT", "/environments/Required", string(raw))
		if status != 400 || out["message"] == nil {
			t.Fatal(member, status, out)
		}
	}
	status, _ = mwaaWire(t, s, "PUT", "/environments/WrongCase", `{"executionRoleArn":"wrong-case"}`)
	if status < 400 {
		t.Fatal("casing", status)
	}
	status, out = mwaaWire(t, s, "PATCH", path, `{"DagS3Path":"changed/prefix","MaxWorkers":20}`)
	if status != 200 || out["Arn"] != arn {
		t.Fatal(status, out)
	}
	status, out = mwaaWire(t, s, "GET", "/environments?MaxResults=1", "")
	if status != 200 || len(out["Environments"].([]any)) != 1 {
		t.Fatal(status, out)
	}
	for _, q := range []string{
		"MaxResults=0",
		"MaxResults=26",
		"MaxResults=bad",
		"NextToken=bad",
		"maxResults=1",
		"Name=filter",
		"unknown=false",
	} {
		status, _ = mwaaWire(t, s, "GET", "/environments?"+q, "")
		if status < 400 {
			t.Fatal(q, status)
		}
	}
	tagPath := "/tags/" + url.PathEscape(arn)
	status, _ = mwaaWire(t, s, "POST", tagPath, `{"Tags":{"a":"value","b":"keep"}}`)
	if status != 200 {
		t.Fatal(status)
	}
	status, out = mwaaWire(t, s, "GET", tagPath, "")
	if status != 200 || len(out["Tags"].(map[string]any)) != 3 {
		t.Fatal(status, out)
	}
	status, _ = mwaaWire(t, s, "DELETE", tagPath+"?tagKeys=a&tagKeys=absent", "")
	if status != 200 {
		t.Fatal(status)
	}
	status, _ = mwaaWire(t, s, "DELETE", tagPath+"?TagKeys=b", "")
	if status < 400 {
		t.Fatal("tagKeys case")
	}
	for _, route := range []string{
		"/clitoken/records-environment",
		"/webtoken/records-environment",
		"/restapi/records-environment",
		"/metrics/environments/records-environment",
	} {
		status, _ = mwaaWire(t, s, "POST", route, `{}`)
		if status >= 200 && status < 300 {
			t.Fatal("runtime route", route, status)
		}
	}
	status, out = mwaaWire(t, s, "DELETE", path, "")
	if status != 200 || len(out) != 0 {
		t.Fatal(status, out)
	}
	status, out = mwaaWire(t, s, "DELETE", path, "")
	if status != 404 || out["message"] == nil ||
		!strings.Contains(out["__type"].(string), "ResourceNotFoundException") {
		t.Fatal("modeled wire error", status, out)
	}
}

func TestMWAASQLiteOfflineFixtureRestartExactScopeAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	in := mwaaInput(t, s, "us-west-2")
	c := mwaaClient(s, "test", "us-west-2")
	_, e := c.CreateEnvironment(ctx, in)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	_, e = c.UpdateEnvironment(
		ctx,
		&sdk.UpdateEnvironmentInput{
			Name:       in.Name,
			DagS3Path:  new("exact/nondefault"),
			MinWorkers: new(int32(5)),
			MaxWorkers: new(int32(21)),
			Schedulers: new(int32(5)),
		},
	)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	_, e = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceArn: mwaaGet(ctx, t, c, in.Name).Arn,
			Tags:        map[string]string{"fixture": "exact"},
		},
	)
	if e != nil {
		stop()
		t.Fatal(e)
	}
	before := mwaaGet(ctx, t, c, in.Name)
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	c = mwaaClient(s, "test", "us-west-2")
	if !reflect.DeepEqual(before, mwaaGet(ctx, t, c, in.Name)) {
		stop()
		t.Fatal("SQLite metadata")
	}
	stop()
	fixture := filepath.Join(t.TempDir(), "mwaa.tar.gz")
	if e = archive.Export(ctx, archive.ExportOptions{StorageDir: dir, OutputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "child")
	if e = archive.Import(ctx, archive.ImportOptions{StorageDir: child, InputFile: fixture}); e != nil {
		t.Fatal(e)
	}
	s, stop = persistent(t, child, &fakeRuntime{})
	c = mwaaClient(s, "test", "us-west-2")
	if !reflect.DeepEqual(before, mwaaGet(ctx, t, c, in.Name)) {
		stop()
		t.Fatal("fixture")
	}
	stop()
	s, stop = persistent(t, child, &fakeRuntime{})
	defer stop()
	c = mwaaClient(s, "test", "us-west-2")
	if !reflect.DeepEqual(before, mwaaGet(ctx, t, c, in.Name)) {
		t.Fatal("fixture restart")
	}
	_, e = mwaaClient(s, "test", "us-east-1").GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: in.Name})
	mwaaStatus(t, e, "ResourceNotFoundException", 404)
	res, e := s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || !strings.Contains(string(raw), *before.Arn) || !strings.Contains(string(raw), "RecordsOnly") ||
		!strings.Contains(string(raw), "ExecutionReady") {
		t.Fatal(string(raw), e)
	}
	r, e := http.NewRequestWithContext(ctx, "POST", s.URL+"/_tinystack/reset", http.NoBody)
	if e != nil {
		t.Fatal(e)
	}
	res, e = s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	_, e = c.GetEnvironment(ctx, &sdk.GetEnvironmentInput{Name: in.Name})
	mwaaStatus(t, e, "ResourceNotFoundException", 404)
	res, e = s.Client().Get(s.URL + "/_tinystack/resources")
	if e != nil {
		t.Fatal(e)
	}
	raw, e = io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || strings.Contains(string(raw), *before.Arn) {
		t.Fatal("reset inventory", e)
	}
}

func TestMWAASDKPaginationDeletedCursorAndTokenScope(t *testing.T) {
	s := server(t)
	c := mwaaClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := mwaaInput(t, s, "us-east-1")
	for _, name := range []string{"Charlie", "Alpha", "Bravo"} {
		in.Name = new(name)
		if _, e := c.CreateEnvironment(ctx, in); e != nil {
			t.Fatal(e)
		}
	}
	one, e := c.ListEnvironments(ctx, &sdk.ListEnvironmentsInput{MaxResults: new(int32(1))})
	if e != nil || one.NextToken == nil || one.Environments[0] != "Alpha" {
		t.Fatal(one, e)
	}
	if _, e = c.DeleteEnvironment(ctx, &sdk.DeleteEnvironmentInput{Name: new("Alpha")}); e != nil {
		t.Fatal(e)
	}
	two, e := c.ListEnvironments(ctx, &sdk.ListEnvironmentsInput{MaxResults: new(int32(2)), NextToken: one.NextToken})
	if e != nil || two.NextToken != nil || !reflect.DeepEqual(two.Environments, []string{"Bravo", "Charlie"}) {
		t.Fatal(two, e)
	}
	_, e = mwaaClient(
		s,
		"test",
		"us-west-2",
	).ListEnvironments(ctx, &sdk.ListEnvironmentsInput{NextToken: one.NextToken})
	mwaaStatus(t, e, "ValidationException", 400)
}

func TestMWAARealS3ScopedMetadataAndNetworkValidation(t *testing.T) {
	s := server(t)
	c := mwaaClient(s, "test", "us-east-1")
	ctx := context.Background()
	in := mwaaInput(t, s, "us-east-1")
	sc := s3.New(
		s3.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test"), UsePathStyle: true},
	)
	bucket := new("mwaa-source-us-east-1")
	if _, e := sc.PutBucketVersioning(
		ctx,
		&s3.PutBucketVersioningInput{
			Bucket:                  bucket,
			VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusSuspended},
		},
	); e != nil {
		t.Fatal(e)
	}
	_, e := c.CreateEnvironment(ctx, in)
	mwaaStatus(t, e, "ValidationException", 400)
	if _, e = sc.PutBucketVersioning(
		ctx,
		&s3.PutBucketVersioningInput{
			Bucket:                  bucket,
			VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
		},
	); e != nil {
		t.Fatal(e)
	}
	if _, e = sc.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: bucket}); e != nil {
		t.Fatal(e)
	}
	_, e = c.CreateEnvironment(ctx, in)
	mwaaStatus(t, e, "ValidationException", 400)
	if _, e = sc.PutPublicAccessBlock(
		ctx,
		&s3.PutPublicAccessBlockInput{
			Bucket: bucket,
			PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
				BlockPublicAcls:       new(true),
				IgnorePublicAcls:      new(true),
				BlockPublicPolicy:     new(true),
				RestrictPublicBuckets: new(true),
			},
		},
	); e != nil {
		t.Fatal(e)
	}
	in.MinWorkers = nil
	in.MaxWorkers = nil
	in.Schedulers = nil
	in.EnvironmentClass = nil
	in.Tags = nil
	_, e = c.CreateEnvironment(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	before := mwaaGet(ctx, t, c, in.Name)
	if *before.MinWorkers != 1 || *before.MaxWorkers != 10 || *before.Schedulers != 2 ||
		*before.EnvironmentClass != "mw1.small" {
		t.Fatal("documented defaults", before)
	}
	for _, ref := range []string{
		"arn:aws:s3:::missing-bucket",
		"arn:aws:s3:::mwaa-source-us-east-1/object",
		"arn:aws-cn:s3:::mwaa-source-us-east-1",
		"s3://mwaa-source-us-east-1",
	} {
		_, e = c.UpdateEnvironment(
			ctx,
			&sdk.UpdateEnvironmentInput{Name: in.Name, SourceBucketArn: new(ref), DagS3Path: new("must-not-write")},
		)
		mwaaStatus(t, e, "ValidationException", 400)
		if !reflect.DeepEqual(before, mwaaGet(ctx, t, c, in.Name)) {
			t.Fatal("bad source wrote")
		}
	}
	// References must resolve to the same actual account and Region, rather than
	// relying on S3 ARNs to carry those components (bucket ARNs do not).
	for _, scope := range []struct{ key, region, bucket string }{
		{"team-b", "us-east-1", "mwaa-foreign-account"},
		{"test", "us-west-2", "mwaa-foreign-region"},
	} {
		sourceClient := s3.New(
			s3.Options{
				Region:       scope.region,
				BaseEndpoint: new(s.URL),
				Credentials:  creds(scope.key),
				UsePathStyle: true,
			},
		)
		bucketIn := &s3.CreateBucketInput{Bucket: new(scope.bucket)}
		if scope.region != "us-east-1" {
			bucketIn.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
				LocationConstraint: s3types.BucketLocationConstraint(scope.region),
			}
		}
		if _, err := sourceClient.CreateBucket(ctx, bucketIn); err != nil {
			t.Fatal(err)
		}
		_, e = c.UpdateEnvironment(
			ctx,
			&sdk.UpdateEnvironmentInput{Name: in.Name, SourceBucketArn: new("arn:aws:s3:::" + scope.bucket)},
		)
		mwaaStatus(t, e, "ValidationException", 400)
		if !reflect.DeepEqual(before, mwaaGet(ctx, t, c, in.Name)) {
			t.Fatal("foreign S3 source changed live state")
		}
	}
	in.Name = new("bad-network")
	in.NetworkConfiguration.SubnetIds[1] = in.NetworkConfiguration.SubnetIds[0]
	_, e = c.CreateEnvironment(ctx, in)
	mwaaStatus(t, e, "ValidationException", 400)
	_, e = mwaaClient(s, "team-b", "us-east-1").CreateEnvironment(ctx, in)
	mwaaStatus(t, e, "ValidationException", 400)
}
