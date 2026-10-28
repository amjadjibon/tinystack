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
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	headsPrefix = "refs/heads/"
	tagsPrefix  = "refs/tags/"
	// branchPageSize is TinyStack's own bound: the model documents no page size for ListBranches.
	branchPageSize = 1000
)

// branch is a branch and the commit at its tip.
type branch struct{ name, commit string }

// branches returns the repository's branches in name order.
func (r *repository) branches(ctx context.Context) ([]branch, error) {
	if !r.exists() {
		return nil, nil
	}

	out, err := git(ctx, r.dir, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)", headsPrefix)
	if err != nil {
		return nil, err
	}

	var list []branch
	for line := range strings.Lines(string(out)) {
		name, commit, _ := strings.Cut(strings.TrimSpace(line), " ")
		list = append(list, branch{name: strings.TrimPrefix(name, headsPrefix), commit: commit})
	}

	return list, nil
}

// defaultBranch returns the branch HEAD names, along with every branch. A repository's first pushed branch
// becomes its default, so a HEAD that names no branch is pointed at one when there is any: main or master
// first, otherwise the first by name. The default is empty while the repository has no branch.
func (r *repository) defaultBranch(ctx context.Context) (string, []branch, error) {
	list, err := r.branches(ctx)
	if err != nil || len(list) == 0 {
		return "", list, err
	}

	head, err := os.ReadFile(filepath.Join(r.dir, "HEAD"))
	if err != nil {
		return "", nil, err
	}

	target := strings.TrimPrefix(strings.TrimSpace(string(head)), "ref: "+headsPrefix)
	if slices.ContainsFunc(list, func(b branch) bool { return b.name == target }) {
		return target, list, nil
	}

	chosen := list[0].name
	for _, preferred := range []string{"main", "master"} {
		if slices.ContainsFunc(list, func(b branch) bool { return b.name == preferred }) {
			chosen = preferred
			break
		}
	}

	if _, err = git(ctx, r.dir, nil, nil, "symbolic-ref", "HEAD", headsPrefix+chosen); err != nil {
		return "", nil, err
	}

	return chosen, list, nil
}

// isGit reports whether err is a failed git command, as opposed to git being unavailable or the request ending.
func isGit(err error) bool {
	_, ok := errors.AsType[*gitError](err)
	return ok
}

// commitOf resolves a revision to the commit it names, and whether there is one.
func (r *repository) commitOf(ctx context.Context, revision string) (commit string, ok bool, err error) {
	if !r.exists() {
		return "", false, nil
	}

	out, err := git(ctx, r.dir, nil, nil, "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if isGit(err) {
		return "", false, nil
	}

	return strings.TrimSpace(string(out)), err == nil, err
}

// resolve returns the commit a commit specifier names: a commit ID, a branch, a tag, HEAD or a fully qualified
// reference. Without a specifier it is the tip of the default branch.
func (r *repository) resolve(ctx context.Context, specifier *string) (string, error) {
	if specifier == nil || *specifier == "" {
		name, list, err := r.defaultBranch(ctx)
		if err != nil {
			return "", err
		}

		for _, b := range list {
			if b.name == name {
				return b.commit, nil
			}
		}

		return "", awserror.New("CommitDoesNotExistException", "The repository has no commits.")
	}

	spec := *specifier
	if err := validateRevision(spec); err != nil {
		return "", err
	}

	candidates := []string{headsPrefix + spec, tagsPrefix + spec}
	switch {
	case commitIDPattern.MatchString(spec):
		candidates = []string{strings.ToLower(spec), headsPrefix + spec, tagsPrefix + spec}
	case spec == "HEAD", strings.HasPrefix(spec, "refs/"):
		candidates = []string{spec}
	}

	for _, candidate := range candidates {
		commit, ok, err := r.commitOf(ctx, candidate)
		if err != nil {
			return "", err
		}

		if ok {
			return commit, nil
		}
	}

	return "", awserror.New("CommitDoesNotExistException", fmt.Sprintf("The commit %s does not exist.", spec))
}

// repositoryNamed validates a repository name and returns the repository.
func (s *Service) repositoryNamed(ctx context.Context, scope request.Scope, name *string) (*repository, error) {
	valid, err := validateRepoName(name)
	if err != nil {
		return nil, err
	}

	return s.lookup(ctx, scope, valid)
}

// Branch returns the commit at the tip of a branch.
func (s *Service) Branch(ctx context.Context, scope request.Scope, repo, name string) (string, error) {
	r, err := s.lookup(ctx, scope, repo)
	if err != nil {
		return "", err
	}

	return r.tip(ctx, name)
}

// tip returns the commit at the tip of a branch.
func (r *repository) tip(ctx context.Context, name string) (string, error) {
	_, list, err := r.defaultBranch(ctx)
	if err != nil {
		return "", err
	}

	for _, b := range list {
		if b.name == name {
			return b.commit, nil
		}
	}

	return "", awserror.New("BranchDoesNotExistException", fmt.Sprintf("The branch %s does not exist.", name))
}

func (s *Service) GetBranch(ctx context.Context, in *api.GetBranchInput) (*api.GetBranchOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	name, err := validateBranchName(in.BranchName)
	if err != nil {
		return nil, err
	}

	commit, err := s.Branch(ctx, scope, repoName, name)
	if err != nil {
		return nil, err
	}

	return &api.GetBranchOutput{Branch: &api.BranchInfo{BranchName: &name, CommitId: &commit}}, nil
}

func (s *Service) ListBranches(ctx context.Context, in *api.ListBranchesInput) (*api.ListBranchesOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.repositoryNamed(ctx, scope, in.RepositoryName)
	if err != nil {
		return nil, err
	}

	_, list, err := r.defaultBranch(ctx)
	if err != nil {
		return nil, err
	}

	invalid := awserror.New("InvalidContinuationTokenException", "The specified continuation token is not valid.")
	page, next, err := common.Page(list, func(b branch) string { return b.name }, in.NextToken, branchPageSize, invalid)
	if err != nil {
		return nil, err
	}

	out := &api.ListBranchesOutput{Branches: make([]string, 0, len(page)), NextToken: next}
	for _, b := range page {
		out.Branches = append(out.Branches, b.name)
	}

	return out, nil
}

func (s *Service) CreateBranch(ctx context.Context, in *api.CreateBranchInput) (*api.CreateBranchOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	name, err := validateBranchName(in.BranchName)
	if err != nil {
		return nil, err
	}

	id, err := validateCommitID(in.CommitId, "CommitIdRequiredException", "InvalidCommitIdException")
	if err != nil {
		return nil, err
	}

	r, err := s.lookup(ctx, scope, repoName)
	if err != nil {
		return nil, err
	}

	commit, ok, err := r.commitOf(ctx, id)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, awserror.New("CommitDoesNotExistException", fmt.Sprintf("The commit %s does not exist.", id))
	}

	if err := r.createBranch(ctx, name, commit); err != nil {
		return nil, err
	}

	s.emit(ctx, r, refCreated, name, commit, "")
	return &api.CreateBranchOutput{}, nil
}

// createBranch points a new branch at a commit.
func (r *repository) createBranch(ctx context.Context, name, commit string) error {
	exists := awserror.New("BranchNameExistsException", fmt.Sprintf("The branch %s already exists.", name))
	if r.exists() {
		if _, err := r.tip(ctx, name); err == nil {
			return exists
		}
	}

	if err := r.ensure(); err != nil {
		return err
	}

	// Creating a ref with an old value of zero fails if it exists, or collides with a branch named like a folder.
	_, err := git(ctx, r.dir, nil, nil, "update-ref", headsPrefix+name, commit, zeroID)
	if isGit(err) {
		return exists
	}

	return err
}

func (s *Service) DeleteBranch(ctx context.Context, in *api.DeleteBranchInput) (*api.DeleteBranchOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	name, err := validateBranchName(in.BranchName)
	if err != nil {
		return nil, err
	}

	r, err := s.lookup(ctx, scope, repoName)
	if err != nil {
		return nil, err
	}

	current, list, err := r.defaultBranch(ctx)
	if err != nil {
		return nil, err
	}

	if name == current {
		return nil, awserror.New("DefaultBranchCannotBeDeletedException",
			fmt.Sprintf("The branch %s is the default branch of the repository and cannot be deleted.", name))
	}

	i := slices.IndexFunc(list, func(b branch) bool { return b.name == name })
	if i < 0 {
		// Deleting a branch that does not exist succeeds, returning no branch.
		return &api.DeleteBranchOutput{}, nil
	}

	tip := list[i].commit
	if _, err = git(ctx, r.dir, nil, nil, "update-ref", "-d", headsPrefix+name, tip); err != nil {
		return nil, err
	}

	s.emit(ctx, r, refDeleted, name, "", tip)
	return &api.DeleteBranchOutput{DeletedBranch: &api.BranchInfo{BranchName: &name, CommitId: &tip}}, nil
}

func (s *Service) UpdateDefaultBranch(
	ctx context.Context,
	in *api.UpdateDefaultBranchInput,
) (*api.UpdateDefaultBranchOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	name, err := validateBranchName(in.DefaultBranchName)
	if err != nil {
		return nil, err
	}

	r, err := s.lookup(ctx, scope, repoName)
	if err != nil {
		return nil, err
	}

	current, _, err := r.defaultBranch(ctx)
	if err != nil {
		return nil, err
	}

	if _, err = r.tip(ctx, name); err != nil {
		return nil, err
	}

	if name == current {
		return &api.UpdateDefaultBranchOutput{}, nil
	}

	if _, err = git(ctx, r.dir, nil, nil, "symbolic-ref", "HEAD", headsPrefix+name); err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, repositoryKind, repoName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[repositoryRecord](tx, key)
		if readErr != nil || !found {
			return readErr
		}

		rec.Metadata.LastModifiedDate = new(common.Unix(time.Now()))
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDefaultBranchOutput{}, nil
}

// refs returns every reference with its commit, for comparing the state before and after a push.
func (r *repository) refs(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if !r.exists() {
		return out, nil
	}

	raw, err := git(ctx, r.dir, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}

	for line := range bytes.Lines(raw) {
		name, commit, _ := strings.Cut(strings.TrimSpace(string(line)), " ")
		out[name] = commit
	}

	return out, nil
}
