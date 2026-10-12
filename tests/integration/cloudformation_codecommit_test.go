package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfn "github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	sdk "github.com/aws/aws-sdk-go-v2/service/codecommit"
)

func codecommitCfnTemplate(
	name, description, kmsKeyId string,
	tags map[string]string,
	code, triggers any,
) string {
	props := map[string]any{
		"RepositoryName": name,
	}

	if description != "" {
		props["RepositoryDescription"] = description
	}

	if kmsKeyId != "" {
		props["KmsKeyId"] = kmsKeyId
	}

	if len(tags) > 0 {
		var tagList []map[string]string
		for k, v := range tags {
			tagList = append(tagList, map[string]string{"Key": k, "Value": v})
		}

		props["Tags"] = tagList
	}

	if code != nil {
		props["Code"] = code
	}

	if triggers != nil {
		props["Triggers"] = triggers
	}

	tmpl := map[string]any{
		"Resources": map[string]any{
			"Repository": map[string]any{
				"Type":       "AWS::CodeCommit::Repository",
				"Properties": props,
			},
		},
		"Outputs": map[string]any{
			"RepoId": map[string]any{
				"Value": map[string]any{"Ref": "Repository"},
			},
			"Arn": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []string{"Repository", "Arn"}},
			},
			"CloneUrlHttp": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []string{"Repository", "CloneUrlHttp"}},
			},
			"CloneUrlSsh": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []string{"Repository", "CloneUrlSsh"}},
			},
			"Name": map[string]any{
				"Value": map[string]any{"Fn::GetAtt": []string{"Repository", "Name"}},
			},
		},
	}

	b, _ := json.Marshal(tmpl)
	return string(b)
}

func TestCloudFormationCodeCommitLifecycleAndRollback(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	cf := cfnClient(s)
	cc := codecommitClient(s, "test", "us-east-1")

	stackName := "cf-codecommit-stack"

	// 1. Create stack
	initTemplate := codecommitCfnTemplate(
		"cf-repo",
		"Initial description",
		"arn:aws:kms:us-east-1:000000000000:key/cf-key",
		map[string]string{"Env": "Test", "Dept": "Eng"},
		nil,
		nil,
	)

	_, err := cf.CreateStack(ctx, &cfn.CreateStackInput{
		StackName:    new(stackName),
		TemplateBody: new(initTemplate),
	})
	if err != nil {
		t.Fatalf("CreateStack failed: %v", err)
	}

	createdStack := wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusCreateComplete)
	outputs := outputsOf(&createdStack)

	repoId := outputs["RepoId"]
	if !uuidPattern.MatchString(repoId) {
		t.Errorf("RepoId output is not UUID v4: %s", repoId)
	}

	if outputs["Name"] != "cf-repo" {
		t.Errorf("Name output mismatch: %s", outputs["Name"])
	}

	if !strings.HasPrefix(outputs["Arn"], "arn:aws:codecommit:us-east-1:") {
		t.Errorf("Arn output mismatch: %s", outputs["Arn"])
	}

	if !strings.HasSuffix(outputs["CloneUrlHttp"], "/v1/repos/cf-repo") {
		t.Errorf("CloneUrlHttp mismatch: %s", outputs["CloneUrlHttp"])
	}

	if !strings.HasSuffix(outputs["CloneUrlSsh"], "/v1/repos/cf-repo") {
		t.Errorf("CloneUrlSsh mismatch: %s", outputs["CloneUrlSsh"])
	}

	// Verify live repository in CodeCommit
	repoGet, err := cc.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("cf-repo"),
	})
	if err != nil {
		t.Fatalf("GetRepository failed: %v", err)
	}

	if aws.ToString(repoGet.RepositoryMetadata.RepositoryId) != repoId {
		t.Errorf("RepositoryId mismatch: %s vs %s", aws.ToString(repoGet.RepositoryMetadata.RepositoryId), repoId)
	}

	if aws.ToString(repoGet.RepositoryMetadata.RepositoryDescription) != "Initial description" {
		t.Errorf("RepositoryDescription mismatch: %s", aws.ToString(repoGet.RepositoryMetadata.RepositoryDescription))
	}

	tagsGet, err := cc.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: repoGet.RepositoryMetadata.Arn,
	})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}

	if tagsGet.Tags["Env"] != "Test" || tagsGet.Tags["Dept"] != "Eng" {
		t.Errorf("tags mismatch: %v", tagsGet.Tags)
	}

	// 2. In-place update: Rename repository, change description, update tags
	updatedTemplate := codecommitCfnTemplate(
		"cf-repo-renamed",
		"Updated description",
		"arn:aws:kms:us-east-1:000000000000:key/cf-key",
		map[string]string{"Env": "Prod", "Team": "DevOps"},
		nil,
		nil,
	)

	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    new(stackName),
		TemplateBody: new(updatedTemplate),
	})
	if err != nil {
		t.Fatalf("UpdateStack failed: %v", err)
	}

	updatedStack := wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusUpdateComplete)
	updatedOutputs := outputsOf(&updatedStack)

	if updatedOutputs["RepoId"] != repoId {
		t.Errorf("RepoId changed after rename update: %s vs %s", updatedOutputs["RepoId"], repoId)
	}

	if updatedOutputs["Name"] != "cf-repo-renamed" {
		t.Errorf("Name output mismatch after rename: %s", updatedOutputs["Name"])
	}

	if !strings.HasSuffix(updatedOutputs["CloneUrlHttp"], "/v1/repos/cf-repo-renamed") {
		t.Errorf("CloneUrlHttp mismatch after rename: %s", updatedOutputs["CloneUrlHttp"])
	}

	// Old name must not exist
	_, err = cc.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("cf-repo"),
	})
	if err == nil {
		t.Errorf("expected old repository name cf-repo to be gone")
	}

	// New name must exist with same repository ID, updated description, and updated tags
	renamedGet, err := cc.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("cf-repo-renamed"),
	})
	if err != nil {
		t.Fatalf("GetRepository on renamed repo failed: %v", err)
	}

	if aws.ToString(renamedGet.RepositoryMetadata.RepositoryId) != repoId {
		t.Errorf(
			"RepositoryId mismatch after rename: %s vs %s",
			aws.ToString(renamedGet.RepositoryMetadata.RepositoryId),
			repoId,
		)
	}

	if aws.ToString(renamedGet.RepositoryMetadata.RepositoryDescription) != "Updated description" {
		t.Errorf(
			"description mismatch after update: %s",
			aws.ToString(renamedGet.RepositoryMetadata.RepositoryDescription),
		)
	}

	tagsGet, err = cc.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: renamedGet.RepositoryMetadata.Arn,
	})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}

	if tagsGet.Tags["Env"] != "Prod" || tagsGet.Tags["Team"] != "DevOps" || tagsGet.Tags["Dept"] != "" {
		t.Errorf("unexpected tags after update: %v", tagsGet.Tags)
	}

	// 3. A KmsKeyId change cannot be applied and must roll back.
	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName: new(stackName),
		TemplateBody: new(codecommitCfnTemplate(
			"cf-repo-renamed",
			"Updated description",
			"arn:aws:kms:us-east-1:000000000000:key/other-key",
			map[string]string{"Env": "Prod", "Team": "DevOps"},
			nil,
			nil,
		)),
	})
	if err == nil {
		wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusUpdateRollbackComplete)
	} else {
		code(t, err, "ValidationError")
	}

	// 4. Rollback test with unsupported Code property
	invalidCodeTemplate := codecommitCfnTemplate(
		"cf-repo-renamed",
		"Trying code",
		"arn:aws:kms:us-east-1:000000000000:key/cf-key",
		nil,
		map[string]any{"S3": map[string]any{"Bucket": "test-bkt", "Key": "test.zip"}},
		nil,
	)

	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    new(stackName),
		TemplateBody: new(invalidCodeTemplate),
	})
	if err == nil {
		wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusUpdateRollbackComplete)
	} else {
		code(t, err, "ValidationError")
	}

	// Verify repository state was preserved during rollback
	afterRollback, err := cc.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("cf-repo-renamed"),
	})
	if err != nil {
		t.Fatalf("GetRepository after rollback failed: %v", err)
	}

	if aws.ToString(afterRollback.RepositoryMetadata.RepositoryDescription) != "Updated description" {
		t.Errorf(
			"description changed during rolled-back update: %s",
			aws.ToString(afterRollback.RepositoryMetadata.RepositoryDescription),
		)
	}

	// 5. Rollback test with unsupported Triggers property
	invalidTriggersTemplate := codecommitCfnTemplate(
		"cf-repo-renamed",
		"Trying triggers",
		"arn:aws:kms:us-east-1:000000000000:key/cf-key",
		nil,
		nil,
		[]any{map[string]any{"Name": "test-trigger", "DestinationArn": "arn:aws:sns:us-east-1:000000000000:topic"}},
	)

	_, err = cf.UpdateStack(ctx, &cfn.UpdateStackInput{
		StackName:    new(stackName),
		TemplateBody: new(invalidTriggersTemplate),
	})
	if err == nil {
		wantStatus(ctx, t, cf, stackName, cfntypes.StackStatusUpdateRollbackComplete)
	} else {
		code(t, err, "ValidationError")
	}

	// 6. Delete stack
	_, err = cf.DeleteStack(ctx, &cfn.DeleteStackInput{
		StackName: new(stackName),
	})
	if err != nil {
		t.Fatalf("DeleteStack failed: %v", err)
	}

	wantStatus(ctx, t, cf, *createdStack.StackId, cfntypes.StackStatusDeleteComplete)

	// Verify repository is deleted
	_, err = cc.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("cf-repo-renamed"),
	})
	if err == nil {
		t.Errorf("expected repository to be deleted after stack deletion")
	}
}
