package codecommit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// initialBranch is the branch a new repository's HEAD names until a branch is pushed.
const initialBranch = "main"

// bareConfig is the configuration of every repository: bare, with the files ref format.
const bareConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"

// repository is a repository record and where its git objects live.
type repository struct {
	scope request.Scope
	meta  *api.RepositoryMetadata
	name  string
	id    string
	dir   string
}

// UseDataDir keeps repository content in dir, which survives a restart. Without it, content lives in a
// temporary directory that Stop removes.
func (s *Service) UseDataDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.root, s.temp = dir, false
}

// contentRoot returns the directory holding every repository, creating a temporary one on first use.
func (s *Service) contentRoot() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != "" {
		return s.root, nil
	}

	root, err := os.MkdirTemp("", "tinystack-codecommit-")
	if err != nil {
		return "", err
	}

	s.root, s.temp = root, true
	return root, nil
}

// Stop removes the repository content of an in-memory run.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.temp {
		_ = os.RemoveAll(s.root)
		s.root, s.temp = "", false
	}
}

// Reset removes every repository's content. Records are cleared separately.
func (s *Service) Reset(context.Context) {
	s.mu.Lock()
	root := s.root
	s.mu.Unlock()
	if root != "" {
		_ = os.RemoveAll(root)
	}
}

// lookup returns the repository named name in scope. It reads the record only: no repository directory
// exists until something is written to it.
func (s *Service) lookup(ctx context.Context, scope request.Scope, name string) (*repository, error) {
	var record *repositoryRecord
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		var found bool
		record, found, readErr = storage.Read[repositoryRecord](tx, scope.Key(serviceName, repositoryKind, name))
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New(
				"RepositoryDoesNotExistException",
				fmt.Sprintf("The repository %s does not exist.", name),
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := *record.Metadata.RepositoryId
	dir, err := s.repositoryDir(scope, id)
	if err != nil {
		return nil, err
	}

	return &repository{scope: scope, meta: record.Metadata, name: name, id: id, dir: dir}, nil
}

// repositoryDir is where the repository with the ID keeps its git objects. Renames keep the ID, so they move no files.
func (s *Service) repositoryDir(scope request.Scope, id string) (string, error) {
	root, err := s.contentRoot()
	if err != nil {
		return "", err
	}

	return filepath.Join(root, scope.AccountID, scope.Region, id+".git"), nil
}

// exists reports whether anything has been written to the repository.
func (r *repository) exists() bool {
	_, err := os.Stat(filepath.Join(r.dir, "HEAD"))
	return err == nil
}

// ensure creates the empty bare repository if it does not exist yet. HEAD is written last, so a
// repository that has one is complete.
func (r *repository) ensure() error {
	if r.exists() {
		return nil
	}

	for _, dir := range []string{"objects/info", "objects/pack", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(r.dir, dir), 0o750); err != nil {
			return err
		}
	}

	if err := os.WriteFile(filepath.Join(r.dir, "config"), []byte(bareConfig), 0o600); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(r.dir, "HEAD"), []byte("ref: refs/heads/"+initialBranch+"\n"), 0o600)
}
