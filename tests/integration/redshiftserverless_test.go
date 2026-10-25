package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/service/redshiftserverless"
	"github.com/aws/aws-sdk-go-v2/service/redshiftserverless/types"
)

func serverlessClient(url string) *sdk.Client {
	return sdk.New(sdk.Options{Region: "us-east-1", BaseEndpoint: new(url), Credentials: creds("test")})
}

func waitServerless(t *testing.T, c *sdk.Client, name *string) *types.Workgroup {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.GetWorkgroup(context.Background(), &sdk.GetWorkgroupInput{WorkgroupName: name})
		if err != nil {
			t.Fatal(err)
		}

		if out.Workgroup.Status == types.WorkgroupStatusAvailable {
			return out.Workgroup
		}

		time.Sleep(25 * time.Millisecond)
	}

	t.Fatal("workgroup did not become available")
	return nil
}

func createServerless(t *testing.T, url, name string) (*sdk.Client, *types.Workgroup) {
	t.Helper()
	ctx := context.Background()
	c := serverlessClient(url)
	_, err := c.CreateNamespace(
		ctx,
		&sdk.CreateNamespaceInput{
			NamespaceName:     new(name),
			AdminUsername:     new("admin"),
			AdminUserPassword: new("Password123!"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.CreateWorkgroup(ctx, &sdk.CreateWorkgroupInput{NamespaceName: new(name), WorkgroupName: new(name)})
	if err != nil {
		t.Fatal(err)
	}

	return c, waitServerless(t, c, out.Workgroup.WorkgroupName)
}

func TestRedshiftServerlessOperations(t *testing.T) {
	server := server(t)
	ctx := context.Background()
	c, w := createServerless(t, server.URL, "warehouse")
	n, err := c.GetNamespace(ctx, &sdk.GetNamespaceInput{NamespaceName: new("warehouse")})
	if err != nil || *n.Namespace.DbName != "dev" {
		t.Fatal(n, err)
	}

	_, err = c.UpdateNamespace(
		ctx,
		&sdk.UpdateNamespaceInput{
			NamespaceName: new("warehouse"),
			LogExports:    []types.LogExport{types.LogExportUserLog},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateNamespace(ctx, &sdk.CreateNamespaceInput{NamespaceName: new("another")})
	if err != nil {
		t.Fatal(err)
	}

	page, err := c.ListNamespaces(ctx, &sdk.ListNamespacesInput{MaxResults: new(int32(1))})
	if err != nil || len(page.Namespaces) != 1 || page.NextToken == nil {
		t.Fatal(page, err)
	}

	next, err := c.ListNamespaces(ctx, &sdk.ListNamespacesInput{MaxResults: new(int32(1)), NextToken: page.NextToken})
	if err != nil || len(next.Namespaces) != 1 {
		t.Fatal(next, err)
	}

	_, err = c.ListWorkgroups(ctx, &sdk.ListWorkgroupsInput{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateWorkgroup(ctx, &sdk.UpdateWorkgroupInput{WorkgroupName: w.WorkgroupName, Port: new(int32(5440))})
	if err != nil {
		t.Fatal(err)
	}

	w = waitServerless(t, c, w.WorkgroupName)
	if *w.Endpoint.Port != 5440 {
		t.Fatal(w)
	}

	for _, a := range []*string{n.Namespace.NamespaceArn, w.WorkgroupArn} {
		_, err = c.TagResource(
			ctx,
			&sdk.TagResourceInput{ResourceArn: a, Tags: []types.Tag{{Key: new("env"), Value: new("test")}}},
		)
		if err != nil {
			t.Fatal(err)
		}

		tags, e := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceArn: a})
		if e != nil || len(tags.Tags) != 1 {
			t.Fatal(tags, e)
		}

		_, e = c.UntagResource(ctx, &sdk.UntagResourceInput{ResourceArn: a, TagKeys: []string{"env"}})
		if e != nil {
			t.Fatal(e)
		}
	}

	snap, err := c.CreateSnapshot(
		ctx,
		&sdk.CreateSnapshotInput{
			NamespaceName:   new("warehouse"),
			SnapshotName:    new("backup"),
			RetentionPeriod: new(int32(7)),
			Tags:            []types.Tag{{Key: new("env"), Value: new("test")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetSnapshot(ctx, &sdk.GetSnapshotInput{SnapshotArn: snap.Snapshot.SnapshotArn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListSnapshots(ctx, &sdk.ListSnapshotsInput{NamespaceName: new("warehouse")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateSnapshot(
		ctx,
		&sdk.UpdateSnapshotInput{SnapshotName: new("backup"), RetentionPeriod: new(int32(14))},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.RestoreFromSnapshot(
		ctx,
		&sdk.RestoreFromSnapshotInput{
			NamespaceName: new("warehouse"),
			WorkgroupName: w.WorkgroupName,
			SnapshotName:  new("backup"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	limit, err := c.CreateUsageLimit(
		ctx,
		&sdk.CreateUsageLimitInput{
			ResourceArn: w.WorkgroupArn,
			UsageType:   types.UsageLimitUsageTypeServerlessCompute,
			Amount:      new(int64(10)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetUsageLimit(ctx, &sdk.GetUsageLimitInput{UsageLimitId: limit.UsageLimit.UsageLimitId})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListUsageLimits(ctx, &sdk.ListUsageLimitsInput{ResourceArn: w.WorkgroupArn})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateUsageLimit(
		ctx,
		&sdk.UpdateUsageLimitInput{UsageLimitId: limit.UsageLimit.UsageLimitId, Amount: new(int64(20))},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteUsageLimit(ctx, &sdk.DeleteUsageLimitInput{UsageLimitId: limit.UsageLimit.UsageLimitId})
	if err != nil {
		t.Fatal(err)
	}

	endpoint, err := c.CreateEndpointAccess(
		ctx,
		&sdk.CreateEndpointAccessInput{
			EndpointName:        new("private"),
			WorkgroupName:       w.WorkgroupName,
			SubnetIds:           w.SubnetIds,
			VpcSecurityGroupIds: w.SecurityGroupIds,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetEndpointAccess(ctx, &sdk.GetEndpointAccessInput{EndpointName: endpoint.Endpoint.EndpointName})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.ListEndpointAccess(ctx, &sdk.ListEndpointAccessInput{WorkgroupName: w.WorkgroupName})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.UpdateEndpointAccess(
		ctx,
		&sdk.UpdateEndpointAccessInput{
			EndpointName:        endpoint.Endpoint.EndpointName,
			VpcSecurityGroupIds: w.SecurityGroupIds,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteEndpointAccess(ctx, &sdk.DeleteEndpointAccessInput{EndpointName: endpoint.Endpoint.EndpointName})
	if err != nil {
		t.Fatal(err)
	}

	credentials, err := c.GetCredentials(
		ctx,
		&sdk.GetCredentialsInput{WorkgroupName: w.WorkgroupName, DurationSeconds: new(int32(900))},
	)
	if err != nil || credentials.Expiration == nil || *credentials.DbPassword == "" {
		t.Fatal(credentials, err)
	}

	_, err = c.DeleteNamespace(ctx, &sdk.DeleteNamespaceInput{NamespaceName: new("warehouse")})
	wantAccessAnalyzerError(t, err, "ConflictException")
	_, err = c.CreateWorkgroup(
		ctx,
		&sdk.CreateWorkgroupInput{NamespaceName: new("missing"), WorkgroupName: new("missing")},
	)
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	_, err = c.CreateWorkgroup(
		ctx,
		&sdk.CreateWorkgroupInput{
			NamespaceName: new("another"),
			WorkgroupName: new("badsubnet"),
			SubnetIds:     []string{"subnet-deadbeef"},
		},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.CreateNamespace(
		ctx,
		&sdk.CreateNamespaceInput{
			NamespaceName: new("badrole"),
			IamRoles:      []string{"arn:aws:iam::000000000000:role/missing"},
		},
	)
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.CreateNamespace(ctx, &sdk.CreateNamespaceInput{NamespaceName: new("badkey"), KmsKeyId: new("missing")})
	wantAccessAnalyzerError(t, err, "ValidationException")
	_, err = c.DeleteSnapshot(ctx, &sdk.DeleteSnapshotInput{SnapshotName: new("backup")})
	if err != nil {
		t.Fatal(err)
	}

	// The Terraform provider recognizes a deleted snapshot by "snapshot" in the not-found message.
	_, err = c.GetSnapshot(ctx, &sdk.GetSnapshotInput{SnapshotName: new("backup")})
	wantAccessAnalyzerError(t, err, "ResourceNotFoundException")
	if !strings.Contains(err.Error(), "snapshot backup not found") {
		t.Fatal(err)
	}

	_, err = c.DeleteWorkgroup(ctx, &sdk.DeleteWorkgroupInput{WorkgroupName: w.WorkgroupName})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteNamespace(
		ctx,
		&sdk.DeleteNamespaceInput{NamespaceName: new("warehouse"), FinalSnapshotName: new("final")},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRedshiftServerlessPersistent(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c, w := createServerless(t, s.URL, "persistent")
	_, err := c.CreateSnapshot(
		context.Background(),
		&sdk.CreateSnapshotInput{NamespaceName: new("persistent"), SnapshotName: new("persistent")},
	)
	if err != nil {
		t.Fatal(err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = serverlessClient(s.URL)
	waitServerless(t, c, w.WorkgroupName)
	_, err = c.GetSnapshot(context.Background(), &sdk.GetSnapshotInput{SnapshotName: new("persistent")})
	if err != nil {
		t.Fatal(err)
	}
}
