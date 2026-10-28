package codecommit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// errNoChange is a commit that would leave the tree as it is.
var errNoChange = errors.New("the commit changes nothing")

// change is one file of a commit: a file written, a file deleted or a file's mode changed.
type change struct {
	path    string
	mode    string
	content []byte
	// put is a file written with content; a change without one and without delete sets the mode.
	put    bool
	delete bool
}

// commitRequest is a commit on a branch: the branch's current tip as parent, changes to its tree, and a message.
type commitRequest struct {
	parent  string
	message *string
	name    *string
	email   *string
	branch  string
	changes []change
}

// commitResult is a commit CommitFiles made.
type commitResult struct {
	commit, tree            string
	added, updated, deleted []api.FileMetadata
}

// tipFor checks the parent commit against the branch. A branch that does not exist can only be created by the
// first commit of an empty repository, which has no parent.
func (r *repository) tipFor(ctx context.Context, req *commitRequest) (string, error) {
	list, err := r.branches(ctx)
	if err != nil {
		return "", err
	}

	i := slices.IndexFunc(list, func(b branch) bool { return b.name == req.branch })
	if i < 0 {
		if len(list) > 0 {
			return "", awserror.New("BranchDoesNotExistException",
				fmt.Sprintf("The branch %s does not exist.", req.branch))
		}

		if req.parent != "" {
			return "", awserror.New("ParentCommitDoesNotExistException",
				fmt.Sprintf("The parent commit %s does not exist.", req.parent))
		}

		return "", nil
	}

	tip := list[i].commit
	if req.parent == "" {
		return "", awserror.New("ParentCommitIdRequiredException",
			"A parent commit ID is required. Use GetBranch to find the tip of the branch.")
	}

	if _, ok, commitErr := r.commitOf(ctx, req.parent); commitErr != nil || !ok {
		if commitErr != nil {
			return "", commitErr
		}

		return "", awserror.New("ParentCommitDoesNotExistException",
			fmt.Sprintf("The parent commit %s does not exist.", req.parent))
	}

	if req.parent != tip {
		return "", awserror.New("ParentCommitIdOutdatedException",
			"The file could not be added because the provided parent commit ID is not the current tip of "+
				req.branch+".")
	}

	return tip, nil
}

// conflicts reports a change whose path clashes with a file or folder of the resulting tree.
func conflicts(existing map[string]entry, changes []change) error {
	files := map[string]string{}
	for path, e := range existing {
		files[path] = e.mode
	}

	for _, c := range changes {
		if c.delete {
			delete(files, c.path)
		}
	}

	puts := map[string]bool{}
	for _, c := range changes {
		if c.put {
			files[c.path] = c.mode
			puts[c.path] = true
		}
	}

	for _, c := range changes {
		if !c.put {
			continue
		}

		parts := strings.Split(c.path, "/")
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], "/")
			mode, isFile := files[parent]
			switch {
			case !isFile:
			case mode == gitSubmodule:
				return awserror.New("FilePathConflictsWithSubmodulePathException",
					fmt.Sprintf("The path %s conflicts with the submodule %s.", c.path, parent))
			case puts[parent]:
				return awserror.New("PutFileEntryConflictException",
					"One or more files specified in the commit reference both a file and a folder.")
			default:
				return awserror.New("DirectoryNameConflictsWithFileNameException",
					fmt.Sprintf("The path %s conflicts with the file %s.", c.path, parent))
			}
		}

		if mode, isFile := files[c.path]; isFile && mode == gitSubmodule {
			return awserror.New("FilePathConflictsWithSubmodulePathException",
				fmt.Sprintf("The path %s is a submodule.", c.path))
		}

		for path := range files {
			if !strings.HasPrefix(path, c.path+"/") {
				continue
			}

			if puts[path] {
				return awserror.New("PutFileEntryConflictException",
					"One or more files specified in the commit reference both a file and a folder.")
			}

			return awserror.New("FileNameConflictsWithDirectoryNameException",
				fmt.Sprintf("The path %s is a folder with files in it.", c.path))
		}
	}

	return nil
}

// plan checks the changes against the files of the parent commit and returns the index lines that apply them,
// along with the files added, updated and deleted. Writing a file as it is already counts as no change.
func (r *repository) plan(
	ctx context.Context, existing map[string]entry, changes []change,
) (lines []byte, res *commitResult, err error) {
	seen := map[string]bool{}
	for _, c := range changes {
		if seen[c.path] {
			return nil, nil, awserror.New("SamePathRequestException",
				"One or more changes in this commit duplicate actions in the same file path.")
		}

		seen[c.path] = true
	}

	if err := conflicts(existing, changes); err != nil {
		return nil, nil, err
	}

	res = &commitResult{}
	var deletes, writes bytes.Buffer
	for _, c := range changes {
		current, found := existing[c.path]
		if !found || current.kind != "blob" {
			found = false
		}

		switch {
		case c.delete:
			if !found {
				return nil, nil, awserror.New(
					"FileDoesNotExistException",
					fmt.Sprintf("The file %s does not exist.", c.path),
				)
			}

			fmt.Fprintf(&deletes, "0 %s\t%s\x00", zeroID, c.path)
			res.deleted = append(res.deleted, api.FileMetadata{
				AbsolutePath: &c.path, BlobId: &current.id, FileMode: new(fileMode(current.mode)),
			})
		case c.put:
			mode := c.mode
			if mode == "" && found {
				mode = current.mode
			} else if mode == "" {
				mode = gitNormal
			}

			out, hashErr := git(ctx, r.dir, c.content, nil, "hash-object", "-w", "--stdin")
			if hashErr != nil {
				return nil, nil, hashErr
			}

			id := strings.TrimSpace(string(out))
			if found && current.id == id && current.mode == mode {
				continue
			}

			fmt.Fprintf(&writes, "%s %s\t%s\x00", mode, id, c.path)
			meta := api.FileMetadata{AbsolutePath: &c.path, BlobId: &id, FileMode: new(fileMode(mode))}
			if found {
				res.updated = append(res.updated, meta)
			} else {
				res.added = append(res.added, meta)
			}
		default:
			if !found {
				return nil, nil, awserror.New(
					"FileDoesNotExistException",
					fmt.Sprintf("The file %s does not exist.", c.path),
				)
			}

			if current.mode == c.mode {
				continue
			}

			fmt.Fprintf(&writes, "%s %s\t%s\x00", c.mode, current.id, c.path)
			res.updated = append(res.updated, api.FileMetadata{
				AbsolutePath: &c.path, BlobId: &current.id, FileMode: new(fileMode(c.mode)),
			})
		}
	}

	return append(deletes.Bytes(), writes.Bytes()...), res, nil
}

// author returns the name and email a commit is attributed to: those given, else the caller's name.
func author(ctx context.Context, name, email *string) (authorName, authorEmail string) {
	if name != nil && *name != "" {
		return *name, common.Value(email)
	}

	m, _ := request.FromContext(ctx)
	if m.Principal != nil && m.Principal.Name != "" {
		return m.Principal.Name, common.Value(email)
	}

	return m.AccountID, common.Value(email)
}

// commit writes the commit on the branch and moves the branch to it. The branch moves only if it is still at the
// parent the commit was built on, so concurrent commits cannot lose each other.
func (s *Service) commit(ctx context.Context, r *repository, req *commitRequest) (*commitResult, error) {
	if exists := r.exists(); exists {
		if _, ok, err := r.commitOf(ctx, tagsPrefix+req.branch); err != nil {
			return nil, err
		} else if ok {
			return nil, awserror.New("BranchNameIsTagNameException",
				fmt.Sprintf("The name %s is a tag, not a branch.", req.branch))
		}
	}

	tip, err := r.tipFor(ctx, req)
	if err != nil {
		return nil, err
	}

	if ensureErr := r.ensure(); ensureErr != nil {
		return nil, ensureErr
	}

	existing := map[string]entry{}
	var oldTree string
	if tip != "" {
		entries, treeErr := r.tree(ctx, tip, true, "")
		if treeErr != nil {
			return nil, treeErr
		}

		for _, e := range entries {
			existing[e.path] = e
		}

		out, revErr := git(ctx, r.dir, nil, nil, "rev-parse", "--verify", tip+"^{tree}")
		if revErr != nil {
			return nil, revErr
		}

		oldTree = strings.TrimSpace(string(out))
	}

	lines, res, err := r.plan(ctx, existing, req.changes)
	if err != nil {
		return nil, err
	}

	index := filepath.Join(r.dir, "tinystack-index-"+common.RandomID(12, "0123456789abcdef"))
	defer func() { _ = os.Remove(index) }()
	env := []string{"GIT_INDEX_FILE=" + index}
	if tip != "" {
		if _, err = git(ctx, r.dir, nil, env, "read-tree", tip); err != nil {
			return nil, err
		}
	}

	if len(lines) > 0 {
		if _, err = git(ctx, r.dir, lines, env, "update-index", "-z", "--index-info"); err != nil {
			return nil, err
		}
	}

	out, err := git(ctx, r.dir, nil, env, "write-tree")
	if err != nil {
		return nil, err
	}

	res.tree = strings.TrimSpace(string(out))
	if res.tree == oldTree {
		return nil, errNoChange
	}

	name, email := author(ctx, req.name, req.email)
	ident := []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
	}

	args := []string{"commit-tree", res.tree}
	if tip != "" {
		args = append(args, "-p", tip)
	}

	args = append(args, "-F", "-")
	out, err = git(ctx, r.dir, []byte(common.Value(req.message)), ident, args...)
	if err != nil {
		return nil, err
	}

	res.commit = strings.TrimSpace(string(out))
	old := tip
	if old == "" {
		old = zeroID
	}

	if _, err = git(ctx, r.dir, nil, nil, "update-ref", headsPrefix+req.branch, res.commit, old); err != nil {
		if isGit(err) {
			return nil, awserror.New("ParentCommitIdOutdatedException",
				"The branch was updated by another commit while this one was being made.")
		}

		return nil, err
	}

	if tip == "" {
		s.emit(ctx, r, refCreated, req.branch, res.commit, "")
	} else {
		s.emit(ctx, r, refUpdated, req.branch, res.commit, tip)
	}

	return res, nil
}
