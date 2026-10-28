package codecommit

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// File modes git stores for the entries CodeCommit distinguishes.
const (
	gitTree       = "040000"
	gitNormal     = "100644"
	gitExecutable = "100755"
	gitSymlink    = "120000"
	gitSubmodule  = "160000"
)

// entry is one line of git ls-tree.
type entry struct {
	mode, kind, id, path string
}

// fileMode names the CodeCommit file mode of a git mode.
func fileMode(mode string) string {
	switch mode {
	case gitExecutable:
		return modeExecutable
	case gitSymlink:
		return modeSymlink
	default:
		return modeNormal
	}
}

// gitMode is the git mode of a CodeCommit file mode; no mode stays unset.
func gitMode(mode string) string {
	switch mode {
	case modeExecutable:
		return gitExecutable
	case modeSymlink:
		return gitSymlink
	case modeNormal:
		return gitNormal
	default:
		return ""
	}
}

// tree lists the entries of a tree-ish, or only the entry at path when there is one.
func (r *repository) tree(ctx context.Context, treeish string, recursive bool, path string) ([]entry, error) {
	args := []string{"ls-tree", "-z"}
	if recursive {
		args = append(args, "-r")
	}

	args = append(args, treeish)
	if path != "" {
		args = append(args, "--", path)
	}

	out, err := git(ctx, r.dir, nil, nil, args...)
	if err != nil {
		return nil, err
	}

	var entries []entry
	for record := range strings.SplitSeq(string(out), "\x00") {
		meta, name, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}

		fields := strings.Fields(meta)
		if len(fields) != 3 {
			continue
		}

		entries = append(entries, entry{mode: fields[0], kind: fields[1], id: fields[2], path: name})
	}

	return entries, nil
}

// at returns the entry at an exact path of a commit's tree.
func (r *repository) at(ctx context.Context, commit, path string) (entry, bool, error) {
	entries, err := r.tree(ctx, commit, false, path)
	if err != nil {
		return entry{}, false, err
	}

	for _, e := range entries {
		if e.path == path {
			return e, true, nil
		}
	}

	return entry{}, false, nil
}

// blob reads a blob's content, refusing one the model's file size limit rules out.
func (r *repository) blob(ctx context.Context, id string) ([]byte, error) {
	out, err := git(ctx, r.dir, nil, nil, "cat-file", "-s", id)
	if err != nil {
		return nil, err
	}

	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return nil, err
	}

	if size > maxFileSize {
		return nil, awserror.New("FileTooLargeException",
			"The specified file exceeds the file size limit for CodeCommit.")
	}

	return git(ctx, r.dir, nil, nil, "cat-file", "blob", id)
}

// File is a file at a commit.
type File struct {
	CommitID, BlobID, Path, Mode string
	Content                      []byte
}

// File reads a file at a commit specifier, which is a branch, tag, HEAD, fully qualified reference or commit ID.
// Without one it is read at the tip of the default branch.
func (s *Service) File(
	ctx context.Context,
	scope request.Scope,
	repo string,
	specifier *string,
	path string,
) (*File, error) {
	r, err := s.lookup(ctx, scope, repo)
	if err != nil {
		return nil, err
	}

	clean, err := validatePath(&path)
	if err != nil {
		return nil, err
	}

	commit, err := r.resolve(ctx, specifier)
	if err != nil {
		return nil, err
	}

	e, found, err := r.at(ctx, commit, clean)
	if err != nil {
		return nil, err
	}

	if !found || e.kind != "blob" {
		return nil, awserror.New("FileDoesNotExistException", fmt.Sprintf("The file %s does not exist.", path))
	}

	content, err := r.blob(ctx, e.id)
	if err != nil {
		return nil, err
	}

	return &File{CommitID: commit, BlobID: e.id, Path: path, Mode: fileMode(e.mode), Content: content}, nil
}

// Archive writes a zip of the tree at a commit.
func (s *Service) Archive(ctx context.Context, scope request.Scope, repo, commitID string, w io.Writer) error {
	r, err := s.lookup(ctx, scope, repo)
	if err != nil {
		return err
	}

	commit, err := r.resolve(ctx, &commitID)
	if err != nil {
		return err
	}

	return stream(ctx, r.dir, nil, w, nil, "archive", "--format=zip", commit)
}

func (s *Service) GetFile(ctx context.Context, in *api.GetFileInput) (*api.GetFileOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	if _, err = validatePath(in.FilePath); err != nil {
		return nil, err
	}

	file, err := s.File(ctx, scope, repoName, in.CommitSpecifier, *in.FilePath)
	if err != nil {
		return nil, err
	}

	return &api.GetFileOutput{
		CommitId:    &file.CommitID,
		BlobId:      &file.BlobID,
		FilePath:    &file.Path,
		FileMode:    &file.Mode,
		FileSize:    new(int64(len(file.Content))),
		FileContent: file.Content,
	}, nil
}

func (s *Service) GetFolder(ctx context.Context, in *api.GetFolderInput) (*api.GetFolderOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.repositoryNamed(ctx, scope, in.RepositoryName)
	if err != nil {
		return nil, err
	}

	// The root is "/" or ".".
	folder := ""
	if in.FolderPath != nil && *in.FolderPath != "/" && *in.FolderPath != "." {
		if folder, err = validatePath(in.FolderPath); err != nil {
			return nil, err
		}
	} else if in.FolderPath == nil || *in.FolderPath == "" {
		return nil, awserror.New(
			"PathRequiredException",
			"The folderPath or filePath for a file or folder is required.",
		)
	}

	commit, err := r.resolve(ctx, in.CommitSpecifier)
	if err != nil {
		return nil, err
	}

	treeID, err := r.treeID(ctx, commit, folder)
	if err != nil {
		return nil, err
	}

	if treeID == "" {
		return nil, awserror.New(
			"FolderDoesNotExistException",
			fmt.Sprintf("The folder %s does not exist.", *in.FolderPath),
		)
	}

	entries, err := r.tree(ctx, treeID, false, "")
	if err != nil {
		return nil, err
	}

	out := &api.GetFolderOutput{CommitId: &commit, FolderPath: in.FolderPath, TreeId: &treeID}
	for _, e := range entries {
		absolute := strings.TrimPrefix(folder+"/"+e.path, "/")
		relative := e.path
		switch e.mode {
		case gitTree:
			out.SubFolders = append(
				out.SubFolders,
				api.Folder{TreeId: &e.id, AbsolutePath: &absolute, RelativePath: &relative},
			)
		case gitSymlink:
			out.SymbolicLinks = append(out.SymbolicLinks, api.SymbolicLink{
				BlobId: &e.id, AbsolutePath: &absolute, RelativePath: &relative, FileMode: new(modeSymlink),
			})
		case gitSubmodule:
			out.SubModules = append(out.SubModules, api.SubModule{
				CommitId: &e.id, AbsolutePath: &absolute, RelativePath: &relative,
			})
		default:
			out.Files = append(out.Files, api.File{
				BlobId: &e.id, AbsolutePath: &absolute, RelativePath: &relative, FileMode: new(fileMode(e.mode)),
			})
		}
	}

	return out, nil
}

// treeID returns the ID of the tree at a folder path of a commit, or "" if there is none.
func (r *repository) treeID(ctx context.Context, commit, folder string) (string, error) {
	if folder == "" {
		out, err := git(ctx, r.dir, nil, nil, "rev-parse", "--verify", commit+"^{tree}")
		return strings.TrimSpace(string(out)), err
	}

	e, found, err := r.at(ctx, commit, folder)
	if err != nil || !found || e.kind != "tree" {
		return "", err
	}

	return e.id, nil
}
