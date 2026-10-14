// Package ssm implements the Parameter Store subset of AWS Systems Manager,
// without HTTP dependencies. Parameters belong to an account and region. Each
// version is an immutable record; the parameter record holds the current
// version number, labels, and tags.
package ssm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	kmsapi "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/kms"
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
	// publicPrefix starts the names of the public parameters AWS publishes.
	publicPrefix = "/aws/service/"
	// keyType is the Type filter key and member name.
	keyType = "Type"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store     storage.Store
	keys      *kms.Service
	public    PublicParameters
	instances Instances
	now       func() time.Time
	// bg runs the sweeper that applies parameter policies and finishes
	// commands, until Stop.
	bg *background.Group
}

// PublicParameters returns the public parameters AWS publishes in a Region
// under /aws/service/, by name.
type PublicParameters func(region string) map[string]string

// publicModified is when the public parameters were last changed, in Unix
// milliseconds.
const publicModified = 1736899200000

// UsePublicParameters serves the public parameters that fn returns, such as
// the EC2 image IDs; call it before serving requests.
func (s *Service) UsePublicParameters(fn PublicParameters) { s.public = fn }

// publicParameters returns the public parameters of region as current
// parameters.
func (s *Service) publicParameters(region string) []current {
	if s.public == nil {
		return nil
	}

	var out []current
	for name, v := range s.public(region) {
		out = append(out, current{
			parameter: &parameter{Name: name, Version: 1},
			version: &version{
				Value: v, Type: typeString, Tier: tierStandard, DataType: dataTypeText, Version: 1,
				ModifiedAt: publicModified,
			},
		})
	}

	slices.SortFunc(out, func(a, b current) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// New returns Systems Manager and starts its sweeper, which runs until Stop;
// keys encrypts and decrypts SecureString parameters.
func New(store storage.Store, keys *kms.Service) *Service {
	s := &Service{store: store, keys: keys, now: time.Now, bg: background.New()}
	s.bg.Every(sweepInterval, s.sweep)

	return s
}

// Stop ends the sweeper and waits for it. Later calls return at once.
func (s *Service) Stop() { s.bg.Stop() }

// keyError reports a KMS failure as InvalidKeyId, as Parameter Store does for
// a key that is missing, disabled, pending deletion, or of the wrong kind.
func keyError(err error) error {
	if e, ok := errors.AsType[*awserror.Error](err); ok {
		return awserror.New("InvalidKeyId", e.Message)
	}

	return err
}

// chunkSize is how much plaintext one KMS call encrypts.
const chunkSize = 4096

// seal encrypts a SecureString value under the KMS key keyID, with the
// parameter's ARN as the encryption context. A value longer than KMS accepts
// is encrypted in chunks, whose base64 forms are joined with dots.
func (s *Service) seal(ctx context.Context, scope request.Scope, name, keyID, plaintext string) (string, error) {
	if keyID == defaultKeyID {
		if err := s.keys.EnsureManagedKey(ctx, scope, "ssm"); err != nil {
			return "", err
		}
	}

	var chunks []string
	for rest := []byte(plaintext); len(rest) > 0; {
		n := min(len(rest), chunkSize)
		out, err := s.keys.Encrypt(ctx, &kmsapi.EncryptRequest{
			KeyId: &keyID, Plaintext: rest[:n], EncryptionContext: encryptionContext(scope, name),
		})
		if err != nil {
			return "", keyError(err)
		}

		chunks = append(chunks, base64.StdEncoding.EncodeToString(out.CiphertextBlob))
		rest = rest[n:]
	}

	return strings.Join(chunks, "."), nil
}

func encryptionContext(scope request.Scope, name string) map[string]string {
	return map[string]string{"PARAMETER_ARN": parameterARN(scope, name)}
}

// unseal decrypts a SecureString version. The key must still be usable.
func (s *Service) unseal(ctx context.Context, scope request.Scope, name string, v *version) (string, error) {
	var plain strings.Builder
	for chunk := range strings.SplitSeq(v.Ciphertext, ".") {
		blob, err := base64.StdEncoding.DecodeString(chunk)
		if err != nil {
			return "", awserror.New("InvalidKeyId", "The stored parameter value is not valid ciphertext.")
		}

		out, err := s.keys.Decrypt(ctx, &kmsapi.DecryptRequest{
			CiphertextBlob: blob, EncryptionContext: encryptionContext(scope, name),
		})
		if err != nil {
			return "", keyError(err)
		}

		plain.Write(out.Plaintext)
	}

	return plain.String(), nil
}

// revealed is a SecureString version a read decrypts, and where the plaintext
// goes.
type revealed struct {
	version *version
	set     func(plaintext string)
	name    string
}

// reveal decrypts the SecureString versions of a read with decryption.
func (s *Service) reveal(ctx context.Context, scope request.Scope, items []revealed) error {
	for _, item := range items {
		plain, err := s.unseal(ctx, scope, item.name, item.version)
		if err != nil {
			return err
		}

		item.set(plain)
	}

	return nil
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
	// Value is empty for a SecureString, which holds its Ciphertext.
	Value string `json:",omitempty"`
	// Ciphertext is the KMS encryption of a SecureString value, base64 encoded.
	Ciphertext     string `json:",omitempty"`
	Type           string
	KeyID          string `json:",omitempty"`
	Description    string `json:",omitempty"`
	AllowedPattern string `json:",omitempty"`
	Tier           string
	DataType       string
	// Policies are the parameter policies of an advanced parameter, and
	// ExpiresAt, in Unix milliseconds, is when its Expiration policy deletes it.
	Policies   []policy `json:",omitempty"`
	Version    int64
	ModifiedAt int64 // Unix milliseconds
	ExpiresAt  int64 `json:",omitempty"`
}

var (
	nameChars  = regexp.MustCompile(`^[a-zA-Z0-9_.\-/]+$`)
	labelChars = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+$`)
)

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

// parameterARN returns a parameter's ARN; AWS owns the public parameters,
// whose names no account can create.
func parameterARN(scope request.Scope, name string) string {
	account := scope.AccountID
	if strings.HasPrefix(name, publicPrefix) {
		account = ""
	}

	return "arn:aws:ssm:" + scope.Region + ":" + account + ":parameter/" + strings.TrimPrefix(name, "/")
}

func seconds(ms int64) *float64 { return new(float64(ms) / 1000) }

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
	p, ok, err := storage.Read[parameter](tx, parameterKey(scope, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound(name)
	}

	return p, nil
}

func loadVersion(tx storage.ReadTx, scope request.Scope, name string, v int64) (*version, bool, error) {
	return storage.Read[version](tx, versionKey(scope, name, v))
}

// resolve returns the version that name, with an optional ":version" or
// ":label" selector, refers to: a stored parameter or a public one.
func (s *Service) resolve(tx storage.ReadTx, scope request.Scope, name string) (
	p *parameter,
	v *version,
	selector string,
	err error,
) {
	base, sel, hasSelector := strings.Cut(name, ":")
	if guardErr := checkName(&base); guardErr != nil {
		return nil, nil, "", guardErr
	}

	if strings.HasPrefix(base, publicPrefix) {
		for _, c := range s.publicParameters(scope.Region) {
			if c.Name != base {
				continue
			}

			if hasSelector && sel != "1" {
				return nil, nil, "", versionNotFound(sel, base)
			}

			if hasSelector {
				selector = ":" + sel
			}

			return c.parameter, c.version, selector, nil
		}
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

// stored returns what a read without decryption returns for v: a
// SecureString's ciphertext.
func (v *version) stored() string {
	if v.Type == typeSecureString {
		return v.Ciphertext
	}

	return v.Value
}

func (v *version) parameter(scope request.Scope, name, selector string) api.Parameter {
	out := api.Parameter{
		Name:             new(name),
		Type:             new(v.Type),
		Value:            new(v.stored()),
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
		Policies:         v.policyOutput(),
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
