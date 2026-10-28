package codecommit

import (
	"context"
	"net/http"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	// emptyTree is the ID of the empty tree, which git knows without storing it.
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	// differencesPageSize is the default number of differences in a response.
	differencesPageSize = 100
)

func (s *Service) GetDifferences(ctx context.Context, in *api.GetDifferencesInput) (*api.GetDifferencesOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	r, err := s.repositoryNamed(ctx, scope, in.RepositoryName)
	if err != nil {
		return nil, err
	}

	if in.AfterCommitSpecifier == nil || *in.AfterCommitSpecifier == "" {
		return nil, awserror.New("CommitRequiredException", "A commit was not specified.")
	}

	limit := differencesPageSize
	if in.MaxResults != nil {
		if *in.MaxResults < 1 {
			return nil, awserror.New("InvalidMaxResultsException",
				"The specified number of maximum results is not valid.")
		}

		limit = int(min(*in.MaxResults, int64(1<<30)))
	}

	path, err := differencePath(in)
	if err != nil {
		return nil, err
	}

	after, err := r.resolve(ctx, in.AfterCommitSpecifier)
	if err != nil {
		return nil, err
	}

	before := emptyTree
	if in.BeforeCommitSpecifier != nil && *in.BeforeCommitSpecifier != "" {
		if before, err = r.resolve(ctx, in.BeforeCommitSpecifier); err != nil {
			return nil, err
		}
	}

	args := []string{"diff-tree", "-r", "-z", "--raw", "--no-renames", "--no-abbrev", before, after}
	if path != "" {
		args = append(args, "--", path)
	}

	out, err := git(ctx, r.dir, nil, nil, args...)
	if err != nil {
		return nil, err
	}

	if path != "" && len(out) == 0 {
		if pathErr := r.requirePath(ctx, before, after, path); pathErr != nil {
			return nil, pathErr
		}
	}

	diffs := parseDifferences(string(out))
	invalid := awserror.New("InvalidContinuationTokenException", "The specified continuation token is not valid.")
	page, next, err := common.Page(diffs, diffKey, in.NextToken, limit, invalid)
	if err != nil {
		return nil, err
	}

	return &api.GetDifferencesOutput{Differences: page, NextToken: next}, nil
}

// differencePath returns the path that limits the differences. A different path for each side, which compares a
// moved folder with its old place, is not supported.
func differencePath(in *api.GetDifferencesInput) (string, error) {
	before, after := common.Value(in.BeforePath), common.Value(in.AfterPath)
	if before != "" && after != "" && before != after {
		return "", &awserror.Error{
			Code:       "NotImplemented",
			Message:    "GetDifferences with different beforePath and afterPath is not supported.",
			StatusCode: http.StatusNotImplemented,
		}
	}

	chosen := after
	if chosen == "" {
		chosen = before
	}

	if chosen == "" {
		return "", nil
	}

	return validatePath(&chosen)
}

// requirePath fails with PathDoesNotExistException if a path is in neither commit.
func (r *repository) requirePath(ctx context.Context, before, after, path string) error {
	for _, commit := range []string{before, after} {
		if commit == emptyTree {
			continue
		}

		if _, found, err := r.at(ctx, commit, path); err != nil || found {
			return err
		}
	}

	return awserror.New("PathDoesNotExistException", "The path "+path+" does not exist.")
}

// diffKey is the key differences are ordered and paged by: git lists them in path order.
func diffKey(d api.Difference) string {
	if d.AfterBlob != nil {
		return common.Value(d.AfterBlob.Path)
	}

	return common.Value(d.BeforeBlob.Path)
}

// parseDifferences reads git diff-tree --raw -z output: ":mode mode id id status", then the path.
func parseDifferences(raw string) []api.Difference {
	var diffs []api.Difference
	fields := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			continue
		}

		path := fields[i+1]
		diff := api.Difference{ChangeType: new(meta[4])}
		if meta[4] != "A" {
			diff.BeforeBlob = &api.BlobMetadata{BlobId: new(meta[2]), Mode: new(meta[0]), Path: new(path)}
		}

		if meta[4] != "D" {
			diff.AfterBlob = &api.BlobMetadata{BlobId: new(meta[3]), Mode: new(meta[1]), Path: new(path)}
		}

		diffs = append(diffs, diff)
	}

	return diffs
}
