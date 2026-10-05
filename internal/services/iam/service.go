// Package iam implements IAM roles, users, and groups with their managed and
// inline policies, and users' access keys, without HTTP dependencies. IAM is global: records live under the account,
// with no region, whatever region the request was signed for. Policies are
// stored and returned but never evaluated.
package iam

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindRole   = "role"
	kindPolicy = "policy"

	defaultPageSize    = 100
	maxPageSize        = 1000
	maxAttached        = 10
	maxPolicyVersions  = 5
	maxDocument        = 131072
	maxDescription     = 1000
	defaultSessionTime = 3600
	maxSessionTime     = 43200
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

type role struct {
	Tags             map[string]string `json:",omitempty"`
	Name             string
	ID               string
	Path             string
	AssumeRolePolicy string
	Description      string `json:",omitempty"`
	permissions
	MaxSessionDuration int64
	CreatedAt          int64
}

type policy struct {
	Tags           map[string]string `json:",omitempty"`
	Name           string
	ID             string
	Path           string
	Description    string `json:",omitempty"`
	DefaultVersion string
	Versions       []policyVersion
	NextVersion    int64
	Attachments    int64
	CreatedAt      int64
	UpdatedAt      int64
}

type policyVersion struct {
	ID        string
	Document  string
	CreatedAt int64
}

var (
	roleName   = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)
	policyName = regexp.MustCompile(`^[\w+=,.@-]{1,128}$`)
	rolePath   = regexp.MustCompile(`^(/|/[\x21-\x7E]+/)$`)
	policyPath = regexp.MustCompile(`^((/[A-Za-z0-9.,+@=_-]+)*)/$`)
	pathPrefix = regexp.MustCompile(`^/[\x21-\x7F]*$`)
	accountID  = regexp.MustCompile(`^\d{12}$`)
)

func validation(format string, args ...any) error {
	return awserror.New("ValidationError", fmt.Sprintf(format, args...))
}

func noSuchEntity(format string, args ...any) error {
	return awserror.New("NoSuchEntityException", fmt.Sprintf(format, args...))
}

func alreadyExists(format string, args ...any) error {
	return awserror.New("EntityAlreadyExistsException", fmt.Sprintf(format, args...))
}

func deleteConflict(message string) error {
	return awserror.New("DeleteConflictException", message)
}

func limitExceeded(format string, args ...any) error {
	return awserror.New("LimitExceededException", fmt.Sprintf(format, args...))
}

func invalidInput(format string, args ...any) error {
	return awserror.New("InvalidInputException", fmt.Sprintf(format, args...))
}

// key is the storage key of a record: IAM state is account-wide, so the key has
// no region. Names are case-insensitive in IAM, so two roles or two policies
// cannot differ only in case.
func key(account, kind, name string) string {
	return account + "/iam/" + kind + "/" + strings.ToLower(name)
}

func arn(account, resource string) string { return "arn:aws:iam::" + account + ":" + resource }

func millis(ms int64) *time.Time { return new(time.UnixMilli(ms).UTC()) }

// newID returns an IAM unique ID: prefix and 16 base32 characters.
func newID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + base32.StdEncoding.EncodeToString(b[:])
}

// list decodes every record of kind in account, ordered by key.
func list[T any](tx storage.ReadTx, account, kind string) ([]*T, error) {
	return storage.Records[T](tx, key(account, kind, ""))
}

func required(name string, v *string) error {
	if v == nil {
		return validation("1 validation error detected: Value null at '%s' failed to satisfy constraint: "+
			"Member must not be null", name)
	}

	return nil
}

func checkName(field string, name *string, pattern *regexp.Regexp) error {
	if err := required(field, name); err != nil {
		return err
	}

	if !pattern.MatchString(*name) {
		return validation("The specified value for %s is invalid. It must contain only alphanumeric characters "+
			"and/or the following: +=,.@_-", field)
	}

	return nil
}

// checkPath returns path, defaulting to "/", if it is a valid IAM path.
func checkPath(path *string, pattern *regexp.Regexp) (string, error) {
	if path == nil {
		return "/", nil
	}

	if len(*path) > 512 || !pattern.MatchString(*path) {
		return "", validation("The specified value for path is invalid. It must begin and end with / and contain " +
			"only alphanumeric characters and/or / characters.")
	}

	return *path, nil
}

// checkDocument requires a policy document to be a JSON object with a Statement.
func checkDocument(field string, doc *string) error {
	if err := required(field, doc); err != nil {
		return err
	}

	if *doc == "" || len(*doc) > maxDocument {
		return validation("%s must be 1 to %d characters long", field, maxDocument)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*doc), &parsed); err != nil {
		return awserror.New("MalformedPolicyDocumentException", "This policy contains invalid Json")
	}

	if _, ok := parsed["Statement"]; !ok {
		return awserror.New("MalformedPolicyDocumentException", "Syntax errors in policy.")
	}

	return nil
}

// encodeDocument URL-encodes a policy document, as IAM returns them.
func encodeDocument(doc string) *string {
	return new(strings.ReplaceAll(url.QueryEscape(doc), "+", "%20"))
}

// tagMap converts request tags, rejecting duplicates and IAM's tag limits.
func tagMap(tags []api.Tag) (map[string]string, error) {
	return tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		validation("Tags must have a Key and a Value"),
		func(string) error {
			return invalidInput("Duplicate tag keys found. Please note that Tag keys are case insensitive.")
		},
	)
}

// tagError reports a tagging limit violation with IAM's error codes.
func tagError(err error) error {
	switch {
	case errors.Is(err, tagging.ErrLimit):
		return limitExceeded("The number of tags has reached the maximum limit of %d.", tagging.Max)
	case err != nil:
		return invalidInput("%s", err.Error())
	}

	return nil
}

// tagList returns tags ordered by key.
func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} })
}

// page returns the items after marker, at most maxItems of them, whether more
// remain, and the marker of the next page. items must be ordered by sortKey.
func page[T any](items []T, sortKey func(T) string, marker *string, maxItems *int64) (
	selected []T,
	truncated *bool,
	next *string,
	err error,
) {
	size := int64(defaultPageSize)
	if maxItems != nil {
		if *maxItems < 1 || *maxItems > maxPageSize {
			return nil, nil, nil, validation("MaxItems must be between 1 and %d", maxPageSize)
		}

		size = *maxItems
	}

	if marker != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*marker)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, nil, validation("Invalid Marker")
		}

		start, _ := slices.BinarySearchFunc(items, string(after), func(item T, target string) int {
			return strings.Compare(sortKey(item), target)
		})
		if start < len(items) && sortKey(items[start]) == string(after) {
			start++
		}

		items = items[start:]
	}

	if int64(len(items)) <= size {
		return items, new(false), nil, nil
	}

	items = items[:size]
	return items, new(true), new(base64.RawURLEncoding.EncodeToString([]byte(sortKey(items[size-1])))), nil
}
