// Package ssm implements the Parameter Store subset of AWS Systems Manager,
// without HTTP dependencies. Parameters belong to an account and region. Each
// version is an immutable record; the parameter record holds the current
// version number, labels, and tags.
package ssm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	typeString       = "String"
	typeStringList   = "StringList"
	typeSecureString = "SecureString"

	tierStandard     = "Standard"
	tierAdvanced     = "Advanced"
	tierIntelligent  = "Intelligent-Tiering"
	dataTypeText     = "text"
	defaultKeyID     = "alias/aws/ssm"
	resourceType     = "Parameter"
	maxStandardValue = 4096
	maxAdvancedValue = 8192
	maxARN           = 1011
	maxDescription   = 1024
	maxPattern       = 1024
	maxLevels        = 15
	maxVersions      = 100
	maxLabels        = 10
	maxLabelLength   = 100
	maxBatch         = 10
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

// parameter is a parameter's mutable record.
type parameter struct {
	Tags    map[string]string `json:",omitempty"`
	Labels  map[string]int64  `json:",omitempty"` // label to version
	Name    string
	Version int64 // current version
}

// version is one immutable parameter version.
type version struct {
	Value string
	// Ciphertext stands in for a SecureString value read without decryption.
	Ciphertext     string `json:",omitempty"`
	Type           string
	KeyID          string `json:",omitempty"`
	Description    string `json:",omitempty"`
	AllowedPattern string `json:",omitempty"`
	Tier           string
	DataType       string
	Version        int64
	ModifiedAt     int64 // Unix milliseconds
}

var (
	nameChars  = regexp.MustCompile(`^[a-zA-Z0-9_.\-/]+$`)
	labelChars = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+$`)
)

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func validation(format string, args ...any) error {
	return awserror.New("ValidationException", fmt.Sprintf(format, args...))
}

func notFound(name string) error {
	return awserror.New("ParameterNotFound", fmt.Sprintf("Parameter %s not found.", name))
}

func versionNotFound(selector, name string) error {
	return awserror.New("ParameterVersionNotFound",
		fmt.Sprintf("Systems Manager could not find version %s of %s. Verify the version and try again.",
			selector, name))
}

func parameterKey(scope request.Scope, name string) string {
	return scope.AccountID + "/" + scope.Region + "/ssm/p/" + name
}

// versionPrefix orders a parameter's versions: names cannot contain ":".
func versionPrefix(scope request.Scope, name string) string {
	return scope.AccountID + "/" + scope.Region + "/ssm/v/" + name + ":"
}

func versionKey(scope request.Scope, name string, v int64) string {
	return versionPrefix(scope, name) + fmt.Sprintf("%010d", v)
}

func parameterARN(scope request.Scope, name string) string {
	return "arn:aws:ssm:" + scope.Region + ":" + scope.AccountID + ":parameter/" + strings.TrimPrefix(name, "/")
}

func seconds(ms int64) *float64 { return new(float64(ms) / 1000) }

func read[T any](tx storage.ReadTx, k string) (v *T, found bool, err error) {
	data, ok := tx.Get(k)
	if !ok {
		return nil, false, nil
	}

	v = new(T)
	if err = json.Unmarshal(data, v); err != nil {
		return nil, false, fmt.Errorf("decode %s: %w", k, err)
	}

	return v, true, nil
}

func write(tx storage.WriteTx, k string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	tx.Put(k, data)
	return nil
}

// checkName validates a parameter name as a request refers to one.
func checkName(name *string) error {
	if name == nil || *name == "" || len(*name) > 2048 || !nameChars.MatchString(*name) {
		return validation("Parameter name must contain only letters, numbers, and the symbols .-_/, " +
			"and be 1 to 2048 characters long.")
	}

	return nil
}

// checkNewName also applies the rules for creating a parameter: fully
// qualified hierarchies, at most 15 levels, and no reserved prefix.
func checkNewName(scope request.Scope, name string) error {
	if strings.Contains(name, "/") &&
		(!strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//")) {
		return validation("Parameter name: a fully qualified hierarchy name must begin with a forward slash " +
			"(/), and must not end with one or contain empty levels.")
	}

	if strings.Count(name, "/") > maxLevels {
		return awserror.New("HierarchyLevelLimitExceededException",
			fmt.Sprintf("A hierarchy can have a maximum of %d levels.", maxLevels))
	}

	lower := strings.ToLower(strings.TrimPrefix(name, "/"))
	if strings.HasPrefix(lower, "aws") || strings.HasPrefix(lower, "ssm") {
		return validation(`Parameter name: can't be prefixed with "aws" or "ssm" (case-insensitive).`)
	}

	if len(parameterARN(scope, name)) > maxARN {
		return validation("Parameter name: the full ARN of a parameter can be at most %d characters long.", maxARN)
	}

	return nil
}

// checkLabel reports whether a version label is valid.
func checkLabel(label string) bool {
	lower := strings.ToLower(label)
	return label != "" && len(label) <= maxLabelLength && labelChars.MatchString(label) &&
		(label[0] < '0' || label[0] > '9') && !strings.HasPrefix(lower, "aws") && !strings.HasPrefix(lower, "ssm")
}

// loadParameter returns the parameter named name, which has no selector.
func loadParameter(tx storage.ReadTx, scope request.Scope, name string) (*parameter, error) {
	p, ok, err := read[parameter](tx, parameterKey(scope, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound(name)
	}

	return p, nil
}

func loadVersion(tx storage.ReadTx, scope request.Scope, name string, v int64) (*version, bool, error) {
	return read[version](tx, versionKey(scope, name, v))
}

// resolve returns the version that name, with an optional ":version" or
// ":label" selector, refers to.
func resolve(tx storage.ReadTx, scope request.Scope, name string) (
	p *parameter,
	v *version,
	selector string,
	err error,
) {
	base, sel, hasSelector := strings.Cut(name, ":")
	if guardErr := checkName(&base); guardErr != nil {
		return nil, nil, "", guardErr
	}

	if p, err = loadParameter(tx, scope, base); err != nil {
		return nil, nil, "", err
	}

	number := p.Version
	if hasSelector {
		selector = ":" + sel
		if n, parseErr := strconv.ParseInt(sel, 10, 64); parseErr == nil {
			number = n
		} else if labeled, ok := p.Labels[sel]; ok {
			number = labeled
		} else {
			return nil, nil, "", versionNotFound(sel, base)
		}
	}

	v, ok, err := loadVersion(tx, scope, base, number)
	if err != nil {
		return nil, nil, "", err
	}

	if !ok {
		return nil, nil, "", versionNotFound(sel, base)
	}

	return p, v, selector, nil
}

// value returns what a read returns for v: a SecureString value only with
// decryption.
func (v *version) value(decrypt bool) string {
	if v.Type == typeSecureString && !decrypt {
		return v.Ciphertext
	}

	return v.Value
}

func (v *version) parameter(scope request.Scope, name, selector string, decrypt bool) api.Parameter {
	out := api.Parameter{
		Name:             new(name),
		Type:             new(v.Type),
		Value:            new(v.value(decrypt)),
		Version:          new(v.Version),
		LastModifiedDate: seconds(v.ModifiedAt),
		ARN:              new(parameterARN(scope, name)),
		DataType:         new(v.DataType),
	}
	if selector != "" {
		out.Selector = new(selector)
	}

	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return new(s)
}

func (v *version) metadata(scope request.Scope, name string) api.ParameterMetadata {
	return api.ParameterMetadata{
		Name:             new(name),
		ARN:              new(parameterARN(scope, name)),
		Type:             new(v.Type),
		KeyId:            optional(v.KeyID),
		LastModifiedDate: seconds(v.ModifiedAt),
		Description:      optional(v.Description),
		AllowedPattern:   optional(v.AllowedPattern),
		Version:          new(v.Version),
		Tier:             new(v.Tier),
		DataType:         new(v.DataType),
	}
}

// labelsOf returns the labels on each version of p, sorted.
func labelsOf(p *parameter) map[int64][]string {
	out := map[int64][]string{}
	for label, v := range p.Labels {
		out[v] = append(out[v], label)
	}

	for _, labels := range out {
		slices.Sort(labels)
	}

	return out
}

// page returns the items after token, at most size of them (default and
// maximum limit), and the token of the next page. items must be ordered by
// sortKey.
func page[T any](items []T, sortKey func(T) string, token *string, size *int64, limit int64) (
	selected []T,
	next *string,
	err error,
) {
	n := limit
	if size != nil {
		if *size < 1 || *size > limit {
			return nil, nil, validation("MaxResults must be between 1 and %d.", limit)
		}

		n = *size
	}

	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, awserror.New("InvalidNextToken", "The specified token is not valid.")
		}

		start, found := slices.BinarySearchFunc(items, string(after), func(item T, target string) int {
			return strings.Compare(sortKey(item), target)
		})
		if found {
			start++
		}

		items = items[start:]
	}

	if int64(len(items)) <= n {
		return items, nil, nil
	}

	items = items[:n]
	return items, new(base64.RawURLEncoding.EncodeToString([]byte(sortKey(items[n-1])))), nil
}
