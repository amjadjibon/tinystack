package codecommit

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestValidRefName(t *testing.T) {
	for name, want := range map[string]bool{
		"main": true, "feature/login": true, "release-1.0": true, "v1.2.3": true, "a@b": true,
		"": false, "@": false, "/a": false, "a/": false, "a//b": false, "a..b": false, "a.": false, "-a": false,
		"a b": false, "a~1": false, "a^": false, "a:b": false, "a?": false, "a*": false, "a[": false, `a\b`: false,
		"a@{b": false, "a\x01": false, ".hidden": false, "dir/.hidden": false, "x.lock": false, "dir.lock/x": false,
		strings.Repeat("a", 256): true, strings.Repeat("a", 257): false,
	} {
		if got := validRefName(name); got != want {
			t.Errorf("validRefName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestValidatePath(t *testing.T) {
	for path, want := range map[string]string{"a": "a", "/a/b.txt": "a/b.txt", "dir/.gitkeep": "dir/.gitkeep"} {
		got, err := validatePath(&path)
		if err != nil || got != want {
			t.Errorf("validatePath(%q) = %q, %v, want %q", path, got, err, want)
		}
	}

	for _, path := range []string{"/", "a/", "a//b", "./a", "a/..", "../a", ".git/config", "a/.GIT/x", "a\x00b", "a\nb"} {
		if _, err := validatePath(&path); err == nil {
			t.Errorf("validatePath(%q) accepted", path)
		}
	}

	wantError(t, func() error { _, err := validatePath(nil); return err }(), "PathRequiredException")
}

func TestValidateRevision(t *testing.T) {
	for _, spec := range []string{"main", "feature/x", "refs/heads/main", "HEAD", strings.Repeat("a", 40), "v1.0"} {
		if err := validateRevision(spec); err != nil {
			t.Errorf("validateRevision(%q): %v", spec, err)
		}
	}

	for _, spec := range []string{
		"--output=x", "-h", "a..b", "a@{1}", "a b", "main~1", "main^", "main:file", "a/", "x.lock",
	} {
		wantError(t, validateRevision(spec), "InvalidCommitException")
	}
}

func TestParseCommit(t *testing.T) {
	raw := "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"parent aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nparent bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
		"author Ann Dev <ann@example.com> 1700000000 -0800\ncommitter Bo <bo@example.com> 1700000001 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n\nsubject\n\nbody\n"
	c := parseCommit("cccccccccccccccccccccccccccccccccccccccc", raw)
	if *c.TreeId != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" || len(c.Parents) != 2 || c.Parents[1][0] != 'b' ||
		*c.Message != "subject\n\nbody\n" {
		t.Fatal(c)
	}

	if *c.Author.Name != "Ann Dev" || *c.Author.Email != "ann@example.com" || *c.Author.Date != "1700000000 -0800" ||
		*c.Committer.Name != "Bo" || *c.Committer.Date != "1700000001 +0000" {
		t.Fatal(c.Author, c.Committer)
	}

	root := parseCommit(
		"cccccccccccccccccccccccccccccccccccccccc",
		"tree x\nauthor A <a> 1 +0000\ncommitter A <a> 1 +0000\n\nm",
	)
	if root.Parents == nil || len(root.Parents) != 0 {
		t.Fatal("a root commit has an empty parent list")
	}
}

func TestParseDifferences(t *testing.T) {
	raw := ":000000 100644 " + strings.Repeat("0", 40) + " " + strings.Repeat("a", 40) + " A\x00new.txt\x00" +
		":100644 100755 " + strings.Repeat("b", 40) + " " + strings.Repeat("c", 40) + " M\x00dir/run.sh\x00" +
		":100644 000000 " + strings.Repeat("d", 40) + " " + strings.Repeat("0", 40) + " D\x00old.txt\x00"
	diffs := parseDifferences(raw)
	if len(diffs) != 3 {
		t.Fatal(diffs)
	}

	if *diffs[0].ChangeType != "A" || diffs[0].BeforeBlob != nil || *diffs[0].AfterBlob.Path != "new.txt" ||
		*diffs[0].AfterBlob.Mode != "100644" {
		t.Fatal(diffs[0])
	}

	if *diffs[1].ChangeType != "M" || *diffs[1].BeforeBlob.Mode != "100644" || *diffs[1].AfterBlob.Mode != "100755" {
		t.Fatal(diffs[1])
	}

	if *diffs[2].ChangeType != "D" || diffs[2].AfterBlob != nil || *diffs[2].BeforeBlob.Path != "old.txt" {
		t.Fatal(diffs[2])
	}

	if len(parseDifferences("")) != 0 {
		t.Fatal("no output has no differences")
	}
}

// TestRequiredMembers runs the checks that SDK clients make themselves through the generated operations.
func TestRequiredMembers(t *testing.T) {
	svc := New(memory.New(nil, nil))
	t.Cleanup(svc.Stop)
	ctx := localContext()
	if _, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{RepositoryName: new("repo")}); err != nil {
		t.Fatal(err)
	}

	ops := api.Descriptor(svc).Operations
	repo := new("repo")
	for name, tc := range map[string]struct {
		op   string
		in   any
		code string
	}{
		"no repository": {"GetBranch", &api.GetBranchInput{BranchName: new("main")}, "RepositoryNameRequiredException"},
		"no branch":     {"GetBranch", &api.GetBranchInput{RepositoryName: repo}, "BranchNameRequiredException"},
		"bad repository": {
			"GetBranch", &api.GetBranchInput{RepositoryName: new("a b"), BranchName: new("main")},
			"InvalidRepositoryNameException",
		},
		"bad branch": {
			"GetBranch", &api.GetBranchInput{RepositoryName: repo, BranchName: new("a..b")}, "InvalidBranchNameException",
		},
		"no commit to branch": {
			"CreateBranch", &api.CreateBranchInput{RepositoryName: repo, BranchName: new("x")}, "CommitIdRequiredException",
		},
		"invalid commit to branch": {
			"CreateBranch", &api.CreateBranchInput{RepositoryName: repo, BranchName: new("x"), CommitId: new("zz")},
			"InvalidCommitIdException",
		},
		"no commit id": {"GetCommit", &api.GetCommitInput{RepositoryName: repo}, "CommitIdRequiredException"},
		"no file path": {"GetFile", &api.GetFileInput{RepositoryName: repo}, "PathRequiredException"},
		"no folder path": {
			"GetFolder", &api.GetFolderInput{RepositoryName: repo}, "PathRequiredException",
		},
		"no after commit": {
			"GetDifferences", &api.GetDifferencesInput{RepositoryName: repo}, "CommitRequiredException",
		},
		"no default branch name": {
			"UpdateDefaultBranch", &api.UpdateDefaultBranchInput{RepositoryName: repo}, "BranchNameRequiredException",
		},
		"no content": {
			"PutFile", &api.PutFileInput{RepositoryName: repo, BranchName: new("main"), FilePath: new("a")},
			"FileContentRequiredException",
		},
		"no entries": {
			"CreateCommit", &api.CreateCommitInput{RepositoryName: repo, BranchName: new("main")}, "FileEntryRequiredException",
		},
		"no entry content": {
			"CreateCommit", &api.CreateCommitInput{
				RepositoryName: repo, BranchName: new("main"), PutFiles: []api.PutFileEntry{{FilePath: new("a")}},
			}, "SourceFileOrContentRequiredException",
		},
		"no mode": {
			"CreateCommit", &api.CreateCommitInput{
				RepositoryName: repo, BranchName: new("main"), SetFileModes: []api.SetFileModeEntry{{FilePath: new("a")}},
			}, "FileModeRequiredException",
		},
		"invalid mode": {
			"CreateCommit", &api.CreateCommitInput{
				RepositoryName: repo, BranchName: new("main"),
				PutFiles: []api.PutFileEntry{{FilePath: new("a"), FileContent: []byte("x"), FileMode: new("FAST")}},
			}, "InvalidFileModeException",
		},
		"message too long": {
			"CreateCommit", &api.CreateCommitInput{
				RepositoryName: repo, BranchName: new("main"), CommitMessage: new(strings.Repeat("m", 256<<10+1)),
				PutFiles: []api.PutFileEntry{{FilePath: new("a"), FileContent: []byte("x")}},
			}, "CommitMessageLengthExceededException",
		},
		"invalid email": {
			"CreateCommit", &api.CreateCommitInput{
				RepositoryName: repo, BranchName: new("main"), Email: new("a b"),
				PutFiles: []api.PutFileEntry{{FilePath: new("a"), FileContent: []byte("x")}},
			}, "InvalidEmailException",
		},
	} {
		_, err := ops[tc.op].Invoke(ctx, tc.in)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}

		if !strings.Contains(err.Error(), tc.code) {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
}

// TestReadAPI exercises what other services use to read a repository: a branch's commit, a file and a zip.
func TestReadAPI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}

	svc := New(memory.New(nil, nil))
	t.Cleanup(svc.Stop)
	ctx := localContext()
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	if _, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{RepositoryName: new("repo")}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Branch(ctx, scope, "repo", "main"); err == nil {
		t.Fatal("an empty repository has no branch")
	}

	put, err := svc.CreateCommit(ctx, &api.CreateCommitInput{
		RepositoryName: new("repo"), BranchName: new("main"),
		PutFiles: []api.PutFileEntry{
			{FilePath: new("a.txt"), FileContent: []byte("alpha")},
			{FilePath: new("dir/b.txt"), FileContent: []byte("beta")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tip, err := svc.Branch(ctx, scope, "repo", "main")
	if err != nil || tip != *put.CommitId {
		t.Fatal(tip, err)
	}

	file, err := svc.File(ctx, scope, "repo", new("main"), "dir/b.txt")
	if err != nil || string(file.Content) != "beta" || file.CommitID != tip || file.Mode != modeNormal {
		t.Fatal(file, err)
	}

	var archive bytes.Buffer
	if err = svc.Archive(ctx, scope, "repo", tip, &archive); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}

	contents := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}

		r, openErr := f.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}

		b, _ := io.ReadAll(r)
		_ = r.Close()
		contents[f.Name] = string(b)
	}

	if len(contents) != 2 || contents["a.txt"] != "alpha" || contents["dir/b.txt"] != "beta" {
		t.Fatal(contents)
	}

	if err = svc.Archive(ctx, scope, "other", tip, io.Discard); err == nil {
		t.Fatal("an unknown repository has no archive")
	}
}

func TestContentLifecycle(t *testing.T) {
	root := t.TempDir()
	svc := New(memory.New(nil, nil))
	svc.UseDataDir(root)
	ctx := localContext()
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	out, err := svc.CreateRepository(ctx, &api.CreateRepositoryInput{RepositoryName: new("repo")})
	if err != nil {
		t.Fatal(err)
	}

	dir, err := svc.repositoryDir(scope, *out.RepositoryMetadata.RepositoryId)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(dir, root) || !strings.HasSuffix(dir, *out.RepositoryMetadata.RepositoryId+".git") {
		t.Fatalf("content is keyed by repository ID under the data directory: %s", dir)
	}

	// Nothing is on disk until something is written.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("a new repository has content: %v", statErr)
	}

	r, err := svc.lookup(ctx, scope, "repo")
	if err != nil {
		t.Fatal(err)
	}

	if err = r.ensure(); err != nil {
		t.Fatal(err)
	}

	if !r.exists() {
		t.Fatal("ensure creates the repository")
	}

	// Renaming moves no files; deleting removes them.
	if _, err = svc.UpdateRepositoryName(
		ctx,
		&api.UpdateRepositoryNameInput{OldName: new("repo"), NewName: new("renamed")},
	); err != nil {
		t.Fatal(err)
	}

	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("renaming moved the content: %v", statErr)
	}

	if _, err = svc.DeleteRepository(ctx, &api.DeleteRepositoryInput{RepositoryName: new("renamed")}); err != nil {
		t.Fatal(err)
	}

	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("deleting left the content: %v", statErr)
	}

	// Stop leaves a data directory alone but removes a temporary one.
	svc.Stop()
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("Stop removed the data directory: %v", statErr)
	}

	temp := New(memory.New(nil, nil))
	tempRoot, err := temp.contentRoot()
	if err != nil {
		t.Fatal(err)
	}

	if _, statErr := os.Stat(tempRoot); statErr != nil {
		t.Fatal(statErr)
	}

	temp.Stop()
	if _, statErr := os.Stat(tempRoot); !os.IsNotExist(statErr) {
		t.Fatalf("Stop left the temporary directory: %v", statErr)
	}
}

func TestServeGitUnknownRepository(t *testing.T) {
	svc := New(memory.New(nil, nil))
	t.Cleanup(svc.Stop)
	scope := request.Scope{AccountID: "123456789012", Region: "us-east-1"}
	for _, name := range []string{"missing", "bad name", ""} {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(
			context.Background(), http.MethodGet, "/v1/repos/x/info/refs?service=git-upload-pack", http.NoBody)
		svc.ServeGit(w, req, scope, name, "info/refs")
		if w.Code != http.StatusNotFound {
			t.Errorf("%q: %d", name, w.Code)
		}
	}
}
