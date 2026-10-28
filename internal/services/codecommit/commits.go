package codecommit

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

func (s *Service) GetCommit(ctx context.Context, in *api.GetCommitInput) (*api.GetCommitOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	repoName, err := validateRepoName(in.RepositoryName)
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

	missing := awserror.New("CommitIdDoesNotExistException", fmt.Sprintf("The commit %s does not exist.", id))
	if !r.exists() {
		return nil, missing
	}

	raw, err := git(ctx, r.dir, nil, nil, "cat-file", "commit", id)
	if isGit(err) {
		return nil, missing
	}

	if err != nil {
		return nil, err
	}

	return &api.GetCommitOutput{Commit: parseCommit(id, string(raw))}, nil
}

// parseCommit reads a raw commit object: header lines, a blank line, then the message.
func parseCommit(id, raw string) *api.Commit {
	header, message, _ := strings.Cut(raw, "\n\n")
	commit := &api.Commit{CommitId: &id, Message: &message, Parents: []string{}}
	for line := range strings.SplitSeq(header, "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			commit.TreeId = &value
		case "parent":
			commit.Parents = append(commit.Parents, value)
		case "author":
			commit.Author = parseUser(value)
		case "committer":
			commit.Committer = parseUser(value)
		}
	}

	return commit
}

// parseUser reads "Name <email> 1700000000 +0000". The date keeps git's form, a timestamp with its GMT offset,
// which is the form CodeCommit documents.
func parseUser(ident string) *api.UserInfo {
	open, closing := strings.LastIndex(ident, "<"), strings.LastIndex(ident, ">")
	if open < 0 || closing < open {
		return &api.UserInfo{Name: &ident}
	}

	name := strings.TrimSpace(ident[:open])
	email := ident[open+1 : closing]
	date := strings.TrimSpace(ident[closing+1:])
	return &api.UserInfo{Name: &name, Email: &email, Date: &date}
}
