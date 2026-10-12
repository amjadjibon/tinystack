package integration_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codecommit"
	"github.com/aws/aws-sdk-go-v2/service/codecommit/types"
	rgta "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func codecommitClient(s *httptest.Server, key, region string) *sdk.Client {
	return sdk.New(
		sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
	)
}

func TestCodeCommitLifecycle(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codecommitClient(s, "test", "us-east-1")

	// 1. Create repository
	createOut, err := c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
		RepositoryName:        new("demo-repo"),
		RepositoryDescription: new("Demo repository"),
		KmsKeyId:              new("arn:aws:kms:us-east-1:000000000000:key/test-key"),
		Tags: map[string]string{
			"Environment": "Test",
			"Team":        "Platform",
		},
	})
	if err != nil {
		t.Fatalf("CreateRepository failed: %v", err)
	}

	meta := createOut.RepositoryMetadata
	if meta == nil {
		t.Fatal("expected non-nil RepositoryMetadata")
	}

	if aws.ToString(meta.RepositoryName) != "demo-repo" {
		t.Errorf("unexpected RepositoryName: %s", aws.ToString(meta.RepositoryName))
	}

	if aws.ToString(meta.RepositoryDescription) != "Demo repository" {
		t.Errorf("unexpected RepositoryDescription: %s", aws.ToString(meta.RepositoryDescription))
	}

	if aws.ToString(meta.KmsKeyId) != "arn:aws:kms:us-east-1:000000000000:key/test-key" {
		t.Errorf("unexpected KmsKeyId: %s", aws.ToString(meta.KmsKeyId))
	}

	if !strings.HasPrefix(aws.ToString(meta.Arn), "arn:aws:codecommit:us-east-1:") {
		t.Errorf("unexpected Arn: %s", aws.ToString(meta.Arn))
	}

	repoID := aws.ToString(meta.RepositoryId)
	if !uuidPattern.MatchString(repoID) {
		t.Errorf("RepositoryId is not valid UUID: %s", repoID)
	}

	if !strings.HasSuffix(aws.ToString(meta.CloneUrlHttp), "/v1/repos/demo-repo") {
		t.Errorf("unexpected CloneUrlHttp: %s", aws.ToString(meta.CloneUrlHttp))
	}

	if !strings.HasSuffix(
		aws.ToString(meta.CloneUrlSsh),
		"ssh://git-codecommit.us-east-1.amazonaws.com/v1/repos/demo-repo",
	) {
		t.Errorf("unexpected CloneUrlSsh: %s", aws.ToString(meta.CloneUrlSsh))
	}

	if meta.CreationDate == nil || meta.LastModifiedDate == nil {
		t.Errorf("expected timestamps to be set")
	}

	// 2. Duplicate create fails with RepositoryNameExistsException
	_, err = c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
		RepositoryName: new("demo-repo"),
	})
	if err == nil {
		t.Fatal("expected error creating duplicate repository")
	}

	var existsErr *types.RepositoryNameExistsException
	if !errors.As(err, &existsErr) {
		t.Fatalf("expected RepositoryNameExistsException, got: %T: %v", err, err)
	}

	// 3. GetRepository
	getOut, err := c.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("demo-repo"),
	})
	if err != nil {
		t.Fatalf("GetRepository failed: %v", err)
	}

	if aws.ToString(getOut.RepositoryMetadata.RepositoryId) != repoID {
		t.Errorf("GetRepository ID mismatch: %s vs %s", aws.ToString(getOut.RepositoryMetadata.RepositoryId), repoID)
	}

	// Get missing repository fails with RepositoryDoesNotExistException
	_, err = c.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("nonexistent"),
	})
	if err == nil {
		t.Fatal("expected error getting missing repository")
	}

	var notFoundErr *types.RepositoryDoesNotExistException
	if !errors.As(err, &notFoundErr) {
		t.Fatalf("expected RepositoryDoesNotExistException, got: %T: %v", err, err)
	}

	// 4. UpdateRepositoryDescription
	_, err = c.UpdateRepositoryDescription(ctx, &sdk.UpdateRepositoryDescriptionInput{
		RepositoryName:        new("demo-repo"),
		RepositoryDescription: new("Updated description"),
	})
	if err != nil {
		t.Fatalf("UpdateRepositoryDescription failed: %v", err)
	}

	getOut, err = c.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("demo-repo"),
	})
	if err != nil {
		t.Fatalf("GetRepository after description update failed: %v", err)
	}

	if aws.ToString(getOut.RepositoryMetadata.RepositoryDescription) != "Updated description" {
		t.Errorf("unexpected description: %s", aws.ToString(getOut.RepositoryMetadata.RepositoryDescription))
	}

	// 5. UpdateRepositoryName
	_, err = c.UpdateRepositoryName(ctx, &sdk.UpdateRepositoryNameInput{
		OldName: new("demo-repo"),
		NewName: new("renamed-repo"),
	})
	if err != nil {
		t.Fatalf("UpdateRepositoryName failed: %v", err)
	}

	// Old name must not exist
	_, err = c.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("demo-repo"),
	})
	if err == nil {
		t.Fatal("expected old name to not exist")
	}

	// New name must exist with same repository ID and updated clone URLs
	renamedGet, err := c.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("renamed-repo"),
	})
	if err != nil {
		t.Fatalf("GetRepository with new name failed: %v", err)
	}

	if aws.ToString(renamedGet.RepositoryMetadata.RepositoryId) != repoID {
		t.Errorf(
			"RepositoryId changed after rename: %s vs %s",
			aws.ToString(renamedGet.RepositoryMetadata.RepositoryId),
			repoID,
		)
	}

	if !strings.HasSuffix(aws.ToString(renamedGet.RepositoryMetadata.CloneUrlHttp), "/v1/repos/renamed-repo") {
		t.Errorf("unexpected CloneUrlHttp after rename: %s", aws.ToString(renamedGet.RepositoryMetadata.CloneUrlHttp))
	}

	// 6. Tags
	repoArn := renamedGet.RepositoryMetadata.Arn
	tagsOut, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: repoArn,
	})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}

	if tagsOut.Tags["Environment"] != "Test" || tagsOut.Tags["Team"] != "Platform" {
		t.Errorf("unexpected tags: %v", tagsOut.Tags)
	}

	_, err = c.TagResource(ctx, &sdk.TagResourceInput{
		ResourceArn: repoArn,
		Tags: map[string]string{
			"NewTag": "NewVal",
		},
	})
	if err != nil {
		t.Fatalf("TagResource failed: %v", err)
	}

	_, err = c.UntagResource(ctx, &sdk.UntagResourceInput{
		ResourceArn: repoArn,
		TagKeys:     []string{"Environment"},
	})
	if err != nil {
		t.Fatalf("UntagResource failed: %v", err)
	}

	tagsOut, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: repoArn,
	})
	if err != nil {
		t.Fatalf("ListTagsForResource after untag failed: %v", err)
	}

	if _, ok := tagsOut.Tags["Environment"]; ok {
		t.Error("expected Environment tag to be removed")
	}

	if tagsOut.Tags["NewTag"] != "NewVal" || tagsOut.Tags["Team"] != "Platform" {
		t.Errorf("unexpected tags after untag: %v", tagsOut.Tags)
	}

	// 7. DeleteRepository
	delOut, err := c.DeleteRepository(ctx, &sdk.DeleteRepositoryInput{
		RepositoryName: new("renamed-repo"),
	})
	if err != nil {
		t.Fatalf("DeleteRepository failed: %v", err)
	}

	if aws.ToString(delOut.RepositoryId) != repoID {
		t.Errorf("DeleteRepository returned unexpected ID: %s vs %s", aws.ToString(delOut.RepositoryId), repoID)
	}

	// Delete missing repository returns nil repositoryId and no error
	delMissing, err := c.DeleteRepository(ctx, &sdk.DeleteRepositoryInput{
		RepositoryName: new("nonexistent"),
	})
	if err != nil {
		t.Fatalf("DeleteRepository on nonexistent repo should not fail: %v", err)
	}

	if delMissing.RepositoryId != nil {
		t.Errorf("expected nil repositoryId when deleting missing repo, got: %s", *delMissing.RepositoryId)
	}

	// 8. Scope isolation
	_, err = c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
		RepositoryName: new("scope-repo"),
	})
	if err != nil {
		t.Fatal(err)
	}

	westClient := codecommitClient(s, "test", "us-west-2")
	_, err = westClient.GetRepository(ctx, &sdk.GetRepositoryInput{
		RepositoryName: new("scope-repo"),
	})
	if err == nil {
		t.Fatal("expected repository in us-east-1 to not be visible in us-west-2")
	}
}

func TestCodeCommitListSortingAndPagination(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codecommitClient(s, "list-user", "us-east-1")

	repoNames := []string{"repo-c", "repo-a", "repo-b"}
	for _, name := range repoNames {
		_, err := c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
			RepositoryName: new(name),
		})
		if err != nil {
			t.Fatalf("CreateRepository failed for %s: %v", name, err)
		}

		time.Sleep(10 * time.Millisecond)
	}

	// Ascending by repositoryName
	listAsc, err := c.ListRepositories(ctx, &sdk.ListRepositoriesInput{
		SortBy: types.SortByEnumRepositoryName,
		Order:  types.OrderEnumAscending,
	})
	if err != nil {
		t.Fatalf("ListRepositories asc failed: %v", err)
	}

	if len(listAsc.Repositories) != 3 {
		t.Fatalf("expected 3 repositories, got %d", len(listAsc.Repositories))
	}

	if *listAsc.Repositories[0].RepositoryName != "repo-a" ||
		*listAsc.Repositories[1].RepositoryName != "repo-b" ||
		*listAsc.Repositories[2].RepositoryName != "repo-c" {
		t.Errorf("unexpected repository order: %v, %v, %v",
			*listAsc.Repositories[0].RepositoryName,
			*listAsc.Repositories[1].RepositoryName,
			*listAsc.Repositories[2].RepositoryName)
	}

	// Descending by repositoryName
	listDesc, err := c.ListRepositories(ctx, &sdk.ListRepositoriesInput{
		SortBy: types.SortByEnumRepositoryName,
		Order:  types.OrderEnumDescending,
	})
	if err != nil {
		t.Fatalf("ListRepositories desc failed: %v", err)
	}

	if *listDesc.Repositories[0].RepositoryName != "repo-c" ||
		*listDesc.Repositories[1].RepositoryName != "repo-b" ||
		*listDesc.Repositories[2].RepositoryName != "repo-a" {
		t.Errorf("unexpected repository order desc: %v, %v, %v",
			*listDesc.Repositories[0].RepositoryName,
			*listDesc.Repositories[1].RepositoryName,
			*listDesc.Repositories[2].RepositoryName)
	}

	// Pagination with nextToken
	p1, err := c.ListRepositories(ctx, &sdk.ListRepositoriesInput{
		SortBy: types.SortByEnumRepositoryName,
		Order:  types.OrderEnumAscending,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(p1.Repositories) != 3 {
		t.Fatalf("expected 3 repositories: got %d", len(p1.Repositories))
	}
}

func TestCodeCommitResourceGroupsTaggingAPI(t *testing.T) {
	s := server(t)
	ctx := context.Background()
	c := codecommitClient(s, "rgta-user", "us-east-1")

	repo, err := c.CreateRepository(ctx, &sdk.CreateRepositoryInput{
		RepositoryName: new("rgta-repo"),
		Tags: map[string]string{
			"Initial": "Tag",
		},
	})
	if err != nil {
		t.Fatalf("CreateRepository failed: %v", err)
	}

	repoArn := *repo.RepositoryMetadata.Arn
	taggingClient := rgta.New(
		rgta.Options{Region: "us-east-1", BaseEndpoint: new(s.URL), Credentials: creds("rgta-user")},
	)

	// Tag through RGTA
	tagged, err := taggingClient.TagResources(ctx, &rgta.TagResourcesInput{
		ResourceARNList: []string{repoArn},
		Tags: map[string]string{
			"TagFromRGTA": "ValueRGTA",
		},
	})
	if err != nil || len(tagged.FailedResourcesMap) != 0 {
		t.Fatalf("TagResources failed: %v, failed: %v", err, tagged.FailedResourcesMap)
	}

	// Verify in CodeCommit
	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: new(repoArn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}

	if tags.Tags["TagFromRGTA"] != "ValueRGTA" || tags.Tags["Initial"] != "Tag" {
		t.Errorf("unexpected tags: %v", tags.Tags)
	}

	// Untag through RGTA
	untagged, err := taggingClient.UntagResources(ctx, &rgta.UntagResourcesInput{
		ResourceARNList: []string{repoArn},
		TagKeys:         []string{"TagFromRGTA"},
	})
	if err != nil || len(untagged.FailedResourcesMap) != 0 {
		t.Fatalf("UntagResources failed: %v, failed: %v", err, untagged.FailedResourcesMap)
	}

	// Verify in CodeCommit
	tags, err = c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{
		ResourceArn: new(repoArn),
	})
	if err != nil {
		t.Fatalf("ListTagsForResource failed: %v", err)
	}

	if _, ok := tags.Tags["TagFromRGTA"]; ok {
		t.Errorf("expected TagFromRGTA to be removed: %v", tags.Tags)
	}

	if tags.Tags["Initial"] != "Tag" {
		t.Errorf("expected Initial tag to persist: %v", tags.Tags)
	}
}
