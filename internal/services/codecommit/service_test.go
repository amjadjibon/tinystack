package codecommit

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func testContext(account, region string) context.Context {
	return request.WithMetadata(
		context.Background(),
		request.Metadata{Scope: request.Scope{AccountID: account, Region: region}},
	)
}

func wantError(t *testing.T, e error, code string) {
	t.Helper()
	var aws *awserror.Error
	if !errors.As(e, &aws) || aws.Code != code {
		t.Fatalf("want %s, got %v", code, e)
	}
}

func localContext() context.Context {
	return testContext("123456789012", "us-east-1")
}

func TestCreateGetRepository(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()

	name := "my-repo"
	desc := "test repo description"
	kms := "arn:aws:kms:us-east-1:123456789012:key/test-key"
	tags := map[string]string{"env": "test", "team": "dev"}

	out, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName:        &name,
		RepositoryDescription: &desc,
		KmsKeyId:              &kms,
		Tags:                  tags,
	})
	if err != nil {
		t.Fatalf("CreateRepository failed: %v", err)
	}

	meta := out.RepositoryMetadata
	if meta == nil {
		t.Fatal("expected non-nil RepositoryMetadata")
	}

	if common.Value(meta.RepositoryName) != name {
		t.Errorf("got RepositoryName %s, want %s", common.Value(meta.RepositoryName), name)
	}

	if common.Value(meta.RepositoryDescription) != desc {
		t.Errorf("got RepositoryDescription %s, want %s", common.Value(meta.RepositoryDescription), desc)
	}

	if common.Value(meta.KmsKeyId) != kms {
		t.Errorf("got KmsKeyId %s, want %s", common.Value(meta.KmsKeyId), kms)
	}

	if common.Value(meta.AccountId) != "123456789012" {
		t.Errorf("got AccountId %s, want 123456789012", common.Value(meta.AccountId))
	}

	if id := common.Value(meta.RepositoryId); !uuidPattern.MatchString(id) {
		t.Errorf("RepositoryId %s does not match UUID format", id)
	}

	expectedArn := "arn:aws:codecommit:us-east-1:123456789012:my-repo"
	if common.Value(meta.Arn) != expectedArn {
		t.Errorf("got Arn %s, want %s", common.Value(meta.Arn), expectedArn)
	}

	expectedHttp := "https://git-codecommit.us-east-1.amazonaws.com/v1/repos/my-repo"
	if common.Value(meta.CloneUrlHttp) != expectedHttp {
		t.Errorf("got CloneUrlHttp %s, want %s", common.Value(meta.CloneUrlHttp), expectedHttp)
	}

	expectedSsh := "ssh://git-codecommit.us-east-1.amazonaws.com/v1/repos/my-repo"
	if common.Value(meta.CloneUrlSsh) != expectedSsh {
		t.Errorf("got CloneUrlSsh %s, want %s", common.Value(meta.CloneUrlSsh), expectedSsh)
	}

	if common.Value(meta.CreationDate) <= 0 || common.Value(meta.LastModifiedDate) <= 0 {
		t.Errorf("expected positive timestamps, got creation=%f modified=%f",
			common.Value(meta.CreationDate), common.Value(meta.LastModifiedDate))
	}

	getOut, err := svc.GetRepository(ctx, &api.GetRepositoryInput{
		RepositoryName: &name,
	})
	if err != nil {
		t.Fatalf("GetRepository failed: %v", err)
	}

	if common.Value(getOut.RepositoryMetadata.RepositoryId) != common.Value(meta.RepositoryId) {
		t.Errorf("GetRepository ID mismatch")
	}

	// Duplicate create should fail with RepositoryNameExistsException
	_, err = svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &name,
	})
	wantError(t, err, "RepositoryNameExistsException")

	// Missing repo name
	_, err = svc.CreateRepository(ctx, &api.CreateRepositoryInput{})
	wantError(t, err, "RepositoryNameRequiredException")

	// Invalid name
	invalidName := "my repo with spaces"
	_, err = svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &invalidName,
	})
	wantError(t, err, "InvalidRepositoryNameException")

	// Name too long (> 100)
	tooLongName := strings.Repeat("a", 101)
	_, err = svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &tooLongName,
	})
	wantError(t, err, "InvalidRepositoryNameException")

	// Description too long (> 1000)
	tooLongDesc := strings.Repeat("d", 1001)
	validName := "another-repo"
	_, err = svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName:        &validName,
		RepositoryDescription: &tooLongDesc,
	})
	wantError(t, err, "InvalidRepositoryDescriptionException")

	// Get non-existent
	nonExistent := "non-existent"
	_, err = svc.GetRepository(ctx, &api.GetRepositoryInput{
		RepositoryName: &nonExistent,
	})
	wantError(t, err, "RepositoryDoesNotExistException")
}

func TestUpdateRepositoryName(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()

	oldName := "repo-old"
	newName := "repo-new"
	existingName := "repo-other"

	cr, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &oldName,
	})
	if err != nil {
		t.Fatal(err)
	}

	origID := common.Value(cr.RepositoryMetadata.RepositoryId)
	origCreated := common.Value(cr.RepositoryMetadata.CreationDate)

	_, err = svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &existingName,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Conflict with existing name
	_, err = svc.UpdateRepositoryName(ctx, &api.UpdateRepositoryNameInput{
		OldName: &oldName,
		NewName: &existingName,
	})
	wantError(t, err, "RepositoryNameExistsException")

	// Non-existent old name
	fakeName := "does-not-exist"
	_, err = svc.UpdateRepositoryName(ctx, &api.UpdateRepositoryNameInput{
		OldName: &fakeName,
		NewName: &newName,
	})
	wantError(t, err, "RepositoryDoesNotExistException")

	// Successful rename
	time.Sleep(10 * time.Millisecond)
	_, err = svc.UpdateRepositoryName(ctx, &api.UpdateRepositoryNameInput{
		OldName: &oldName,
		NewName: &newName,
	})
	if err != nil {
		t.Fatalf("UpdateRepositoryName failed: %v", err)
	}

	// Old name gone
	_, err = svc.GetRepository(ctx, &api.GetRepositoryInput{
		RepositoryName: &oldName,
	})
	wantError(t, err, "RepositoryDoesNotExistException")

	// New name present with preserved ID and creation date, updated modified date and URLs
	updated, err := svc.GetRepository(ctx, &api.GetRepositoryInput{
		RepositoryName: &newName,
	})
	if err != nil {
		t.Fatalf("GetRepository newName failed: %v", err)
	}

	meta := updated.RepositoryMetadata
	if common.Value(meta.RepositoryId) != origID {
		t.Errorf("RepositoryId changed: got %s, want %s", common.Value(meta.RepositoryId), origID)
	}

	if common.Value(meta.CreationDate) != origCreated {
		t.Errorf("CreationDate changed: got %f, want %f", common.Value(meta.CreationDate), origCreated)
	}

	if common.Value(meta.Arn) != "arn:aws:codecommit:us-east-1:123456789012:repo-new" {
		t.Errorf("Arn not updated: %s", common.Value(meta.Arn))
	}

	if common.Value(meta.LastModifiedDate) <= origCreated {
		t.Errorf("LastModifiedDate not advanced: %f <= %f", common.Value(meta.LastModifiedDate), origCreated)
	}
}

func TestUpdateRepositoryDescription(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()

	name := "repo-desc-test"
	origDesc := "original"
	newDesc := "updated description"

	cr, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName:        &name,
		RepositoryDescription: &origDesc,
	})
	if err != nil {
		t.Fatal(err)
	}

	origCreated := common.Value(cr.RepositoryMetadata.CreationDate)

	time.Sleep(10 * time.Millisecond)
	_, err = svc.UpdateRepositoryDescription(ctx, &api.UpdateRepositoryDescriptionInput{
		RepositoryName:        &name,
		RepositoryDescription: &newDesc,
	})
	if err != nil {
		t.Fatalf("UpdateRepositoryDescription failed: %v", err)
	}

	got, err := svc.GetRepository(ctx, &api.GetRepositoryInput{
		RepositoryName: &name,
	})
	if err != nil {
		t.Fatal(err)
	}

	if common.Value(got.RepositoryMetadata.RepositoryDescription) != newDesc {
		t.Errorf("description mismatch: got %s, want %s",
			common.Value(got.RepositoryMetadata.RepositoryDescription), newDesc)
	}

	if common.Value(got.RepositoryMetadata.LastModifiedDate) <= origCreated {
		t.Errorf("LastModifiedDate not advanced")
	}

	// Non-existent
	missing := "non-existent"
	_, err = svc.UpdateRepositoryDescription(ctx, &api.UpdateRepositoryDescriptionInput{
		RepositoryName:        &missing,
		RepositoryDescription: &newDesc,
	})
	wantError(t, err, "RepositoryDoesNotExistException")

	// Too long
	tooLong := strings.Repeat("x", 1001)
	_, err = svc.UpdateRepositoryDescription(ctx, &api.UpdateRepositoryDescriptionInput{
		RepositoryName:        &name,
		RepositoryDescription: &tooLong,
	})
	wantError(t, err, "InvalidRepositoryDescriptionException")
}

func TestDeleteRepository(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()

	name := "to-delete"
	cr, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &name,
	})
	if err != nil {
		t.Fatal(err)
	}

	expectedID := common.Value(cr.RepositoryMetadata.RepositoryId)

	delOut, err := svc.DeleteRepository(ctx, &api.DeleteRepositoryInput{
		RepositoryName: &name,
	})
	if err != nil {
		t.Fatalf("DeleteRepository failed: %v", err)
	}

	if common.Value(delOut.RepositoryId) != expectedID {
		t.Errorf("deleted RepositoryId mismatch: got %s, want %s",
			common.Value(delOut.RepositoryId), expectedID)
	}

	// Deleting again (already absent) returns nil RepositoryId and NO error
	delOut2, err := svc.DeleteRepository(ctx, &api.DeleteRepositoryInput{
		RepositoryName: &name,
	})
	if err != nil {
		t.Fatalf("DeleteRepository absent repo should not error: %v", err)
	}

	if delOut2.RepositoryId != nil {
		t.Errorf("expected nil RepositoryId for absent repo, got %s", *delOut2.RepositoryId)
	}

	// Get confirms gone
	_, err = svc.GetRepository(ctx, &api.GetRepositoryInput{
		RepositoryName: &name,
	})
	wantError(t, err, "RepositoryDoesNotExistException")
}

func TestListRepositories(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()

	names := []string{"repo-c", "repo-a", "repo-b"}
	for _, n := range names {
		nameCopy := n
		_, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{
			RepositoryName: &nameCopy,
		})
		if err != nil {
			t.Fatal(err)
		}

		time.Sleep(5 * time.Millisecond)
	}

	// Default sort (repositoryName, ascending)
	out, err := svc.ListRepositories(ctx, &api.ListRepositoriesInput{})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Repositories) != 3 {
		t.Fatalf("expected 3 repos, got %d", len(out.Repositories))
	}

	if *out.Repositories[0].RepositoryName != "repo-a" ||
		*out.Repositories[1].RepositoryName != "repo-b" ||
		*out.Repositories[2].RepositoryName != "repo-c" {
		t.Errorf("unexpected ascending order: %v, %v, %v",
			*out.Repositories[0].RepositoryName,
			*out.Repositories[1].RepositoryName,
			*out.Repositories[2].RepositoryName)
	}

	// Descending order
	descOrder := "descending"
	outDesc, err := svc.ListRepositories(ctx, &api.ListRepositoriesInput{
		Order: &descOrder,
	})
	if err != nil {
		t.Fatal(err)
	}

	if *outDesc.Repositories[0].RepositoryName != "repo-c" ||
		*outDesc.Repositories[1].RepositoryName != "repo-b" ||
		*outDesc.Repositories[2].RepositoryName != "repo-a" {
		t.Errorf("unexpected descending order: %v, %v, %v",
			*outDesc.Repositories[0].RepositoryName,
			*outDesc.Repositories[1].RepositoryName,
			*outDesc.Repositories[2].RepositoryName)
	}

	// Sort by lastModifiedDate ascending (created c, then a, then b)
	sortByDate := "lastModifiedDate"
	outDate, err := svc.ListRepositories(ctx, &api.ListRepositoriesInput{
		SortBy: &sortByDate,
	})
	if err != nil {
		t.Fatal(err)
	}

	if *outDate.Repositories[0].RepositoryName != "repo-c" ||
		*outDate.Repositories[1].RepositoryName != "repo-a" ||
		*outDate.Repositories[2].RepositoryName != "repo-b" {
		t.Errorf("unexpected date order: %v, %v, %v",
			*outDate.Repositories[0].RepositoryName,
			*outDate.Repositories[1].RepositoryName,
			*outDate.Repositories[2].RepositoryName)
	}

	// Invalid SortBy
	invalidSort := "invalidSort"
	_, err = svc.ListRepositories(ctx, &api.ListRepositoriesInput{
		SortBy: &invalidSort,
	})
	wantError(t, err, "InvalidSortByException")

	// Invalid Order
	invalidOrder := "invalidOrder"
	_, err = svc.ListRepositories(ctx, &api.ListRepositoriesInput{
		Order: &invalidOrder,
	})
	wantError(t, err, "InvalidOrderException")

	// Invalid Continuation Token
	badToken := "bad-token"
	_, err = svc.ListRepositories(ctx, &api.ListRepositoriesInput{
		NextToken: &badToken,
	})
	wantError(t, err, "InvalidContinuationTokenException")
}

func TestTags(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()

	name := "tagged-repo"
	cr, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &name,
		Tags:           map[string]string{"initial": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	repoArn := common.Value(cr.RepositoryMetadata.Arn)

	// ListTags
	listOut, err := svc.ListTagsForResource(ctx, &api.ListTagsForResourceInput{
		ResourceArn: &repoArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	if listOut.Tags["initial"] != "true" {
		t.Errorf("expected initial tag")
	}

	// TagResource
	_, err = svc.TagResource(ctx, &api.TagResourceInput{
		ResourceArn: &repoArn,
		Tags:        map[string]string{"env": "staging", "tier": "backend"},
	})
	if err != nil {
		t.Fatal(err)
	}

	listOut, err = svc.ListTagsForResource(ctx, &api.ListTagsForResourceInput{
		ResourceArn: &repoArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listOut.Tags) != 3 || listOut.Tags["env"] != "staging" {
		t.Errorf("unexpected tags: %v", listOut.Tags)
	}

	// UntagResource
	_, err = svc.UntagResource(ctx, &api.UntagResourceInput{
		ResourceArn: &repoArn,
		TagKeys:     []string{"initial", "env"},
	})
	if err != nil {
		t.Fatal(err)
	}

	listOut, err = svc.ListTagsForResource(ctx, &api.ListTagsForResourceInput{
		ResourceArn: &repoArn,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(listOut.Tags) != 1 || listOut.Tags["tier"] != "backend" {
		t.Errorf("unexpected tags after untag: %v", listOut.Tags)
	}

	// Validation: aws: prefix
	_, err = svc.TagResource(ctx, &api.TagResourceInput{
		ResourceArn: &repoArn,
		Tags:        map[string]string{"aws:reserved": "bad"},
	})
	wantError(t, err, "InvalidSystemTagUsageException")

	// Validation: missing ARN
	_, err = svc.TagResource(ctx, &api.TagResourceInput{
		Tags: map[string]string{"k": "v"},
	})
	wantError(t, err, "ResourceArnRequiredException")

	// Validation: invalid ARN format
	badArn := "invalid-arn"
	_, err = svc.TagResource(ctx, &api.TagResourceInput{
		ResourceArn: &badArn,
		Tags:        map[string]string{"k": "v"},
	})
	wantError(t, err, "InvalidResourceArnException")

	// Validation: non-existent repo ARN
	nonExistentArn := "arn:aws:codecommit:us-east-1:123456789012:ghost"
	_, err = svc.TagResource(ctx, &api.TagResourceInput{
		ResourceArn: &nonExistentArn,
		Tags:        map[string]string{"k": "v"},
	})
	wantError(t, err, "RepositoryDoesNotExistException")
}

func TestInventory(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()

	name := "inventory-repo"
	_, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{
		RepositoryName: &name,
		Tags:           map[string]string{"owned": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.Resources(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(res) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(res))
	}

	r := res[0]
	if r.Account != "123456789012" || r.Region != "us-east-1" || r.Name != name || r.Type != "repository" {
		t.Errorf("unexpected resource fields: %+v", r)
	}

	details, _ := r.Details.(map[string]any)
	if details["RecordsOnly"] != true || details["ExecutionReady"] != false {
		t.Errorf("unexpected details: %v", details)
	}
}

// Pages hold 1,000 repositories, and a page resumes after its cursor even when the cursor repository is deleted.
func TestListRepositoriesPages(t *testing.T) {
	svc := New(memory.New(nil, nil))
	ctx := localContext()
	for i := range 1002 {
		name := fmt.Sprintf("repo-%04d", i)
		if _, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{RepositoryName: &name}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := svc.ListRepositories(ctx, &api.ListRepositoriesInput{Order: new("descending")})
	if err != nil || len(first.Repositories) != 1000 || first.NextToken == nil ||
		*first.Repositories[0].RepositoryName != "repo-1001" {
		t.Fatal(len(first.Repositories), err)
	}

	cursor := first.Repositories[999].RepositoryName
	if _, err = svc.DeleteRepository(ctx, &api.DeleteRepositoryInput{RepositoryName: cursor}); err != nil {
		t.Fatal(err)
	}

	second, err := svc.ListRepositories(ctx, &api.ListRepositoriesInput{
		Order:     new("descending"),
		NextToken: first.NextToken,
	})
	if err != nil || len(second.Repositories) != 2 || second.NextToken != nil ||
		*second.Repositories[0].RepositoryName != "repo-0001" {
		t.Fatal(second, err)
	}
}
