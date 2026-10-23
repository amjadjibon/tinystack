package amp_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/amp"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func testContext(region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: "123456789012", Region: region}},
	)
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *awserror.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

func TestWorkspaceLifecycle(t *testing.T) {
	s := amp.New(memory.New(nil, nil))
	ctx := testContext("us-east-1")

	// Create
	ws, err := s.CreateWorkspace(ctx, &api.CreateWorkspaceRequest{
		Alias: new("test-ws"),
		Tags:  map[string]string{"env": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if ws.WorkspaceId == nil || *ws.WorkspaceId == "" {
		t.Fatal("expected non-empty workspace ID")
	}

	if *ws.Status.StatusCode != "ACTIVE" {
		t.Fatalf("want ACTIVE, got %s", *ws.Status.StatusCode)
	}

	wsID := *ws.WorkspaceId

	// Describe
	desc, err := s.DescribeWorkspace(ctx, &api.DescribeWorkspaceRequest{
		WorkspaceId: &wsID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if *desc.Workspace.Alias != "test-ws" || desc.Workspace.PrometheusEndpoint == nil {
		t.Fatalf("unexpected describe result: %+v", desc.Workspace)
	}

	// Update alias
	_, err = s.UpdateWorkspaceAlias(ctx, &api.UpdateWorkspaceAliasRequest{
		WorkspaceId: &wsID,
		Alias:       new("updated-ws"),
	})
	if err != nil {
		t.Fatal(err)
	}

	desc, err = s.DescribeWorkspace(ctx, &api.DescribeWorkspaceRequest{
		WorkspaceId: &wsID,
	})
	if err != nil || *desc.Workspace.Alias != "updated-ws" {
		t.Fatalf("alias not updated: %v", desc)
	}

	// List
	list, err := s.ListWorkspaces(ctx, &api.ListWorkspacesRequest{})
	if err != nil || len(list.Workspaces) != 1 {
		t.Fatalf("list failed: %v", list)
	}

	// Delete
	_, err = s.DeleteWorkspace(ctx, &api.DeleteWorkspaceRequest{
		WorkspaceId: &wsID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Describe after delete should be not found
	_, err = s.DescribeWorkspace(ctx, &api.DescribeWorkspaceRequest{
		WorkspaceId: &wsID,
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestRuleGroupsNamespaceLifecycle(t *testing.T) {
	s := amp.New(memory.New(nil, nil))
	ctx := testContext("us-east-1")

	ws, err := s.CreateWorkspace(ctx, &api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}

	wsID := *ws.WorkspaceId
	ruleYAML := []byte(`groups:
  - name: example
    rules:
      - record: job:http_inprogress_requests:sum
        expr: sum by (job) (http_inprogress_requests)
`)

	// Create with empty data should fail
	_, err = s.CreateRuleGroupsNamespace(ctx, &api.CreateRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
		Data:        []byte{},
	})
	wantCode(t, err, "ValidationException")

	// Create
	rg, err := s.CreateRuleGroupsNamespace(ctx, &api.CreateRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
		Data:        ruleYAML,
		Tags:        map[string]string{"type": "rules"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if *rg.Name != "rg-1" || *rg.Status.StatusCode != "ACTIVE" {
		t.Fatalf("unexpected rg: %+v", rg)
	}

	// Duplicate create should conflict
	_, err = s.CreateRuleGroupsNamespace(ctx, &api.CreateRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
		Data:        ruleYAML,
	})
	wantCode(t, err, "ConflictException")

	// Describe
	desc, err := s.DescribeRuleGroupsNamespace(ctx, &api.DescribeRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
	})
	if err != nil || !bytes.Equal(desc.RuleGroupsNamespace.Data, ruleYAML) {
		t.Fatalf("unexpected describe: %v", desc)
	}

	// Put
	newYAML := []byte("groups:\n  - name: updated\n    rules: []\n")
	put, err := s.PutRuleGroupsNamespace(ctx, &api.PutRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
		Data:        newYAML,
	})
	if err != nil || *put.Status.StatusCode != "ACTIVE" {
		t.Fatalf("put failed: %v", put)
	}

	desc, err = s.DescribeRuleGroupsNamespace(ctx, &api.DescribeRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
	})
	if err != nil || !bytes.Equal(desc.RuleGroupsNamespace.Data, newYAML) {
		t.Fatalf("data not updated: %v", desc)
	}

	// List
	list, err := s.ListRuleGroupsNamespaces(ctx, &api.ListRuleGroupsNamespacesRequest{
		WorkspaceId: &wsID,
		Name:        new("rg"),
	})
	if err != nil || len(list.RuleGroupsNamespaces) != 1 {
		t.Fatalf("list failed: %v", list)
	}

	// Delete
	_, err = s.DeleteRuleGroupsNamespace(ctx, &api.DeleteRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DescribeRuleGroupsNamespace(ctx, &api.DescribeRuleGroupsNamespaceRequest{
		WorkspaceId: &wsID,
		Name:        new("rg-1"),
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestAlertManagerDefinitionLifecycle(t *testing.T) {
	s := amp.New(memory.New(nil, nil))
	ctx := testContext("us-east-1")

	ws, err := s.CreateWorkspace(ctx, &api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}

	wsID := *ws.WorkspaceId
	amYAML := []byte("alertmanager_config: |-\n  global:\n    resolve_timeout: 5m\n")

	// Create
	created, err := s.CreateAlertManagerDefinition(ctx, &api.CreateAlertManagerDefinitionRequest{
		WorkspaceId: &wsID,
		Data:        amYAML,
	})
	if err != nil || *created.Status.StatusCode != "ACTIVE" {
		t.Fatalf("create failed: %v", created)
	}

	// Duplicate create should conflict
	_, err = s.CreateAlertManagerDefinition(ctx, &api.CreateAlertManagerDefinitionRequest{
		WorkspaceId: &wsID,
		Data:        amYAML,
	})
	wantCode(t, err, "ConflictException")

	// Describe
	desc, err := s.DescribeAlertManagerDefinition(ctx, &api.DescribeAlertManagerDefinitionRequest{
		WorkspaceId: &wsID,
	})
	if err != nil || !bytes.Equal(desc.AlertManagerDefinition.Data, amYAML) {
		t.Fatalf("describe failed: %v", desc)
	}

	// Put
	updatedYAML := []byte("alertmanager_config: |-\n  global:\n    resolve_timeout: 10m\n")
	put, err := s.PutAlertManagerDefinition(ctx, &api.PutAlertManagerDefinitionRequest{
		WorkspaceId: &wsID,
		Data:        updatedYAML,
	})
	if err != nil || *put.Status.StatusCode != "ACTIVE" {
		t.Fatalf("put failed: %v", put)
	}

	// Delete
	_, err = s.DeleteAlertManagerDefinition(ctx, &api.DeleteAlertManagerDefinitionRequest{
		WorkspaceId: &wsID,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DescribeAlertManagerDefinition(ctx, &api.DescribeAlertManagerDefinitionRequest{
		WorkspaceId: &wsID,
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestLoggingConfigurationLifecycle(t *testing.T) {
	s := amp.New(memory.New(nil, nil))
	ctx := testContext("us-east-1")

	ws, err := s.CreateWorkspace(ctx, &api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}

	wsID := *ws.WorkspaceId
	validArn := "arn:aws:logs:us-east-1:123456789012:log-group:my-amp-logs:*"
	invalidArn := "arn:aws:s3:::my-bucket"

	// Invalid ARN should fail
	_, err = s.CreateLoggingConfiguration(ctx, &api.CreateLoggingConfigurationRequest{
		WorkspaceId: &wsID,
		LogGroupArn: &invalidArn,
	})
	wantCode(t, err, "ValidationException")

	// Create
	created, err := s.CreateLoggingConfiguration(ctx, &api.CreateLoggingConfigurationRequest{
		WorkspaceId: &wsID,
		LogGroupArn: &validArn,
	})
	if err != nil || *created.Status.StatusCode != "ACTIVE" {
		t.Fatalf("create failed: %v", created)
	}

	// Describe
	desc, err := s.DescribeLoggingConfiguration(ctx, &api.DescribeLoggingConfigurationRequest{
		WorkspaceId: &wsID,
	})
	if err != nil || *desc.LoggingConfiguration.LogGroupArn != validArn {
		t.Fatalf("describe failed: %v", desc)
	}

	// Update
	newArn := "arn:aws:logs:us-east-1:123456789012:log-group:my-other-logs:*"
	updated, err := s.UpdateLoggingConfiguration(ctx, &api.UpdateLoggingConfigurationRequest{
		WorkspaceId: &wsID,
		LogGroupArn: &newArn,
	})
	if err != nil || *updated.Status.StatusCode != "ACTIVE" {
		t.Fatalf("update failed: %v", updated)
	}

	// Delete
	_, err = s.DeleteLoggingConfiguration(ctx, &api.DeleteLoggingConfigurationRequest{
		WorkspaceId: &wsID,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.DescribeLoggingConfiguration(ctx, &api.DescribeLoggingConfigurationRequest{
		WorkspaceId: &wsID,
	})
	wantCode(t, err, "ResourceNotFoundException")
}

func TestTags(t *testing.T) {
	s := amp.New(memory.New(nil, nil))
	ctx := testContext("us-east-1")

	ws, err := s.CreateWorkspace(ctx, &api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}

	wsArn := *ws.Arn
	_, err = s.TagResource(ctx, &api.TagResourceRequest{
		ResourceArn: &wsArn,
		Tags:        map[string]string{"env": "staging", "owner": "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err := s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{
		ResourceArn: &wsArn,
	})
	if err != nil || len(tags.Tags) != 2 || tags.Tags["env"] != "staging" {
		t.Fatalf("unexpected tags: %v", tags)
	}

	_, err = s.UntagResource(ctx, &api.UntagResourceRequest{
		ResourceArn: &wsArn,
		TagKeys:     []string{"owner"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tags, err = s.ListTagsForResource(ctx, &api.ListTagsForResourceRequest{
		ResourceArn: &wsArn,
	})
	if err != nil || len(tags.Tags) != 1 || tags.Tags["owner"] != "" {
		t.Fatalf("unexpected tags after untag: %v", tags)
	}
}

func TestInventory(t *testing.T) {
	s := amp.New(memory.New(nil, nil))
	ctx := testContext("us-west-2")

	ws, err := s.CreateWorkspace(ctx, &api.CreateWorkspaceRequest{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.CreateRuleGroupsNamespace(ctx, &api.CreateRuleGroupsNamespaceRequest{
		WorkspaceId: ws.WorkspaceId,
		Name:        new("rg-inv"),
		Data:        []byte("groups: []\n"),
	})
	if err != nil {
		t.Fatal(err)
	}

	resources, err := s.Resources(ctx)
	if err != nil || len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d (err: %v)", len(resources), err)
	}
}
