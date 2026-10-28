package codecommit

import (
	"context"
	"errors"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// parentID validates the optional parent commit ID of a commit; none is the empty string.
func parentID(id *string) (string, error) {
	if id == nil || *id == "" {
		return "", nil
	}

	return validateCommitID(id, "", "InvalidParentCommitIdException")
}

func validateContent(content []byte) error {
	if len(content) > maxFileSize {
		return awserror.New("FileContentSizeLimitExceededException",
			"The file cannot be added because it is too large. The maximum file size is 6 MB.")
	}

	return nil
}

func (s *Service) PutFile(ctx context.Context, in *api.PutFileInput) (*api.PutFileOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	branchName, err := validateBranchName(in.BranchName)
	if err != nil {
		return nil, err
	}

	path, err := validatePath(in.FilePath)
	if err != nil {
		return nil, err
	}

	if in.FileContent == nil {
		return nil, awserror.New("FileContentRequiredException", "The file cannot be added because it is empty.")
	}

	if contentErr := validateContent(in.FileContent); contentErr != nil {
		return nil, contentErr
	}

	mode, err := validateFileMode(in.FileMode)
	if err != nil {
		return nil, err
	}

	parent, err := parentID(in.ParentCommitId)
	if err != nil {
		return nil, err
	}

	if messageErr := validateCommitMessage(in.CommitMessage); messageErr != nil {
		return nil, messageErr
	}

	if emailErr := validateEmail(in.Email); emailErr != nil {
		return nil, emailErr
	}

	r, err := s.lookup(ctx, scope, repoName)
	if err != nil {
		return nil, err
	}

	res, err := s.commit(ctx, r, &commitRequest{
		branch: branchName, parent: parent, message: in.CommitMessage, name: in.Name, email: in.Email,
		changes: []change{{path: path, put: true, mode: gitMode(mode), content: in.FileContent}},
	})
	if errors.Is(err, errNoChange) {
		return nil, awserror.New("SameFileContentException",
			"The file was not added or updated because the content of the file is exactly the same as the content "+
				"of that file in the repository and branch that you specified.")
	}

	if err != nil {
		return nil, err
	}

	written := slices.Concat(res.added, res.updated)
	return &api.PutFileOutput{CommitId: &res.commit, TreeId: &res.tree, BlobId: written[0].BlobId}, nil
}

func (s *Service) CreateCommit(ctx context.Context, in *api.CreateCommitInput) (*api.CreateCommitOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	branchName, err := validateBranchName(in.BranchName)
	if err != nil {
		return nil, err
	}

	parent, err := parentID(in.ParentCommitId)
	if err != nil {
		return nil, err
	}

	if messageErr := validateCommitMessage(in.CommitMessage); messageErr != nil {
		return nil, messageErr
	}

	if emailErr := validateEmail(in.Email); emailErr != nil {
		return nil, emailErr
	}

	changes, err := commitChanges(in)
	if err != nil {
		return nil, err
	}

	r, err := s.lookup(ctx, scope, repoName)
	if err != nil {
		return nil, err
	}

	res, err := s.commit(ctx, r, &commitRequest{
		branch: branchName, parent: parent, message: in.CommitMessage, name: in.AuthorName, email: in.Email,
		changes: changes,
	})
	if errors.Is(err, errNoChange) {
		return nil, awserror.New(
			"NoChangeException",
			"The commit cannot be created because no changes will be made to the repository as a result of this commit.",
		)
	}

	if err != nil {
		return nil, err
	}

	return &api.CreateCommitOutput{
		CommitId: &res.commit, TreeId: &res.tree,
		FilesAdded: res.added, FilesUpdated: res.updated, FilesDeleted: res.deleted,
	}, nil
}

// commitChanges validates the files of a CreateCommit request.
func commitChanges(in *api.CreateCommitInput) ([]change, error) {
	if len(in.PutFiles)+len(in.DeleteFiles)+len(in.SetFileModes) == 0 {
		return nil, awserror.New("FileEntryRequiredException",
			"The commit cannot be created because no files have been specified as added, updated, or changed.")
	}

	if len(in.PutFiles) > maxFileEntries || len(in.DeleteFiles) > maxFileEntries ||
		len(in.SetFileModes) > maxFileEntries {
		return nil, awserror.New("MaximumFileEntriesExceededException",
			"The number of specified files to change as part of this commit exceeds the maximum number of files that "+
				"can be changed in a single commit. Consider using a Git client for these changes.")
	}

	var changes []change
	for i := range in.PutFiles {
		entry := &in.PutFiles[i]
		path, err := validatePath(entry.FilePath)
		if err != nil {
			return nil, err
		}

		if entry.FileContent == nil {
			return nil, awserror.New(
				"SourceFileOrContentRequiredException",
				"The commit cannot be created because no source files or file content have been specified for the commit.",
			)
		}

		if contentErr := validateContent(entry.FileContent); contentErr != nil {
			return nil, contentErr
		}

		mode, err := validateFileMode(entry.FileMode)
		if err != nil {
			return nil, err
		}

		changes = append(changes, change{path: path, put: true, mode: gitMode(mode), content: entry.FileContent})
	}

	for _, entry := range in.DeleteFiles {
		path, err := validatePath(entry.FilePath)
		if err != nil {
			return nil, err
		}

		changes = append(changes, change{path: path, delete: true})
	}

	for _, entry := range in.SetFileModes {
		path, err := validatePath(entry.FilePath)
		if err != nil {
			return nil, err
		}

		if entry.FileMode == nil || *entry.FileMode == "" {
			return nil, awserror.New("FileModeRequiredException",
				"The commit cannot be created because no file mode has been specified. "+
					"A file mode is required to update mode permissions for a file.")
		}

		mode, err := validateFileMode(entry.FileMode)
		if err != nil {
			return nil, err
		}

		changes = append(changes, change{path: path, mode: gitMode(mode)})
	}

	return changes, nil
}
