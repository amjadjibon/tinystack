package integration_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	sdk "github.com/aws/aws-sdk-go-v2/service/dsql"
	"github.com/aws/aws-sdk-go-v2/service/dsql/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

func dsqlClient(s *httptest.Server, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds("test"), Retryer: aws.NopRetryer{}},
	)
}

func pollDSQL(t *testing.T, c *sdk.Client, id *string, want types.ClusterStatus) *sdk.GetClusterOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.GetCluster(ctx, &sdk.GetClusterInput{Identifier: id})
		if want == "" && err != nil {
			wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
			return nil
		}

		if err != nil {
			t.Fatal(err)
		}

		if out.Status == want {
			return out
		}

		select {
		case <-ctx.Done():
			t.Fatalf("DSQL status %s, want %s", out.Status, want)
		case <-ticker.C:
		}
	}
}

func TestDSQLLifecycle(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := dsqlClient(s, "us-east-1")
	ctx := context.Background()
	input := &sdk.CreateClusterInput{ClientToken: new("create"), Tags: map[string]string{"team": "core"}}
	made, err := c.CreateCluster(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	if made.Status != types.ClusterStatusCreating || !aws.ToBool(made.DeletionProtectionEnabled) ||
		len(*made.Identifier) != 26 ||
		*made.Endpoint != *made.Identifier+".dsql.us-east-1.on.aws" {
		t.Fatalf("create: %+v", made)
	}

	again, err := c.CreateCluster(ctx, input)
	if err != nil || *again.Identifier != *made.Identifier || again.Status != made.Status {
		t.Fatalf("replay: %+v %v", again, err)
	}

	_, err = c.CreateCluster(
		ctx,
		&sdk.CreateClusterInput{ClientToken: new("create"), DeletionProtectionEnabled: new(false)},
	)
	wantAccessAnalyzerError(t, err, "ConflictException")
	live := pollDSQL(t, c, made.Identifier, types.ClusterStatusActive)
	if live.Tags["team"] != "core" {
		t.Fatal(live.Tags)
	}

	_, err = c.DeleteCluster(ctx, &sdk.DeleteClusterInput{Identifier: made.Identifier})
	wantAccessAnalyzerError(t, err, "ConflictException")
	updated, err := c.UpdateCluster(ctx, &sdk.UpdateClusterInput{
		Identifier: made.Identifier, DeletionProtectionEnabled: new(false), ClientToken: new("update"),
	})
	if err != nil || updated.Status != types.ClusterStatusUpdating {
		t.Fatalf("update: %+v %v", updated, err)
	}

	pollDSQL(t, c, made.Identifier, types.ClusterStatusActive)
	updatedAgain, err := c.UpdateCluster(ctx, &sdk.UpdateClusterInput{
		Identifier: made.Identifier, DeletionProtectionEnabled: new(false), ClientToken: new("update"),
	})
	if err != nil || updatedAgain.Status != types.ClusterStatusUpdating {
		t.Fatalf("update replay: %+v %v", updatedAgain, err)
	}

	endpoint, err := c.GetVpcEndpointServiceName(ctx, &sdk.GetVpcEndpointServiceNameInput{Identifier: made.Identifier})
	if err != nil || endpoint.ServiceName == nil || endpoint.ClusterVpcEndpoint == nil {
		t.Fatalf("vpc: %+v %v", endpoint, err)
	}

	if _, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{ResourceArn: made.Arn, Tags: map[string]string{"extra": "yes"}},
	); err != nil {
		t.Fatal(err)
	}

	if _, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceArn: made.Arn, TagKeys: []string{"team"}},
	); err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: made.Arn})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["extra"] != "yes" {
		t.Fatalf("tags: %+v %v", tags, err)
	}

	tagClient := rgta.New(rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	tagged, err := tagClient.TagResources(
		ctx,
		&rgta.TagResourcesInput{ResourceARNList: []string{*made.Arn}, Tags: map[string]string{"shared": "yes"}},
	)
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("shared tags: %+v %v", tagged, err)
	}

	for range 2 {
		if _, err = c.CreateCluster(ctx, &sdk.CreateClusterInput{DeletionProtectionEnabled: new(false)}); err != nil {
			t.Fatal(err)
		}
	}

	var identifiers []string
	var next *string
	for {
		page, e := c.ListClusters(ctx, &sdk.ListClustersInput{MaxResults: new(int32(1)), NextToken: next})
		if e != nil {
			t.Fatal(e)
		}

		for _, item := range page.Clusters {
			identifiers = append(identifiers, *item.Identifier)
		}

		next = page.NextToken
		if next == nil {
			break
		}
	}

	if len(identifiers) != 3 || identifiers[0] >= identifiers[1] || identifiers[1] >= identifiers[2] {
		t.Fatal(identifiers)
	}

	_, err = c.ListClusters(ctx, &sdk.ListClustersInput{NextToken: new("!")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.GetCluster(ctx, &sdk.GetClusterInput{Identifier: new("bad")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.GetCluster(ctx, &sdk.GetClusterInput{Identifier: new("aaaaaaaaaaaaaaaaaaaaaaaaaa")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = dsqlClient(s, "us-east-2").GetCluster(ctx, &sdk.GetClusterInput{Identifier: made.Identifier})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.CreateCluster(
		ctx,
		&sdk.CreateClusterInput{MultiRegionProperties: &types.MultiRegionProperties{WitnessRegion: new("us-east-1")}},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{ResourceArn: made.Arn, Tags: map[string]string{"aws:bad": "x"}})
	wantAccessAnalyzerError(t, err, "ValidationException")

	removed, err := c.DeleteCluster(
		ctx,
		&sdk.DeleteClusterInput{Identifier: made.Identifier, ClientToken: new("delete")},
	)
	if err != nil || removed.Status != types.ClusterStatusDeleting {
		t.Fatalf("delete: %+v %v", removed, err)
	}

	pollDSQL(t, c, made.Identifier, "")
	removedAgain, err := c.DeleteCluster(
		ctx,
		&sdk.DeleteClusterInput{Identifier: made.Identifier, ClientToken: new("delete")},
	)
	if err != nil || removedAgain.Status != removed.Status {
		t.Fatalf("delete replay: %+v %v", removedAgain, err)
	}
}

func TestDSQLPolicies(t *testing.T) {
	t.Parallel()
	s := server(t)
	c := dsqlClient(s, "us-east-1")
	ctx := context.Background()
	made, err := c.CreateCluster(ctx, &sdk.CreateClusterInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetClusterPolicy(ctx, &sdk.GetClusterPolicyInput{Identifier: made.Identifier})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	policy := `{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Principal":"*","Action":"dsql:GetCluster","Resource":"*"}]}`
	put := &sdk.PutClusterPolicyInput{
		Identifier:                     made.Identifier,
		Policy:                         new(policy),
		BypassPolicyLockoutSafetyCheck: true,
		ClientToken:                    new("policy"),
	}
	first, err := c.PutClusterPolicy(ctx, put)
	if err != nil || first.PolicyVersion == nil {
		t.Fatalf("put: %+v %v", first, err)
	}

	again, err := c.PutClusterPolicy(ctx, put)
	if err != nil || *again.PolicyVersion != *first.PolicyVersion {
		t.Fatalf("replay: %+v %v", again, err)
	}

	got, err := c.GetClusterPolicy(ctx, &sdk.GetClusterPolicyInput{Identifier: made.Identifier})
	if err != nil || *got.Policy != policy || *got.PolicyVersion != *first.PolicyVersion {
		t.Fatalf("get: %+v %v", got, err)
	}

	_, err = c.PutClusterPolicy(ctx, &sdk.PutClusterPolicyInput{
		Identifier: made.Identifier, Policy: new(policy),
		BypassPolicyLockoutSafetyCheck: true, ExpectedPolicyVersion: new("stale"),
	})
	wantAccessAnalyzerError(t, err, "ConflictException")
	_, err = c.PutClusterPolicy(
		ctx,
		&sdk.PutClusterPolicyInput{
			Identifier:                     made.Identifier,
			Policy:                         new("no JSON"),
			BypassPolicyLockoutSafetyCheck: true,
		},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.PutClusterPolicy(ctx, &sdk.PutClusterPolicyInput{Identifier: made.Identifier, Policy: new(policy)})
	wantAccessAnalyzerError(t, err, "ValidationException")
	second, err := c.PutClusterPolicy(ctx, &sdk.PutClusterPolicyInput{
		Identifier: made.Identifier, Policy: new(policy),
		BypassPolicyLockoutSafetyCheck: true, ExpectedPolicyVersion: first.PolicyVersion,
	})
	if err != nil || *second.PolicyVersion == *first.PolicyVersion {
		t.Fatalf("version: %+v %v", second, err)
	}

	_, err = c.DeleteClusterPolicy(
		ctx,
		&sdk.DeleteClusterPolicyInput{Identifier: made.Identifier, ExpectedPolicyVersion: first.PolicyVersion},
	)
	wantAccessAnalyzerError(t, err, "ConflictException")
	del := &sdk.DeleteClusterPolicyInput{
		Identifier:            made.Identifier,
		ExpectedPolicyVersion: second.PolicyVersion,
		ClientToken:           new("delete-policy"),
	}
	removed, err := c.DeleteClusterPolicy(ctx, del)
	if err != nil || *removed.PolicyVersion != *second.PolicyVersion {
		t.Fatalf("delete: %+v %v", removed, err)
	}

	_, err = c.DeleteClusterPolicy(ctx, del)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetClusterPolicy(ctx, &sdk.GetClusterPolicyInput{Identifier: made.Identifier})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.CreateCluster(ctx, &sdk.CreateClusterInput{Policy: new(policy), BypassPolicyLockoutSafetyCheck: true})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDSQLPeeringSQLiteRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	t.Cleanup(func() { stop() })
	east := dsqlClient(s, "us-east-1")
	ohio := dsqlClient(s, "us-east-2")
	first, err := east.CreateCluster(ctx, &sdk.CreateClusterInput{
		DeletionProtectionEnabled: new(false),
		ClientToken:               new("persistent"),
		MultiRegionProperties: &types.MultiRegionProperties{
			WitnessRegion: new("us-west-2"),
		},
		Tags: map[string]string{"restart": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}

	second, err := ohio.CreateCluster(ctx, &sdk.CreateClusterInput{
		DeletionProtectionEnabled: new(false),
		MultiRegionProperties: &types.MultiRegionProperties{
			WitnessRegion: new("us-west-2"),
			Clusters:      []string{*first.Arn},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	pollDSQL(t, east, first.Identifier, types.ClusterStatusPendingSetup)
	pollDSQL(t, ohio, second.Identifier, types.ClusterStatusPendingSetup)
	_, err = east.UpdateCluster(
		ctx,
		&sdk.UpdateClusterInput{Identifier: first.Identifier, MultiRegionProperties: &types.MultiRegionProperties{
			Clusters: []string{"arn:aws:dsql:us-east-2:000000000000:cluster/aaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = east.UpdateCluster(
		ctx,
		&sdk.UpdateClusterInput{Identifier: first.Identifier, MultiRegionProperties: &types.MultiRegionProperties{
			WitnessRegion: new("eu-west-1"), Clusters: []string{*second.Arn},
		}},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = east.UpdateCluster(
		ctx,
		&sdk.UpdateClusterInput{
			Identifier:            first.Identifier,
			MultiRegionProperties: &types.MultiRegionProperties{Clusters: []string{*second.Arn}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	east = dsqlClient(s, "us-east-1")
	ohio = dsqlClient(s, "us-east-2")
	live := pollDSQL(t, east, first.Identifier, types.ClusterStatusActive)
	pollDSQL(t, ohio, second.Identifier, types.ClusterStatusActive)
	if live.Tags["restart"] != "yes" || len(live.MultiRegionProperties.Clusters) != 2 {
		t.Fatalf("restart: %+v", live)
	}

	replay, err := east.CreateCluster(ctx, &sdk.CreateClusterInput{
		DeletionProtectionEnabled: new(false),
		ClientToken:               new("persistent"),
		MultiRegionProperties: &types.MultiRegionProperties{
			WitnessRegion: new("us-west-2"),
		},
		Tags: map[string]string{"restart": "yes"},
	})
	if err != nil || *replay.Identifier != *first.Identifier {
		t.Fatalf("persistent token: %+v %v", replay, err)
	}

	removed, err := east.DeleteCluster(ctx, &sdk.DeleteClusterInput{Identifier: first.Identifier})
	if err != nil || removed.Status != types.ClusterStatusPendingDelete {
		t.Fatalf("delete: %+v %v", removed, err)
	}

	pollDSQL(t, east, first.Identifier, types.ClusterStatusPendingDelete)
	_, err = ohio.DeleteCluster(ctx, &sdk.DeleteClusterInput{Identifier: second.Identifier})
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	pollDSQL(t, dsqlClient(s, "us-east-1"), first.Identifier, "")
	pollDSQL(t, dsqlClient(s, "us-east-2"), second.Identifier, "")
}

func TestDSQLCloudFormation(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c := cfn.New(cfn.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("test")})
	template := func(team string) *string {
		doc := map[string]any{
			"Resources": map[string]any{
				"Cluster": map[string]any{"Type": "AWS::DSQL::Cluster", "Properties": map[string]any{
					"DeletionProtectionEnabled": false,
					"KmsEncryptionKey":          "AWS_OWNED_KMS_KEY",
					"Tags":                      []map[string]string{{"Key": "team", "Value": team}},
				}},
			},
			"Outputs": map[string]any{
				"Identifier": map[string]any{"Value": map[string]string{"Ref": "Cluster"}},
				"Endpoint":   map[string]any{"Value": map[string]any{"Fn::GetAtt": []string{"Cluster", "Endpoint"}}},
			},
		}
		data, _ := json.Marshal(doc)
		return new(string(data))
	}
	_, err := c.CreateStack(ctx, &cfn.CreateStackInput{StackName: new("dsql"), TemplateBody: template("core")})
	if err != nil {
		t.Fatal(err)
	}

	stack := settled(ctx, t, c, "dsql")
	id := outputsOf(new(stack))["Identifier"]
	if id == "" {
		t.Fatalf("missing output: %+v", stack)
	}

	pollDSQL(t, dsqlClient(s, "us-east-1"), new(id), types.ClusterStatusActive)
	_, err = c.UpdateStack(ctx, &cfn.UpdateStackInput{StackName: new("dsql"), TemplateBody: template("platform")})
	if err != nil {
		t.Fatal(err)
	}

	settled(ctx, t, c, "dsql")
	live := pollDSQL(t, dsqlClient(s, "us-east-1"), new(id), types.ClusterStatusActive)
	if live.Tags["team"] != "platform" {
		t.Fatal(live.Tags)
	}

	_, err = c.DeleteStack(ctx, &cfn.DeleteStackInput{StackName: new("dsql")})
	if err != nil {
		t.Fatal(err)
	}

	pollDSQL(t, dsqlClient(s, "us-east-1"), new(id), "")
}
