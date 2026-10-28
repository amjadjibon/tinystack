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

const (
	modeNormal     = "NORMAL"
	modeExecutable = "EXECUTABLE"
	modeSymlink    = "SYMLINK"

	// maxBranchName is the model's length limit for branch names.
	maxBranchName = 256
	// maxFileSize is the model's limit for the content of one file (6 MB).
	maxFileSize = 6 << 20
	// maxCommitMessage is CodeCommit's documented message limit (256 KB).
	maxCommitMessage = 256 << 10
	// maxFileEntries is CodeCommit's documented limit of changed files in one commit.
	maxFileEntries = 100
)

var (
	commitIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	// revisionPattern admits the commit specifiers CodeCommit documents: a branch, a tag, HEAD, a fully
	// qualified reference or a commit ID.
	revisionPattern = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)
	emailPattern    = regexp.MustCompile(`^[^\s<>]+$`)
)

// validRefName applies git's reference name rules (git check-ref-format) to a branch name.
func validRefName(name string) bool {
	if name == "" || len(name) > maxBranchName || name == "@" || strings.HasPrefix(name, "/") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") ||
		strings.Contains(name, "//") || strings.Contains(name, "@{") || strings.HasPrefix(name, "-") {
		return false
	}

	for _, r := range name {
		if r < ' ' || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}

	for part := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}

	return true
}

func validateBranchName(name *string) (string, error) {
	if name == nil || *name == "" {
		return "", awserror.New("BranchNameRequiredException", "A branch name is required, but was not specified.")
	}

	if !validRefName(*name) {
		return "", awserror.New("InvalidBranchNameException",
			"The specified reference name is not valid. Reference names can be 1 to 256 characters long and "+
				"cannot contain spaces, control characters, or the characters ~ ^ : ? * [ \\.")
	}

	return *name, nil
}

// validateCommitID checks a commit ID given to CreateBranch, GetCommit or as a parent. The three operations
// name different exceptions for a missing and an invalid ID.
func validateCommitID(id *string, required, invalid string) (string, error) {
	if id == nil || *id == "" {
		return "", awserror.New(required, "A commit ID is required, but was not specified.")
	}

	if !commitIDPattern.MatchString(*id) {
		return "", awserror.New(
			invalid,
			"The specified commit ID is not valid. Specify the full 40-character commit ID.",
		)
	}

	return strings.ToLower(*id), nil
}

// validatePath checks a file or folder path inside a repository and returns it without a leading slash.
// "/" and "." name the root and are the caller's to accept.
func validatePath(path *string) (string, error) {
	if path == nil || *path == "" {
		return "", awserror.New("PathRequiredException", "The folderPath or filePath for a file or folder is required.")
	}

	clean := strings.TrimPrefix(*path, "/")
	if clean == "" || strings.HasSuffix(clean, "/") || !utf8.ValidString(clean) {
		return "", awserror.New("InvalidPathException", "The specified path is not valid.")
	}

	for part := range strings.SplitSeq(clean, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") ||
			strings.ContainsFunc(part, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			return "", awserror.New("InvalidPathException", "The specified path is not valid.")
		}
	}

	return clean, nil
}

func validateFileMode(mode *string) (string, error) {
	switch {
	case mode == nil || *mode == "":
		return "", nil
	case *mode == modeNormal, *mode == modeExecutable, *mode == modeSymlink:
		return *mode, nil
	default:
		return "", awserror.New("InvalidFileModeException",
			"The specified file mode permission is not valid. Valid file modes are NORMAL, EXECUTABLE and SYMLINK.")
	}
}

func validateCommitMessage(message *string) error {
	if message != nil && len(*message) > maxCommitMessage {
		return awserror.New("CommitMessageLengthExceededException",
			"The commit message is too long. Provide a shorter string.")
	}

	return nil
}

func validateEmail(email *string) error {
	if email != nil && *email != "" && !emailPattern.MatchString(*email) {
		return awserror.New("InvalidEmailException", "The specified email address is either invalid or unspecified.")
	}

	return nil
}

// validateRevision checks a commit specifier: a branch, tag, HEAD, fully qualified reference or commit ID.
func validateRevision(spec string) error {
	if !revisionPattern.MatchString(spec) || strings.HasPrefix(spec, "-") || strings.Contains(spec, "..") ||
		strings.Contains(spec, "@{") || strings.HasSuffix(spec, "/") || strings.HasSuffix(spec, ".lock") {
		return awserror.New("InvalidCommitException",
			"The specified commit is not valid. Specify a branch, tag, HEAD or full commit ID.")
	}

	return nil
}
