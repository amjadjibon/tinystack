package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/amp"
	"github.com/aws/aws-sdk-go-v2/service/amp/types"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/smithy-go"
)

func ampClient(s *httptest.Server) *sdk.Client {
	return sdk.New(
		sdk.Options{
			Region:       "us-east-1",
			BaseEndpoint: new(s.URL),
			Credentials:  creds("test"),
			Retryer:      aws.NopRetryer{},
		},
	)
}

func wantAMPError(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("want error code %s, got %v", code, err)
	}
}

func TestAMPLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := ampClient(s)

	// 1. Workspaces
	ws, err := c.CreateWorkspace(ctx, &sdk.CreateWorkspaceInput{
		Alias: aws.String("my-workspace"),
		Tags:  map[string]string{"env": "test", "project": "monitoring"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace failed: %v", err)
	}

	wsID := aws.ToString(ws.WorkspaceId)
	wsArn := aws.ToString(ws.Arn)
	if wsID == "" || wsArn == "" || ws.Status.StatusCode != types.WorkspaceStatusCodeActive {
		t.Fatalf("unexpected CreateWorkspace response: %+v", ws)
	}

	// Describe Workspace
	desc, err := c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil {
		t.Fatalf("DescribeWorkspace failed: %v", err)
	}

	if aws.ToString(desc.Workspace.Alias) != "my-workspace" ||
		desc.Workspace.PrometheusEndpoint == nil ||
		!strings.Contains(aws.ToString(desc.Workspace.PrometheusEndpoint), wsID) {
		t.Fatalf("unexpected DescribeWorkspace response: %+v", desc.Workspace)
	}

	// Update Workspace Alias
	_, err = c.UpdateWorkspaceAlias(ctx, &sdk.UpdateWorkspaceAliasInput{
		WorkspaceId: ws.WorkspaceId,
		Alias:       aws.String("my-updated-workspace"),
	})
	if err != nil {
		t.Fatalf("UpdateWorkspaceAlias failed: %v", err)
	}

	desc, err = c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil || aws.ToString(desc.Workspace.Alias) != "my-updated-workspace" {
		t.Fatalf("alias not updated: %v, %v", desc, err)
	}

	// List Workspaces with alias filter
	listed, err := c.ListWorkspaces(ctx, &sdk.ListWorkspacesInput{
		Alias: aws.String("my-updated-workspace"),
	})
	if err != nil || len(listed.Workspaces) != 1 {
		t.Fatalf("ListWorkspaces failed: %v, %v", listed, err)
	}

	// 2. Rule Groups Namespaces
	ruleYAML := "groups:\n" +
		"  - name: example\n" +
		"    rules:\n" +
		"      - record: job:http_requests:sum\n" +
		"        expr: sum by (job) (http_requests)\n"
	rg, err := c.CreateRuleGroupsNamespace(ctx, &sdk.CreateRuleGroupsNamespaceInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("my-rules"),
		Data:        []byte(ruleYAML),
		Tags:        map[string]string{"ruleset": "core"},
	})
	if err != nil {
		t.Fatalf("CreateRuleGroupsNamespace failed: %v", err)
	}

	rgArn := aws.ToString(rg.Arn)
	if aws.ToString(rg.Name) != "my-rules" || rg.Status.StatusCode != types.RuleGroupsNamespaceStatusCodeActive {
		t.Fatalf("unexpected CreateRuleGroupsNamespace response: %+v", rg)
	}

	// Describe Rule Groups Namespace
	rgDesc, err := c.DescribeRuleGroupsNamespace(ctx, &sdk.DescribeRuleGroupsNamespaceInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("my-rules"),
	})
	if err != nil || string(rgDesc.RuleGroupsNamespace.Data) != ruleYAML {
		t.Fatalf("DescribeRuleGroupsNamespace failed: %v, %v", rgDesc, err)
	}

	// Put Rule Groups Namespace (update data)
	updatedRuleYAML := "groups:\n  - name: example-updated\n    rules: []\n"
	_, err = c.PutRuleGroupsNamespace(ctx, &sdk.PutRuleGroupsNamespaceInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("my-rules"),
		Data:        []byte(updatedRuleYAML),
	})
	if err != nil {
		t.Fatalf("PutRuleGroupsNamespace failed: %v", err)
	}

	rgDesc, err = c.DescribeRuleGroupsNamespace(ctx, &sdk.DescribeRuleGroupsNamespaceInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("my-rules"),
	})
	if err != nil || string(rgDesc.RuleGroupsNamespace.Data) != updatedRuleYAML {
		t.Fatalf("rule group data not updated: %v, %v", rgDesc, err)
	}

	// List Rule Groups Namespaces with name prefix filter
	rgList, err := c.ListRuleGroupsNamespaces(ctx, &sdk.ListRuleGroupsNamespacesInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("my-"),
	})
	if err != nil || len(rgList.RuleGroupsNamespaces) != 1 {
		t.Fatalf("ListRuleGroupsNamespaces failed: %v, %v", rgList, err)
	}

	// 3. Alert Manager Definition
	amYAML := "alertmanager_config: |-\n  global:\n    resolve_timeout: 5m\n"
	_, err = c.CreateAlertManagerDefinition(ctx, &sdk.CreateAlertManagerDefinitionInput{
		WorkspaceId: ws.WorkspaceId,
		Data:        []byte(amYAML),
	})
	if err != nil {
		t.Fatalf("CreateAlertManagerDefinition failed: %v", err)
	}

	// Describe Alert Manager Definition
	amDesc, err := c.DescribeAlertManagerDefinition(ctx, &sdk.DescribeAlertManagerDefinitionInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil || string(amDesc.AlertManagerDefinition.Data) != amYAML {
		t.Fatalf("DescribeAlertManagerDefinition failed: %v, %v", amDesc, err)
	}

	// Put Alert Manager Definition
	updatedAMYAML := "alertmanager_config: |-\n  global:\n    resolve_timeout: 10m\n"
	_, err = c.PutAlertManagerDefinition(ctx, &sdk.PutAlertManagerDefinitionInput{
		WorkspaceId: ws.WorkspaceId,
		Data:        []byte(updatedAMYAML),
	})
	if err != nil {
		t.Fatalf("PutAlertManagerDefinition failed: %v", err)
	}

	amDesc, err = c.DescribeAlertManagerDefinition(ctx, &sdk.DescribeAlertManagerDefinitionInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil || string(amDesc.AlertManagerDefinition.Data) != updatedAMYAML {
		t.Fatalf("alertmanager definition data not updated: %v, %v", amDesc, err)
	}

	// 4. Logging Configuration
	logGroupArn := "arn:aws:logs:us-east-1:123456789012:log-group:my-amp-logs:*"
	_, err = c.CreateLoggingConfiguration(ctx, &sdk.CreateLoggingConfigurationInput{
		WorkspaceId: ws.WorkspaceId,
		LogGroupArn: aws.String(logGroupArn),
	})
	if err != nil {
		t.Fatalf("CreateLoggingConfiguration failed: %v", err)
	}

	// Describe Logging Configuration
	logDesc, err := c.DescribeLoggingConfiguration(ctx, &sdk.DescribeLoggingConfigurationInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil || aws.ToString(logDesc.LoggingConfiguration.LogGroupArn) != logGroupArn {
		t.Fatalf("DescribeLoggingConfiguration failed: %v, %v", logDesc, err)
	}

	// Update Logging Configuration
	updatedLogGroupArn := "arn:aws:logs:us-east-1:123456789012:log-group:my-amp-logs-2:*"
	_, err = c.UpdateLoggingConfiguration(ctx, &sdk.UpdateLoggingConfigurationInput{
		WorkspaceId: ws.WorkspaceId,
		LogGroupArn: aws.String(updatedLogGroupArn),
	})
	if err != nil {
		t.Fatalf("UpdateLoggingConfiguration failed: %v", err)
	}

	logDesc, err = c.DescribeLoggingConfiguration(ctx, &sdk.DescribeLoggingConfigurationInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil || aws.ToString(logDesc.LoggingConfiguration.LogGroupArn) != updatedLogGroupArn {
		t.Fatalf("logging configuration not updated: %v, %v", logDesc, err)
	}

	// 5. Native Tagging
	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: ws.Arn,
		Tags:        map[string]string{"cost-center": "1234"},
	})
	if err != nil {
		t.Fatalf("TagResource failed: %v", err)
	}

	tagRes, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: ws.Arn,
	})
	if err != nil || tagRes.Tags["cost-center"] != "1234" {
		t.Fatalf("ListTagsForResource failed: %v, %v", tagRes, err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceArn: ws.Arn,
		TagKeys:     []string{"cost-center"},
	})
	if err != nil {
		t.Fatalf("UntagResource failed: %v", err)
	}

	tagRes, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: ws.Arn,
	})
	if err != nil || tagRes.Tags["cost-center"] != "" {
		t.Fatalf("UntagResource did not remove tag: %v", tagRes)
	}

	// 6. RGTA Integration
	tagging := rgta.New(rgta.Options{
		Region:       "us-east-1",
		BaseEndpoint: new(s.URL),
		Credentials:  creds("test"),
		Retryer:      aws.NopRetryer{},
	})

	tagged, err := tagging.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{wsArn, rgArn},
		Tags:            map[string]string{"rgta-tag": "active"},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("RGTA TagResources failed: %v, %v", tagged, err)
	}

	res, err := tagging.GetResources(ctx, &rgta.GetResourcesInput{
		ResourceTypeFilters: []string{"aps:workspace", "aps:rulegroupsnamespace"},
	})
	if err != nil || len(res.ResourceTagMappingList) < 2 {
		t.Fatalf("RGTA GetResources failed: %v, %v", res, err)
	}

	// 7. Error Cases & Validation
	// Empty rule data
	_, err = c.CreateRuleGroupsNamespace(ctx, &sdk.CreateRuleGroupsNamespaceInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("empty-rules"),
		Data:        []byte{},
	})
	wantAMPError(t, err, "ValidationException")

	// Invalid LogGroupArn
	_, err = c.CreateLoggingConfiguration(ctx, &sdk.CreateLoggingConfigurationInput{
		WorkspaceId: ws.WorkspaceId,
		LogGroupArn: aws.String("invalid-arn"),
	})
	wantAMPError(t, err, "ValidationException")

	// Non-existent workspace describe
	_, err = c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: aws.String("ws-non-existent"),
	})
	wantAMPError(t, err, "ResourceNotFoundException")

	// 8. Cleanup and cascade
	_, err = c.DeleteLoggingConfiguration(ctx, &sdk.DeleteLoggingConfigurationInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil {
		t.Fatalf("DeleteLoggingConfiguration failed: %v", err)
	}

	_, err = c.DeleteAlertManagerDefinition(ctx, &sdk.DeleteAlertManagerDefinitionInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil {
		t.Fatalf("DeleteAlertManagerDefinition failed: %v", err)
	}

	_, err = c.DeleteRuleGroupsNamespace(ctx, &sdk.DeleteRuleGroupsNamespaceInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("my-rules"),
	})
	if err != nil {
		t.Fatalf("DeleteRuleGroupsNamespace failed: %v", err)
	}

	_, err = c.DeleteWorkspace(ctx, &sdk.DeleteWorkspaceInput{
		WorkspaceId: ws.WorkspaceId,
	})
	if err != nil {
		t.Fatalf("DeleteWorkspace failed: %v", err)
	}

	// Workspace should now be gone
	_, err = c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: ws.WorkspaceId,
	})
	wantAMPError(t, err, "ResourceNotFoundException")
}

func TestAMPCloudFormation(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	c := ampClient(s)

	tmpl := `
AWSTemplateFormatVersion: '2010-09-09'
Resources:
  MyWorkspace:
    Type: AWS::APS::Workspace
    Properties:
      Alias: cfn-ws-alias
      Tags:
        - Key: env
          Value: dev

  MyRules:
    Type: AWS::APS::RuleGroupsNamespace
    Properties:
      Workspace: !Ref MyWorkspace
      Name: cfn-rules
      Data: |
        groups:
          - name: cfn-group
            rules: []
      Tags:
        - Key: tier
          Value: backend

Outputs:
  WorkspaceArn:
    Value: !Ref MyWorkspace
  WorkspaceId:
    Value: !GetAtt MyWorkspace.WorkspaceId
  Endpoint:
    Value: !GetAtt MyWorkspace.PrometheusEndpoint
  RulesArn:
    Value: !Ref MyRules
`

	_, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    aws.String("amp-stack"),
		TemplateBody: aws.String(tmpl),
	})
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	st := wantStatus(ctx, t, cf, "amp-stack", cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&st)

	wsArn := outputs["WorkspaceArn"]
	wsID := outputs["WorkspaceId"]
	endpoint := outputs["Endpoint"]
	rulesArn := outputs["RulesArn"]

	if wsArn == "" || wsID == "" || endpoint == "" || rulesArn == "" {
		t.Fatalf("missing expected outputs: %+v", outputs)
	}

	// Verify via AMP SDK
	wsDesc, err := c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: aws.String(wsID),
	})
	if err != nil || aws.ToString(wsDesc.Workspace.Alias) != "cfn-ws-alias" {
		t.Fatalf("DescribeWorkspace via SDK failed: %v, %v", wsDesc, err)
	}

	rgDesc, err := c.DescribeRuleGroupsNamespace(ctx, &sdk.DescribeRuleGroupsNamespaceInput{
		WorkspaceId: aws.String(wsID),
		Name:        aws.String("cfn-rules"),
	})
	if err != nil || !strings.Contains(string(rgDesc.RuleGroupsNamespace.Data), "cfn-group") {
		t.Fatalf("DescribeRuleGroupsNamespace via SDK failed: %v, %v", rgDesc, err)
	}

	// Update Stack (update alias, rule data, and tags)
	updateTmpl := `
AWSTemplateFormatVersion: '2010-09-09'
Resources:
  MyWorkspace:
    Type: AWS::APS::Workspace
    Properties:
      Alias: cfn-ws-alias-updated
      Tags:
        - Key: env
          Value: prod

  MyRules:
    Type: AWS::APS::RuleGroupsNamespace
    Properties:
      Workspace: !Ref MyWorkspace
      Name: cfn-rules
      Data: |
        groups:
          - name: cfn-group-updated
            rules: []
      Tags:
        - Key: tier
          Value: prod

Outputs:
  WorkspaceArn:
    Value: !Ref MyWorkspace
  WorkspaceId:
    Value: !GetAtt MyWorkspace.WorkspaceId
  Endpoint:
    Value: !GetAtt MyWorkspace.PrometheusEndpoint
  RulesArn:
    Value: !Ref MyRules
`

	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    aws.String("amp-stack"),
		TemplateBody: aws.String(updateTmpl),
	})
	if err != nil {
		t.Fatalf("UpdateStack failed: %v", err)
	}

	wantStatus(ctx, t, cf, "amp-stack", cfntypes.StackStatusUpdateComplete)

	// Verify updates
	wsDesc, err = c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: aws.String(wsID),
	})
	if err != nil || aws.ToString(wsDesc.Workspace.Alias) != "cfn-ws-alias-updated" {
		t.Fatalf("workspace alias was not updated: %v, %v", wsDesc, err)
	}

	rgDesc, err = c.DescribeRuleGroupsNamespace(ctx, &sdk.DescribeRuleGroupsNamespaceInput{
		WorkspaceId: aws.String(wsID),
		Name:        aws.String("cfn-rules"),
	})
	if err != nil || !strings.Contains(string(rgDesc.RuleGroupsNamespace.Data), "cfn-group-updated") {
		t.Fatalf("rule groups namespace data was not updated: %v, %v", rgDesc, err)
	}

	// Delete Stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: aws.String("amp-stack"),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}

	wantStatus(ctx, t, cf, *st.StackId, cfntypes.StackStatusDeleteComplete)

	// Verify resources deleted
	_, err = c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: aws.String(wsID),
	})
	wantAMPError(t, err, "ResourceNotFoundException")
}

func TestAMPPersistence(t *testing.T) {
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})

	ctx := context.Background()
	c := ampClient(s)

	// Create workspace and rule group
	ws, err := c.CreateWorkspace(ctx, &sdk.CreateWorkspaceInput{
		Alias: aws.String("persisted-ws"),
		Tags:  map[string]string{"persisted": "true"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace failed: %v", err)
	}

	wsID := aws.ToString(ws.WorkspaceId)
	ruleYAML := "groups:\n  - name: persist\n    rules: []\n"
	_, err = c.CreateRuleGroupsNamespace(ctx, &sdk.CreateRuleGroupsNamespaceInput{
		WorkspaceId: ws.WorkspaceId,
		Name:        aws.String("persisted-rules"),
		Data:        []byte(ruleYAML),
	})
	if err != nil {
		t.Fatalf("CreateRuleGroupsNamespace failed: %v", err)
	}

	// Restart server
	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()

	c = ampClient(s)

	// Verify workspace persisted
	wsDesc, err := c.DescribeWorkspace(ctx, &sdk.DescribeWorkspaceInput{
		WorkspaceId: aws.String(wsID),
	})
	if err != nil || aws.ToString(wsDesc.Workspace.Alias) != "persisted-ws" {
		t.Fatalf("DescribeWorkspace after restart failed: %v, %v", wsDesc, err)
	}

	// Verify rule groups namespace persisted
	rgDesc, err := c.DescribeRuleGroupsNamespace(ctx, &sdk.DescribeRuleGroupsNamespaceInput{
		WorkspaceId: aws.String(wsID),
		Name:        aws.String("persisted-rules"),
	})
	if err != nil || string(rgDesc.RuleGroupsNamespace.Data) != ruleYAML {
		t.Fatalf("DescribeRuleGroupsNamespace after restart failed: %v, %v", rgDesc, err)
	}
}
