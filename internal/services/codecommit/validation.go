package codecommit

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
)

const (
	sortByRepositoryName   = "repositoryName"
	sortByLastModifiedDate = "lastModifiedDate"
	orderAscending         = "ascending"
	orderDescending        = "descending"
)

var (
	namePattern   = regexp.MustCompile(`^[\w.-]+$`)
	kmsKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9:/_-]+$`)
)

func validateRepoName(name *string) (string, error) {
	if name == nil || *name == "" {
		return "", awserror.New(
			"RepositoryNameRequiredException",
			"A repository name is required, but was not specified.",
		)
	}

	if len(*name) > 100 || !namePattern.MatchString(*name) {
		return "", awserror.New(
			"InvalidRepositoryNameException",
			"The specified repository name is not valid. "+
				"Repository names can be 1 to 100 characters long and can "+
				"contain letters, numbers, periods, underscores, and hyphens.",
		)
	}

	return *name, nil
}

func validateRepoDescription(desc *string) error {
	if desc != nil && utf8.RuneCountInString(*desc) > 1000 {
		return awserror.New(
			"InvalidRepositoryDescriptionException",
			"The specified repository description is not valid. Repository descriptions cannot exceed 1,000 characters.",
		)
	}

	return nil
}

func validateSortBy(s *string) (string, error) {
	if s == nil || *s == "" {
		return sortByRepositoryName, nil
	}

	if *s != sortByRepositoryName && *s != sortByLastModifiedDate {
		return "", awserror.New(
			"InvalidSortByException",
			"The specified sort by value is not valid. You must use a valid sort by value: repositoryName or lastModifiedDate.",
		)
	}

	return *s, nil
}

func validateOrder(o *string) (string, error) {
	if o == nil || *o == "" {
		return orderAscending, nil
	}

	if *o != orderAscending && *o != orderDescending {
		return "", awserror.New(
			"InvalidOrderException",
			"The specified sort order is not valid. You must use a valid sort order: ascending or descending.",
		)
	}

	return *o, nil
}

func parseRepositoryArn(scope request.Scope, rawArn *string) (string, error) {
	if rawArn == nil || *rawArn == "" {
		return "", awserror.New("ResourceArnRequiredException", "A resource ARN is required, but was not specified.")
	}

	parts := strings.Split(*rawArn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != serviceName ||
		parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", awserror.New("InvalidResourceArnException", "The specified Amazon Resource Name (ARN) is not valid.")
	}

	name := parts[5]
	if !namePattern.MatchString(name) || len(name) > 100 {
		return "", awserror.New("InvalidResourceArnException", "The specified Amazon Resource Name (ARN) is not valid.")
	}

	return name, nil
}

// tagError maps a shared tag-limit error to the CodeCommit error shape.
func tagError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tagging.ErrLimit):
		return awserror.New("TooManyTagsException", err.Error())
	case errors.Is(err, tagging.ErrReserved):
		return awserror.New("InvalidSystemTagUsageException", err.Error())
	default:
		return awserror.New("InvalidTagsMapException", err.Error())
	}
}

func validateKmsKeyID(id *string) error {
	if id != nil && !kmsKeyPattern.MatchString(*id) {
		return awserror.New("EncryptionKeyInvalidIdException", "The KMS key ID is not valid.")
	}

	return nil
}

func validateTagKeys(keys []string) error {
	for _, k := range keys {
		n := utf8.RuneCountInString(k)
		if n < 1 || n > 128 {
			return awserror.New("InvalidTagKeysListException", "Tag keys must be 1 to 128 characters long.")
		}
	}

	return nil
}
