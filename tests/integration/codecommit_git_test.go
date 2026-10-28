package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/codecommit"
	"github.com/aws/aws-sdk-go-v2/service/codecommit/types"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go"

	"github.com/amjadjibon/tinystack/internal/config"
)

// gitHome is a scratch home directory and isolated git environment, so the user's git configuration and
// credential helpers are never read or written.
type gitHome struct {
	t    *testing.T
	home string
	env  []string
}

func newGitHome(t *testing.T) *gitHome {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; the CodeCommit git transport tests need it")
	}

	home := t.TempDir()
	return &gitHome{t: t, home: home, env: append(os.Environ(),
		"HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com",
	)}
}

// run runs git in dir and returns its output.
func (g *gitHome) run(dir string, args ...string) (string, error) {
	g.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git",
		append([]string{"-c", "init.defaultBranch=main", "-c", "advice.defaultBranchName=false"}, args...)...)
	cmd.Dir, cmd.Env = dir, g.env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// must runs git and fails the test if it does.
func (g *gitHome) must(dir string, args ...string) string {
	g.t.Helper()
	out, err := g.run(dir, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// write creates a file in a working tree.
func (g *gitHome) write(dir, name, content string) {
	g.t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		g.t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		g.t.Fatal(err)
	}
}

func codeCommitCode(t *testing.T, err error) string {
	t.Helper()
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		return apiErr.ErrorCode()
	}

	return ""
}

func createRepo(t *testing.T, c *sdk.Client, name string) {
	t.Helper()
	_, err := c.CreateRepository(context.Background(), &sdk.CreateRepositoryInput{RepositoryName: new(name)})
	if err != nil {
		t.Fatal(err)
	}
}

func mainTip(t *testing.T, c *sdk.Client, repo string) string {
	t.Helper()
	out, err := c.GetBranch(
		context.Background(),
		&sdk.GetBranchInput{RepositoryName: new(repo), BranchName: new("main")},
	)
	if err != nil {
		t.Fatal(err)
	}

	return aws.ToString(out.Branch.CommitId)
}

func TestCodeCommitGitCloneAndPush(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := codecommitClient(s, "test", "us-east-1")
	g := newGitHome(t)
	createRepo(t, c, "demo")
	url := s.URL + "/v1/repos/demo"

	// A repository with no branch has no default branch, and lists none.
	repo, err := c.GetRepository(ctx, &sdk.GetRepositoryInput{RepositoryName: new("demo")})
	if err != nil || repo.RepositoryMetadata.DefaultBranch != nil {
		t.Fatal(repo, err)
	}

	branches, err := c.ListBranches(ctx, &sdk.ListBranchesInput{RepositoryName: new("demo")})
	if err != nil || len(branches.Branches) != 0 {
		t.Fatal(branches, err)
	}

	if _, err = c.GetBranch(
		ctx,
		&sdk.GetBranchInput{RepositoryName: new("demo"), BranchName: new("main")},
	); codeCommitCode(
		t,
		err,
	) != "BranchDoesNotExistException" {
		t.Fatal(err)
	}

	// Clone the empty repository, commit and push.
	work := filepath.Join(g.home, "work")
	g.must(g.home, "clone", url, work)
	g.must(work, "checkout", "-b", "main")
	g.write(work, "README.md", "hello\n")
	g.write(work, "docs/guide.txt", "guide\n")
	g.write(work, "run.sh", "#!/bin/sh\n")
	if err = os.Chmod(filepath.Join(work, "run.sh"), 0o700); err != nil {
		t.Fatal(err)
	}

	g.must(work, "add", ".")
	g.must(work, "commit", "-m", "first commit")
	g.must(work, "push", "origin", "main")
	first := g.must(work, "rev-parse", "HEAD")

	repo, err = c.GetRepository(ctx, &sdk.GetRepositoryInput{RepositoryName: new("demo")})
	if err != nil || aws.ToString(repo.RepositoryMetadata.DefaultBranch) != "main" {
		t.Fatal(repo, err)
	}

	// The AWS-format clone URL stays, whatever the local URL is.
	if got := aws.ToString(
		repo.RepositoryMetadata.CloneUrlHttp,
	); got != "https://git-codecommit.us-east-1.amazonaws.com/v1/repos/demo" {
		t.Fatal(got)
	}

	if tip := mainTip(t, c, "demo"); tip != first {
		t.Fatalf("branch tip %s, pushed %s", tip, first)
	}

	branches, err = c.ListBranches(ctx, &sdk.ListBranchesInput{RepositoryName: new("demo")})
	if err != nil || len(branches.Branches) != 1 || branches.Branches[0] != "main" {
		t.Fatal(branches, err)
	}

	commit, err := c.GetCommit(ctx, &sdk.GetCommitInput{RepositoryName: new("demo"), CommitId: &first})
	if err != nil || aws.ToString(commit.Commit.Message) != "first commit\n" ||
		aws.ToString(
			commit.Commit.Author.Name,
		) != "Dev" || aws.ToString(commit.Commit.Author.Email) != "dev@example.com" ||
		len(commit.Commit.Parents) != 0 || aws.ToString(commit.Commit.TreeId) == "" {
		t.Fatal(commit, err)
	}

	file, err := c.GetFile(ctx, &sdk.GetFileInput{RepositoryName: new("demo"), FilePath: new("docs/guide.txt")})
	if err != nil || string(file.FileContent) != "guide\n" || file.FileMode != types.FileModeTypeEnumNormal ||
		file.FileSize != 6 || aws.ToString(file.CommitId) != first {
		t.Fatal(file, err)
	}

	script, err := c.GetFile(ctx, &sdk.GetFileInput{
		RepositoryName: new("demo"), FilePath: new("/run.sh"), CommitSpecifier: new("main"),
	})
	if err != nil || script.FileMode != types.FileModeTypeEnumExecutable {
		t.Fatal(script, err)
	}

	root, err := c.GetFolder(ctx, &sdk.GetFolderInput{RepositoryName: new("demo"), FolderPath: new("/")})
	if err != nil || len(root.SubFolders) != 1 || aws.ToString(root.SubFolders[0].AbsolutePath) != "docs" ||
		len(root.Files) != 2 || aws.ToString(root.Files[0].RelativePath) != "README.md" {
		t.Fatal(root, err)
	}

	docs, err := c.GetFolder(ctx, &sdk.GetFolderInput{RepositoryName: new("demo"), FolderPath: new("docs")})
	if err != nil || len(docs.Files) != 1 || aws.ToString(docs.Files[0].AbsolutePath) != "docs/guide.txt" ||
		aws.ToString(docs.Files[0].RelativePath) != "guide.txt" {
		t.Fatal(docs, err)
	}

	// A second push is a thin pack that deltas against the first; a force push replaces history.
	g.write(work, "README.md", strings.Repeat("hello world\n", 2000))
	g.must(work, "commit", "-am", "second commit")
	g.must(work, "push", "origin", "main")
	g.write(work, "README.md", strings.Repeat("hello world\n", 2001))
	g.must(work, "commit", "-a", "--amend", "-m", "amended")
	g.must(work, "push", "--force", "origin", "main")
	amended := g.must(work, "rev-parse", "HEAD")
	if tip := mainTip(t, c, "demo"); tip != amended {
		t.Fatalf("branch tip %s, pushed %s", tip, amended)
	}

	// PutFile makes a commit that a pull sees.
	put, err := c.PutFile(ctx, &sdk.PutFileInput{
		RepositoryName: new("demo"), BranchName: new("main"), FilePath: new("docs/api.txt"),
		FileContent: []byte("api\n"), ParentCommitId: &amended, CommitMessage: new("add api"),
		Name: new("Pat"), Email: new("pat@example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}

	g.must(work, "pull", "--ff-only", "origin", "main")
	if got := g.must(work, "show", "-s", "--format=%an <%ae> %s", "HEAD"); got != "Pat <pat@example.com> add api" {
		t.Fatal(got)
	}

	if got := g.must(work, "rev-parse", "HEAD"); got != aws.ToString(put.CommitId) {
		t.Fatalf("pulled %s, PutFile made %s", got, aws.ToString(put.CommitId))
	}

	// Shallow clones and every protocol version work.
	shallow := filepath.Join(g.home, "shallow")
	g.must(g.home, "clone", "--depth", "1", url, shallow)
	if got := g.must(shallow, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("shallow clone has %s commits", got)
	}

	for _, version := range []string{"0", "1", "2"} {
		dir := filepath.Join(g.home, "v"+version)
		g.must(g.home, "-c", "protocol.version="+version, "clone", url, dir)
		if got := g.must(dir, "rev-parse", "HEAD"); got != aws.ToString(put.CommitId) {
			t.Fatalf("protocol %s cloned %s", version, got)
		}
	}

	// A second branch does not change the default branch.
	g.must(work, "push", "origin", "main:feature")
	branches, err = c.ListBranches(ctx, &sdk.ListBranchesInput{RepositoryName: new("demo")})
	if err != nil || len(branches.Branches) != 2 || branches.Branches[0] != "feature" ||
		branches.Branches[1] != "main" {
		t.Fatal(branches, err)
	}

	repo, _ = c.GetRepository(ctx, &sdk.GetRepositoryInput{RepositoryName: new("demo")})
	if aws.ToString(repo.RepositoryMetadata.DefaultBranch) != "main" {
		t.Fatal(repo)
	}

	// CreateBranch and DeleteBranch round-trip through git ls-remote.
	tip := aws.ToString(put.CommitId)
	if _, err = c.CreateBranch(ctx, &sdk.CreateBranchInput{
		RepositoryName: new("demo"), BranchName: new("release/1.0"), CommitId: &tip,
	}); err != nil {
		t.Fatal(err)
	}

	if got := g.must(work, "ls-remote", "--heads", "origin", "release/1.0"); got != tip+"\trefs/heads/release/1.0" {
		t.Fatal(got)
	}

	_, err = c.CreateBranch(ctx, &sdk.CreateBranchInput{
		RepositoryName: new("demo"), BranchName: new("release/1.0"), CommitId: &tip,
	})
	if codeCommitCode(t, err) != "BranchNameExistsException" {
		t.Fatal(err)
	}

	deleted, err := c.DeleteBranch(ctx, &sdk.DeleteBranchInput{
		RepositoryName: new("demo"), BranchName: new("release/1.0"),
	})
	if err != nil || aws.ToString(deleted.DeletedBranch.CommitId) != tip {
		t.Fatal(deleted, err)
	}

	if got := g.must(work, "ls-remote", "--heads", "origin", "release/1.0"); got != "" {
		t.Fatal(got)
	}

	// Deleting a branch that is gone succeeds and returns nothing.
	deleted, err = c.DeleteBranch(
		ctx,
		&sdk.DeleteBranchInput{RepositoryName: new("demo"), BranchName: new("release/1.0")},
	)
	if err != nil || deleted.DeletedBranch != nil {
		t.Fatal(deleted, err)
	}

	// The default branch cannot be deleted, through the API or git, until another is the default.
	_, err = c.DeleteBranch(ctx, &sdk.DeleteBranchInput{RepositoryName: new("demo"), BranchName: new("main")})
	if codeCommitCode(t, err) != "DefaultBranchCannotBeDeletedException" {
		t.Fatal(err)
	}

	if out, pushErr := g.run(work, "push", "origin", ":main"); pushErr == nil {
		t.Fatalf("git deleted the default branch: %s", out)
	}

	_, err = c.UpdateDefaultBranch(ctx, &sdk.UpdateDefaultBranchInput{
		RepositoryName: new("demo"), DefaultBranchName: new("nope"),
	})
	if codeCommitCode(t, err) != "BranchDoesNotExistException" {
		t.Fatal(err)
	}

	if _, err = c.UpdateDefaultBranch(ctx, &sdk.UpdateDefaultBranchInput{
		RepositoryName: new("demo"), DefaultBranchName: new("feature"),
	}); err != nil {
		t.Fatal(err)
	}

	repo, _ = c.GetRepository(ctx, &sdk.GetRepositoryInput{RepositoryName: new("demo")})
	if aws.ToString(repo.RepositoryMetadata.DefaultBranch) != "feature" {
		t.Fatal(repo)
	}

	if got := g.must(
		work,
		"ls-remote",
		"--symref",
		"origin",
		"HEAD",
	); !strings.HasPrefix(
		got,
		"ref: refs/heads/feature\tHEAD",
	) {
		t.Fatal(got)
	}

	g.must(work, "push", "origin", ":main")
	_, err = c.DeleteBranch(ctx, &sdk.DeleteBranchInput{RepositoryName: new("demo"), BranchName: new("feature")})
	if codeCommitCode(t, err) != "DefaultBranchCannotBeDeletedException" {
		t.Fatal(err)
	}
}

func TestCodeCommitGitRenameAndDelete(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := codecommitClient(s, "test", "us-east-1")
	g := newGitHome(t)
	createRepo(t, c, "old-name")
	work := filepath.Join(g.home, "work")
	g.must(g.home, "init", "-q", work)
	g.write(work, "a.txt", "kept\n")
	g.must(work, "add", ".")
	g.must(work, "commit", "-m", "keep me")
	g.must(work, "push", s.URL+"/v1/repos/old-name", "HEAD:refs/heads/main")

	if _, err := c.UpdateRepositoryName(ctx, &sdk.UpdateRepositoryNameInput{
		OldName: new("old-name"), NewName: new("new-name"),
	}); err != nil {
		t.Fatal(err)
	}

	clone := filepath.Join(g.home, "clone")
	g.must(g.home, "clone", s.URL+"/v1/repos/new-name", clone)
	if got := g.must(clone, "log", "--format=%s"); got != "keep me" {
		t.Fatal(got)
	}

	file, err := c.GetFile(ctx, &sdk.GetFileInput{RepositoryName: new("new-name"), FilePath: new("a.txt")})
	if err != nil || string(file.FileContent) != "kept\n" {
		t.Fatal(file, err)
	}

	_, err = c.GetBranch(ctx, &sdk.GetBranchInput{RepositoryName: new("old-name"), BranchName: new("main")})
	if codeCommitCode(t, err) != "RepositoryDoesNotExistException" {
		t.Fatal(err)
	}

	out, cloneErr := g.run(g.home, "clone", s.URL+"/v1/repos/old-name", filepath.Join(g.home, "gone"))
	if cloneErr == nil || !strings.Contains(out, "not found") {
		t.Fatalf("the old name still serves a clone: %v %s", cloneErr, out)
	}

	if _, err = c.DeleteRepository(ctx, &sdk.DeleteRepositoryInput{RepositoryName: new("new-name")}); err != nil {
		t.Fatal(err)
	}

	out, cloneErr = g.run(g.home, "clone", s.URL+"/v1/repos/new-name", filepath.Join(g.home, "gone2"))
	if cloneErr == nil || !strings.Contains(out, "not found") {
		t.Fatalf("a deleted repository still serves a clone: %v %s", cloneErr, out)
	}

	// A push to a repository that does not exist is a 404 and creates nothing.
	out, pushErr := g.run(work, "push", s.URL+"/v1/repos/new-name", "HEAD:refs/heads/main")
	if pushErr == nil || !strings.Contains(out, "not found") {
		t.Fatalf("push to a deleted repository: %v %s", pushErr, out)
	}

	_, err = c.GetRepository(ctx, &sdk.GetRepositoryInput{RepositoryName: new("new-name")})
	if codeCommitCode(t, err) != "RepositoryDoesNotExistException" {
		t.Fatal(err)
	}

	// Recreating the name starts empty: the content went with the repository.
	createRepo(t, c, "new-name")
	if got := g.must(work, "ls-remote", s.URL+"/v1/repos/new-name"); got != "" {
		t.Fatalf("the recreated repository has refs: %s", got)
	}
}

func TestCodeCommitCommitAPI(t *testing.T) {
	ctx := context.Background()
	s := server(t)
	c := codecommitClient(s, "test", "us-east-1")
	newGitHome(t)
	createRepo(t, c, "api")
	repo := new("api")

	// The first commit of an empty repository creates the branch and makes it the default.
	first, err := c.PutFile(ctx, &sdk.PutFileInput{
		RepositoryName: repo,
		BranchName:     new("trunk"),
		FilePath:       new("src/main.go"),
		FileContent: []byte(
			"package main\n",
		),
		CommitMessage: new("init"),
		Name:          new("Ann"),
		Email:         new("ann@example.com"),
	})
	if err != nil {
		t.Fatal(err)
	}

	meta, _ := c.GetRepository(ctx, &sdk.GetRepositoryInput{RepositoryName: repo})
	if aws.ToString(meta.RepositoryMetadata.DefaultBranch) != "trunk" {
		t.Fatal(meta)
	}

	commit, err := c.GetCommit(ctx, &sdk.GetCommitInput{RepositoryName: repo, CommitId: first.CommitId})
	if err != nil || aws.ToString(commit.Commit.TreeId) != aws.ToString(first.TreeId) ||
		aws.ToString(commit.Commit.Message) != "init" {
		t.Fatal(commit, err)
	}

	tip := aws.ToString(first.CommitId)
	for name, tc := range map[string]struct {
		in   *sdk.PutFileInput
		code string
	}{
		"no parent": {
			&sdk.PutFileInput{BranchName: new("trunk"), FilePath: new("a"), FileContent: []byte("x")},
			"ParentCommitIdRequiredException",
		},
		"unknown parent": {
			&sdk.PutFileInput{
				BranchName: new("trunk"), FilePath: new("a"), FileContent: []byte("x"),
				ParentCommitId: new(strings.Repeat("a", 40)),
			}, "ParentCommitDoesNotExistException",
		},
		"missing branch": {
			&sdk.PutFileInput{
				BranchName: new("nope"), FilePath: new("a"), FileContent: []byte("x"), ParentCommitId: &tip,
			}, "BranchDoesNotExistException",
		},
		"same content": {
			&sdk.PutFileInput{
				BranchName: new("trunk"), FilePath: new("src/main.go"), FileContent: []byte("package main\n"),
				ParentCommitId: &tip,
			}, "SameFileContentException",
		},
		"file under a file": {
			&sdk.PutFileInput{
				BranchName: new("trunk"), FilePath: new("src/main.go/x"), FileContent: []byte("x"), ParentCommitId: &tip,
			}, "DirectoryNameConflictsWithFileNameException",
		},
		"file over a folder": {
			&sdk.PutFileInput{
				BranchName: new("trunk"), FilePath: new("src"), FileContent: []byte("x"), ParentCommitId: &tip,
			}, "FileNameConflictsWithDirectoryNameException",
		},
		"bad path": {
			&sdk.PutFileInput{
				BranchName: new("trunk"), FilePath: new("a/../b"), FileContent: []byte("x"), ParentCommitId: &tip,
			}, "InvalidPathException",
		},
		"bad branch": {
			&sdk.PutFileInput{
				BranchName: new("a..b"), FilePath: new("a"), FileContent: []byte("x"), ParentCommitId: &tip,
			}, "InvalidBranchNameException",
		},
		"too large": {
			&sdk.PutFileInput{
				BranchName: new("trunk"), FilePath: new("a"), FileContent: make([]byte, 6<<20+1), ParentCommitId: &tip,
			}, "FileContentSizeLimitExceededException",
		},
		"bad parent id": {
			&sdk.PutFileInput{
				BranchName: new("trunk"), FilePath: new("a"), FileContent: []byte("x"), ParentCommitId: new("zz"),
			}, "InvalidParentCommitIdException",
		},
	} {
		tc.in.RepositoryName = repo
		if _, putErr := c.PutFile(ctx, tc.in); codeCommitCode(t, putErr) != tc.code {
			t.Errorf("%s: %v, want %s", name, putErr, tc.code)
		}
	}

	// An empty file is a file.
	empty, err := c.PutFile(ctx, &sdk.PutFileInput{
		RepositoryName: repo, BranchName: new("trunk"), FilePath: new(".gitkeep"), FileContent: []byte{},
		ParentCommitId: &tip, FileMode: types.FileModeTypeEnumExecutable,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.GetFile(ctx, &sdk.GetFileInput{
		RepositoryName: repo, FilePath: new(".gitkeep"), CommitSpecifier: empty.CommitId,
	})
	if err != nil || len(got.FileContent) != 0 || got.FileMode != types.FileModeTypeEnumExecutable {
		t.Fatal(got, err)
	}

	// A stale parent loses.
	_, err = c.PutFile(ctx, &sdk.PutFileInput{
		RepositoryName: repo,
		BranchName:     new("trunk"),
		FilePath:       new("late"),
		FileContent:    []byte("x"),
		ParentCommitId: &tip,
	})
	if codeCommitCode(t, err) != "ParentCommitIdOutdatedException" {
		t.Fatal(err)
	}

	tip = aws.ToString(empty.CommitId)

	// CreateCommit adds, updates, deletes and changes modes in one commit.
	created, err := c.CreateCommit(ctx, &sdk.CreateCommitInput{
		RepositoryName: repo, BranchName: new("trunk"), ParentCommitId: &tip, CommitMessage: new("batch"),
		AuthorName: new("Bo"), Email: new("bo@example.com"),
		PutFiles: []types.PutFileEntry{
			{FilePath: new("docs/a.md"), FileContent: []byte("a")},
			{FilePath: new("src/main.go"), FileContent: []byte("package app\n")},
		},
		DeleteFiles: []types.DeleteFileEntry{{FilePath: new(".gitkeep")}},
	})
	if err != nil || len(created.FilesAdded) != 1 || len(created.FilesUpdated) != 1 || len(created.FilesDeleted) != 1 ||
		aws.ToString(created.FilesAdded[0].AbsolutePath) != "docs/a.md" {
		t.Fatal(created, err)
	}

	tip = aws.ToString(created.CommitId)
	modes, err := c.CreateCommit(ctx, &sdk.CreateCommitInput{
		RepositoryName: repo,
		BranchName:     new("trunk"),
		ParentCommitId: &tip,
		SetFileModes: []types.SetFileModeEntry{
			{FilePath: new("docs/a.md"), FileMode: types.FileModeTypeEnumExecutable},
		},
	})
	if err != nil || len(modes.FilesUpdated) != 1 ||
		modes.FilesUpdated[0].FileMode != types.FileModeTypeEnumExecutable {
		t.Fatal(modes, err)
	}

	tip = aws.ToString(modes.CommitId)
	for name, tc := range map[string]struct {
		in   *sdk.CreateCommitInput
		code string
	}{
		"nothing": {&sdk.CreateCommitInput{BranchName: new("trunk"), ParentCommitId: &tip}, "FileEntryRequiredException"},
		"no change": {
			&sdk.CreateCommitInput{
				BranchName: new("trunk"), ParentCommitId: &tip,
				SetFileModes: []types.SetFileModeEntry{
					{FilePath: new("docs/a.md"), FileMode: types.FileModeTypeEnumExecutable},
				},
			}, "NoChangeException",
		},
		"delete missing": {
			&sdk.CreateCommitInput{
				BranchName: new("trunk"), ParentCommitId: &tip, DeleteFiles: []types.DeleteFileEntry{{FilePath: new("nope")}},
			}, "FileDoesNotExistException",
		},
		"same path twice": {
			&sdk.CreateCommitInput{
				BranchName: new("trunk"), ParentCommitId: &tip,
				PutFiles:    []types.PutFileEntry{{FilePath: new("x"), FileContent: []byte("1")}},
				DeleteFiles: []types.DeleteFileEntry{{FilePath: new("x")}},
			}, "SamePathRequestException",
		},
		"file and folder": {
			&sdk.CreateCommitInput{
				BranchName: new("trunk"), ParentCommitId: &tip,
				PutFiles: []types.PutFileEntry{
					{FilePath: new("x"), FileContent: []byte("1")}, {FilePath: new("x/y"), FileContent: []byte("2")},
				},
			}, "PutFileEntryConflictException",
		},
		"no parent": {
			&sdk.CreateCommitInput{
				BranchName: new("trunk"), PutFiles: []types.PutFileEntry{{FilePath: new("x"), FileContent: []byte("1")}},
			}, "ParentCommitIdRequiredException",
		},
		"too many": {
			&sdk.CreateCommitInput{BranchName: new("trunk"), ParentCommitId: &tip, PutFiles: manyEntries(101)},
			"MaximumFileEntriesExceededException",
		},
	} {
		tc.in.RepositoryName = repo
		if _, err = c.CreateCommit(ctx, tc.in); codeCommitCode(t, err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}

	// GetFile, GetFolder and GetCommit report what is missing or invalid.
	for name, tc := range map[string]struct {
		call func() error
		code string
	}{
		"file is a folder": {func() error {
			_, e := c.GetFile(ctx, &sdk.GetFileInput{RepositoryName: repo, FilePath: new("src")})
			return e
		}, "FileDoesNotExistException"},
		"unknown specifier": {func() error {
			_, e := c.GetFile(ctx, &sdk.GetFileInput{
				RepositoryName: repo, FilePath: new("docs/a.md"), CommitSpecifier: new("nope"),
			})
			return e
		}, "CommitDoesNotExistException"},
		"option as specifier": {func() error {
			_, e := c.GetFile(ctx, &sdk.GetFileInput{
				RepositoryName: repo, FilePath: new("a"), CommitSpecifier: new("--output=x"),
			})
			return e
		}, "InvalidCommitException"},
		"folder is a file": {func() error {
			_, e := c.GetFolder(ctx, &sdk.GetFolderInput{RepositoryName: repo, FolderPath: new("src/main.go")})
			return e
		}, "FolderDoesNotExistException"},
		"unknown commit": {func() error {
			_, e := c.GetCommit(ctx, &sdk.GetCommitInput{RepositoryName: repo, CommitId: new(strings.Repeat("0", 40))})
			return e
		}, "CommitIdDoesNotExistException"},
		"short commit": {func() error {
			_, e := c.GetCommit(ctx, &sdk.GetCommitInput{RepositoryName: repo, CommitId: new("short")})
			return e
		}, "InvalidCommitIdException"},
		"get branch without a name": {func() error {
			_, e := c.GetBranch(ctx, &sdk.GetBranchInput{RepositoryName: repo})
			return e
		}, "BranchNameRequiredException"},
		"get branch without a repository": {func() error {
			_, e := c.GetBranch(ctx, &sdk.GetBranchInput{BranchName: new("trunk")})
			return e
		}, "RepositoryNameRequiredException"},
		"get branch in an unknown repository": {func() error {
			_, e := c.GetBranch(ctx, &sdk.GetBranchInput{RepositoryName: new("missing"), BranchName: new("trunk")})
			return e
		}, "RepositoryDoesNotExistException"},
		"bad list token": {func() error {
			_, e := c.ListBranches(ctx, &sdk.ListBranchesInput{RepositoryName: repo, NextToken: new("!")})
			return e
		}, "InvalidContinuationTokenException"},
	} {
		if got := codeCommitCode(t, tc.call()); got != tc.code {
			t.Errorf("%s: %q, want %s", name, got, tc.code)
		}
	}

	// GetDifferences between commits, paged in path order.
	diffs, err := c.GetDifferences(ctx, &sdk.GetDifferencesInput{
		RepositoryName: repo, BeforeCommitSpecifier: first.CommitId, AfterCommitSpecifier: new("trunk"),
	})
	if err != nil || len(diffs.Differences) != 2 {
		t.Fatal(diffs, err)
	}

	if d := diffs.Differences[0]; d.ChangeType != types.ChangeTypeEnumAdded ||
		aws.ToString(
			d.AfterBlob.Path,
		) != "docs/a.md" || aws.ToString(d.AfterBlob.Mode) != "100755" || d.BeforeBlob != nil {
		t.Fatal(d)
	}

	if d := diffs.Differences[1]; d.ChangeType != types.ChangeTypeEnumModified ||
		aws.ToString(d.AfterBlob.Path) != "src/main.go" || d.BeforeBlob == nil ||
		aws.ToString(d.BeforeBlob.BlobId) == aws.ToString(d.AfterBlob.BlobId) {
		t.Fatal(d)
	}

	page, err := c.GetDifferences(ctx, &sdk.GetDifferencesInput{
		RepositoryName: repo, AfterCommitSpecifier: new("trunk"), MaxResults: new(int32(1)),
	})
	if err != nil || len(page.Differences) != 1 || page.NextToken == nil {
		t.Fatal(page, err)
	}

	next, err := c.GetDifferences(ctx, &sdk.GetDifferencesInput{
		RepositoryName: repo, AfterCommitSpecifier: new("trunk"), MaxResults: new(int32(1)), NextToken: page.NextToken,
	})
	if err != nil || len(next.Differences) != 1 || next.NextToken != nil ||
		aws.ToString(next.Differences[0].AfterBlob.Path) != "src/main.go" {
		t.Fatal(next, err)
	}

	removed, err := c.GetDifferences(ctx, &sdk.GetDifferencesInput{
		RepositoryName: repo, BeforeCommitSpecifier: &tip, AfterCommitSpecifier: first.CommitId, AfterPath: new("docs"),
	})
	if err != nil || len(removed.Differences) != 1 ||
		removed.Differences[0].ChangeType != types.ChangeTypeEnumDeleted ||
		removed.Differences[0].AfterBlob != nil {
		t.Fatal(removed, err)
	}

	for name, tc := range map[string]struct {
		in   *sdk.GetDifferencesInput
		code string
	}{
		"missing": {&sdk.GetDifferencesInput{AfterCommitSpecifier: new("nope")}, "CommitDoesNotExistException"},
		"bad path": {
			&sdk.GetDifferencesInput{AfterCommitSpecifier: new("trunk"), AfterPath: new("zzz")},
			"PathDoesNotExistException",
		},
		"bad max": {
			&sdk.GetDifferencesInput{AfterCommitSpecifier: new("trunk"), MaxResults: new(int32(0))},
			"InvalidMaxResultsException",
		},
		"bad token": {
			&sdk.GetDifferencesInput{AfterCommitSpecifier: new("trunk"), NextToken: new("!")},
			"InvalidContinuationTokenException",
		},
		"moved folder": {
			&sdk.GetDifferencesInput{AfterCommitSpecifier: new("trunk"), BeforePath: new("a"), AfterPath: new("b")},
			"NotImplemented",
		},
	} {
		tc.in.RepositoryName = repo
		if _, err = c.GetDifferences(ctx, tc.in); codeCommitCode(t, err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}

	// ListBranches lists in name order.
	for _, name := range []string{"b2", "b1", "b3"} {
		if _, err = c.CreateBranch(ctx, &sdk.CreateBranchInput{
			RepositoryName: repo, BranchName: new(name), CommitId: &tip,
		}); err != nil {
			t.Fatal(err)
		}
	}

	listed, err := c.ListBranches(ctx, &sdk.ListBranchesInput{RepositoryName: repo})
	if err != nil || strings.Join(listed.Branches, ",") != "b1,b2,b3,trunk" {
		t.Fatal(listed, err)
	}
}

func manyEntries(n int) []types.PutFileEntry {
	entries := make([]types.PutFileEntry, n)
	for i := range entries {
		entries[i] = types.PutFileEntry{FilePath: new("many/" + strings.Repeat("x", i+1)), FileContent: []byte("x")}
	}

	return entries
}

func TestCodeCommitGitTagsAndPersistence(t *testing.T) {
	ctx := context.Background()
	g := newGitHome(t)
	dir := t.TempDir()
	s, stop := persistent(t, dir, &fakeRuntime{})
	c := codecommitClient(s, "test", "us-east-1")
	createRepo(t, c, "kept")
	work := filepath.Join(g.home, "work")
	g.must(g.home, "init", "-q", work)
	g.write(work, "a.txt", "persisted\n")
	g.must(work, "add", ".")
	g.must(work, "commit", "-m", "persist")
	g.must(work, "tag", "v1")
	g.must(work, "push", s.URL+"/v1/repos/kept", "HEAD:refs/heads/main", "v1")
	tip := g.must(work, "rev-parse", "HEAD")

	// A tag name cannot be a branch.
	_, err := c.PutFile(ctx, &sdk.PutFileInput{
		RepositoryName: new(
			"kept",
		),
		BranchName:     new("v1"),
		FilePath:       new("b"),
		FileContent:    []byte("b"),
		ParentCommitId: &tip,
	})
	if codeCommitCode(t, err) != "BranchNameIsTagNameException" {
		t.Fatal(err)
	}

	// Tags resolve as commit specifiers.
	tagged, err := c.GetFile(ctx, &sdk.GetFileInput{
		RepositoryName: new("kept"), FilePath: new("a.txt"), CommitSpecifier: new("v1"),
	})
	if err != nil || aws.ToString(tagged.CommitId) != tip {
		t.Fatal(tagged, err)
	}

	stop()
	s, stop = persistent(t, dir, &fakeRuntime{})
	defer stop()
	c = codecommitClient(s, "test", "us-east-1")
	if got := mainTip(t, c, "kept"); got != tip {
		t.Fatalf("after restart the tip is %s, want %s", got, tip)
	}

	meta, err := c.GetRepository(ctx, &sdk.GetRepositoryInput{RepositoryName: new("kept")})
	if err != nil || aws.ToString(meta.RepositoryMetadata.DefaultBranch) != "main" {
		t.Fatal(meta, err)
	}

	clone := filepath.Join(g.home, "clone")
	g.must(g.home, "clone", s.URL+"/v1/repos/kept", clone)
	if got := g.must(clone, "log", "--format=%s"); got != "persist" {
		t.Fatal(got)
	}

	// A reset removes the content with the records.
	reset, err := http.Post(s.URL+"/_tinystack/reset", "", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}

	_ = reset.Body.Close()
	if reset.StatusCode != http.StatusOK {
		t.Fatal(reset.Status)
	}

	if entries, readErr := os.ReadDir(filepath.Join(dir, "codecommit")); readErr == nil && len(entries) > 0 {
		t.Fatalf("reset left repository content: %v", entries)
	}

	createRepo(t, c, "kept")
	if got := g.must(work, "ls-remote", s.URL+"/v1/repos/kept"); got != "" {
		t.Fatalf("a repository created after a reset has refs: %s", got)
	}
}

func TestCodeCommitGitHostRouting(t *testing.T) {
	s := server(t)
	west := codecommitClient(s, "test", "us-west-2")
	createRepo(t, west, "regional")
	get := func(host, path string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()
		return resp.StatusCode
	}

	const refs = "/v1/repos/regional/info/refs?service=git-upload-pack"
	const awsHost = "git-codecommit.us-west-2.amazonaws.com"
	// The AWS git host names the Region; the main port uses the default Region, us-east-1.
	if got := get(awsHost, refs); got != http.StatusOK {
		t.Errorf("repository in the host's Region: %d", got)
	}

	if got := get("git-codecommit.us-east-1.amazonaws.com", refs); got != http.StatusNotFound {
		t.Errorf("repository in another Region: %d", got)
	}

	if got := get(awsHost, "/other/regional"); got != http.StatusNotFound {
		t.Errorf("non-repository path on the git host: %d", got)
	}

	if got := get(strings.TrimPrefix(s.URL, "http://"), refs); got != http.StatusNotFound {
		t.Errorf("repository outside the default Region: %d", got)
	}

	// The dumb protocol is refused, and a repository name is validated.
	if got := get(awsHost, "/v1/repos/regional/info/refs"); got != http.StatusForbidden {
		t.Errorf("dumb protocol: %d", got)
	}

	if got := get(awsHost, "/v1/repos/bad%20name/info/refs?service=git-upload-pack"); got != http.StatusNotFound {
		t.Errorf("invalid name: %d", got)
	}
}

func TestCodeCommitGitAccounts(t *testing.T) {
	s := server(t)
	g := newGitHome(t)
	other := codecommitClient(s, "111111111111", "us-east-1")
	createRepo(t, other, "theirs")
	work := filepath.Join(g.home, "work")
	g.must(g.home, "init", "-q", work)
	g.write(work, "a", "a")
	g.must(work, "add", ".")
	g.must(work, "commit", "-m", "x")

	// The default account does not have the repository; the account named by the Basic user does.
	out, err := g.run(work, "push", s.URL+"/v1/repos/theirs", "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(out, "not found") {
		t.Fatalf("push in the default account: %v %s", err, out)
	}

	// git sends credentials only after a challenge, which parse mode never issues, so send them up front.
	basic := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("111111111111:x"))
	g.must(work, "-c", "http.extraHeader="+basic, "push", s.URL+"/v1/repos/theirs", "HEAD:refs/heads/main")
	if got := mainTip(t, other, "theirs"); got != g.must(work, "rev-parse", "HEAD") {
		t.Fatal(got)
	}
}

func TestCodeCommitGitVerifyMode(t *testing.T) {
	cfg := testConfig()
	cfg.Identity.Mode = config.IdentityVerify
	cfg.Identity.SecretKeys = map[string]string{"test": "secret"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	c := sdk.New(sdk.Options{
		Region: "us-east-1", BaseEndpoint: new(s.URL), Retryer: aws.NopRetryer{},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "secret"}, nil
		}),
	})
	createRepo(t, c, "locked")
	hostname, _, _ := strings.Cut(strings.TrimPrefix(s.URL, "http://"), ":")
	get := func(user, password string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			s.URL+"/v1/repos/locked/info/refs?service=git-upload-pack", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}

		if user != "" {
			req.SetBasicAuth(user, password)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()
		return resp.StatusCode
	}

	sign := func(secret string, at time.Time, region, name string) string {
		return gitPassword(secret, at, region, name, hostname)
	}

	now := time.Now()
	if got := get("", ""); got != http.StatusUnauthorized {
		t.Errorf("no credentials: %d", got)
	}

	if got := get("test", sign("secret", now, "us-east-1", "locked")); got != http.StatusOK {
		t.Errorf("valid signature: %d", got)
	}

	for name, password := range map[string]string{
		"wrong secret": sign("wrong", now, "us-east-1", "locked"),
		"wrong region": sign("secret", now, "us-west-2", "locked"),
		"wrong repo":   sign("secret", now, "us-east-1", "other"),
		"stale":        sign("secret", now.Add(-time.Hour), "us-east-1", "locked"),
		"malformed":    "not-a-signature",
	} {
		if got := get("test", password); got != http.StatusForbidden {
			t.Errorf("%s: %d", name, got)
		}
	}

	if got := get("unknown", sign("secret", now, "us-east-1", "locked")); got != http.StatusForbidden {
		t.Errorf("unknown key: %d", got)
	}

	// The AWS CLI's own credential helper, when installed, derives a password the server accepts.
	if _, err := exec.LookPath("aws"); err != nil {
		return
	}

	g := newGitHome(t)
	helper := "!aws codecommit credential-helper $@ --ignore-host-check"
	cli := func(secret string) error {
		g.env = append(g.env, "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY="+secret, "AWS_DEFAULT_REGION=us-east-1",
			"AWS_CONFIG_FILE="+os.DevNull, "AWS_SHARED_CREDENTIALS_FILE="+os.DevNull)
		_, err := g.run(g.home, "-c", "credential.helper="+helper, "-c", "credential.useHttpPath=true",
			"ls-remote", s.URL+"/v1/repos/locked")
		return err
	}

	if err := cli("secret"); err != nil {
		t.Fatalf("the AWS CLI credential helper was refused: %v", err)
	}

	if err := cli("wrong"); err == nil {
		t.Fatal("the AWS CLI credential helper with the wrong secret was accepted")
	}
}

// gitPassword is the password aws codecommit credential-helper derives: the time, "Z" and a SigV4 signature of a
// GIT request for the repository path.
func gitPassword(secret string, at time.Time, region, name, hostname string) string {
	stamp := at.UTC().Format("20060102T150405")
	canonical := "GIT\n/v1/repos/" + name + "\n\nhost:" + hostname + "\n\nhost\n"
	digest := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + stamp[:8] + "/" + region + "/codecommit/aws4_request\n" +
		hex.EncodeToString(digest[:])
	key := mac([]byte("AWS4"+secret), stamp[:8])
	key = mac(key, region)
	key = mac(key, "codecommit")
	key = mac(key, "aws4_request")
	return stamp + "Z" + hex.EncodeToString(mac(key, toSign))
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func TestCodeCommitEventsOnPush(t *testing.T) {
	ctx := context.Background()
	g := newGitHome(t)
	s := server(t)
	c := codecommitClient(s, "test", "us-east-1")
	createRepo(t, c, "events")
	queues := sqsClient(s, "test")
	queueURL, queueArn := queueARN(t, queues, "repo-events")
	bus := eventsClient(s, "test")
	putRule(t, bus, "repo-state",
		`{"source":["aws.codecommit"],"detail-type":["CodeCommit Repository State Change"]}`)
	if _, err := bus.PutTargets(ctx, &eventbridge.PutTargetsInput{
		Rule: new("repo-state"), Targets: []ebtypes.Target{{Id: new("q"), Arn: &queueArn}},
	}); err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(g.home, "work")
	g.must(g.home, "init", "-q", work)
	g.write(work, "a", "a")
	g.must(work, "add", ".")
	g.must(work, "commit", "-m", "x")
	g.must(work, "push", s.URL+"/v1/repos/events", "HEAD:refs/heads/main")
	tip := g.must(work, "rev-parse", "HEAD")
	if _, err := c.CreateBranch(ctx, &sdk.CreateBranchInput{
		RepositoryName: new("events"), BranchName: new("dev"), CommitId: &tip,
	}); err != nil {
		t.Fatal(err)
	}

	g.write(work, "b", "b")
	g.must(work, "add", ".")
	g.must(work, "commit", "-m", "y")
	g.must(work, "push", s.URL+"/v1/repos/events", "HEAD:refs/heads/dev")
	g.must(work, "push", s.URL+"/v1/repos/events", ":dev")

	var bodies []string
	for range 20 {
		out, err := queues.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: &queueURL, MaxNumberOfMessages: 10})
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range out.Messages {
			bodies = append(bodies, aws.ToString(m.Body))
		}

		if len(bodies) >= 4 {
			break
		}
	}

	text := strings.Join(bodies, "\n")
	for _, want := range []string{
		`"event":"referenceCreated"`, `"event":"referenceUpdated"`, `"event":"referenceDeleted"`,
		`"referenceFullName":"refs/heads/main"`, `"referenceFullName":"refs/heads/dev"`,
		`"referenceType":"branch"`, `"repositoryName":"events"`, `"oldCommitId":"` + tip + `"`,
		`"detail-type":"CodeCommit Repository State Change"`, `"source":"aws.codecommit"`,
		"arn:aws:codecommit:us-east-1:000000000000:events",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("events lack %s:\n%s", want, text)
		}
	}

	if len(bodies) != 4 {
		t.Errorf("got %d events, want 4 (create main, create dev, update dev, delete dev):\n%s", len(bodies), text)
	}
}
