// Package secretsmanager implements AWS Secrets Manager secrets, versions,
// and staging labels without HTTP dependencies. Secrets belong to an account
// and region. A secret's metadata record never holds a value: each version's
// value is a separate record, so listing and describing cannot expose one.
package secretsmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	stageCurrent  = "AWSCURRENT"
	stagePrevious = "AWSPREVIOUS"
	defaultKeyID  = "alias/aws/secretsmanager"

	maxValue          = 65536
	maxDescription    = 2048
	maxVersions       = 100
	maxStages         = 20
	maxStageLength    = 256
	minToken          = 32
	maxToken          = 64
	minRecoveryWindow = 7
	maxRecoveryWindow = 30
	maxPage           = 100
	suffixLength      = 6
	day               = 24 * time.Hour
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

// secret is a secret's metadata.
type secret struct {
	Tags        map[string]string `json:",omitempty"`
	Stages      map[string]string `json:",omitempty"` // staging label to version ID
	Name        string
	Suffix      string // the random end of the ARN
	Description string `json:",omitempty"`
	KmsKeyID    string `json:",omitempty"`
	Versions    []versionInfo
	CreatedAt   int64 // Unix milliseconds
	ChangedAt   int64
	// DeletionAt is when a secret scheduled for deletion goes away, or 0.
	DeletionAt int64 `json:",omitempty"`
}

type versionInfo struct {
	ID        string
	CreatedAt int64
}

// payload is one version's value.
type payload struct {
	String *string `json:",omitempty"`
	Binary []byte  `json:",omitempty"`
}

var (
	secretName = regexp.MustCompile(`^[a-zA-Z0-9/_+=.@-]{1,512}$`)
	suffixSet  = []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
)

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return m, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func invalidRequest(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

func errNoSecret() error {
	return notFound("Secrets Manager can't find the specified secret.")
}

func errScheduled() error {
	return invalidRequest("You can't perform this operation on the secret because it was marked for deletion.")
}

func secretKey(scope request.Scope, name string) string {
	return scope.AccountID + "/" + scope.Region + "/secretsmanager/secret/" + name
}

// valueKey places a version's value: names cannot contain ":".
func valueKey(scope request.Scope, name, id string) string {
	return scope.AccountID + "/" + scope.Region + "/secretsmanager/value/" + name + ":" + id
}

func (s *secret) arn(scope request.Scope) string {
	return "arn:aws:secretsmanager:" + scope.Region + ":" + scope.AccountID + ":secret:" + s.Name + "-" + s.Suffix
}

func (s *secret) scheduled() bool { return s.DeletionAt != 0 }

func seconds(ms int64) *float64 { return new(float64(ms) / 1000) }

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return new(s)
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func newSuffix() string {
	out := random(suffixLength)
	for i, b := range out {
		out[i] = suffixSet[int(b)%len(suffixSet)]
	}

	return string(out)
}

// newVersionID returns a random UUID, as Secrets Manager generates.
func newVersionID() string {
	b := random(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

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

// load returns the secret named name, including one past its deletion date,
// which callers treat as gone.
func load(tx storage.ReadTx, scope request.Scope, name string) (*secret, bool, error) {
	return read[secret](tx, secretKey(scope, name))
}

// find returns the secret that id names by name, full ARN, or partial ARN
// (without the random suffix). A secret whose recovery window has ended is
// gone; one still scheduled for deletion is returned.
func find(tx storage.ReadTx, scope request.Scope, id *string, now time.Time) (*secret, error) {
	if id == nil || *id == "" || len(*id) > 2048 {
		return nil, invalidParameter("SecretId must be 1 to 2048 characters long.")
	}

	names := []string{*id}
	suffix := ""
	if rest, ok := strings.CutPrefix(*id, "arn:aws:secretsmanager:"); ok {
		parts := strings.SplitN(rest, ":", 4)
		if len(parts) != 4 || parts[0] != scope.Region || parts[1] != scope.AccountID || parts[2] != "secret" {
			return nil, errNoSecret()
		}

		names = []string{parts[3]}
		if n := len(parts[3]); n > suffixLength+1 && parts[3][n-suffixLength-1] == '-' {
			names = []string{parts[3][:n-suffixLength-1], parts[3]}
			suffix = parts[3][n-suffixLength:]
		}
	}

	for i, name := range names {
		s, ok, err := load(tx, scope, name)
		if err != nil {
			return nil, err
		}

		// A full ARN must carry the secret's own suffix.
		fullARN := suffix != "" && i == 0
		if ok && (!fullARN || s.Suffix == suffix) && !expired(s, now) {
			return s, nil
		}
	}

	return nil, errNoSecret()
}

// findLive is find for operations that a scheduled deletion blocks.
func findLive(tx storage.ReadTx, scope request.Scope, id *string, now time.Time) (*secret, error) {
	s, err := find(tx, scope, id, now)
	if err == nil && s.scheduled() {
		return nil, errScheduled()
	}

	return s, err
}

func expired(s *secret, now time.Time) bool {
	return s.scheduled() && s.DeletionAt <= now.UnixMilli()
}

// purge deletes a secret with every version's value.
func purge(tx storage.WriteTx, scope request.Scope, s *secret) {
	for _, v := range s.Versions {
		tx.Delete(valueKey(scope, s.Name, v.ID))
	}

	tx.Delete(secretKey(scope, s.Name))
}

// stagesOf returns the staging labels on each version, sorted.
func (s *secret) stagesOf() map[string][]string {
	out := map[string][]string{}
	for stage, id := range s.Stages {
		out[id] = append(out[id], stage)
	}

	for _, stages := range out {
		slices.Sort(stages)
	}

	return out
}

func (s *secret) hasVersion(id string) bool {
	return slices.ContainsFunc(s.Versions, func(v versionInfo) bool { return v.ID == id })
}

// setStage moves stage to version id. Moving AWSCURRENT labels the version
// that had it AWSPREVIOUS.
func (s *secret) setStage(stage, id string) {
	if s.Stages == nil {
		s.Stages = map[string]string{}
	}

	if previous := s.Stages[stage]; stage == stageCurrent && previous != "" && previous != id {
		s.Stages[stagePrevious] = previous
	}

	s.Stages[stage] = id
}

// checkStageLimit requires every version to have at most 20 labels.
func (s *secret) checkStageLimit() error {
	for _, stages := range s.stagesOf() {
		if len(stages) > maxStages {
			return awserror.New("LimitExceededException",
				fmt.Sprintf("A version can have at most %d staging labels.", maxStages))
		}
	}

	return nil
}

// checkValue returns the value a request sets, and whether it sets one.
func checkValue(str *string, binary []byte) (*payload, bool, error) {
	switch {
	case str != nil && binary != nil:
		return nil, false, invalidParameter("You can't specify both a binary secret value and a string secret " +
			"value in the same secret.")
	case str != nil && (*str == "" || len(*str) > maxValue):
		return nil, false, invalidParameter("SecretString must be 1 to %d bytes long.", maxValue)
	case binary != nil && (len(binary) == 0 || len(binary) > maxValue):
		return nil, false, invalidParameter("SecretBinary must be 1 to %d bytes long.", maxValue)
	case str == nil && binary == nil:
		return nil, false, nil
	}

	return &payload{String: str, Binary: binary}, true, nil
}

func (p *payload) equal(other *payload) bool {
	return (p.String == nil) == (other.String == nil) && (p.String == nil || *p.String == *other.String) &&
		bytes.Equal(p.Binary, other.Binary)
}

func checkToken(token *string) error {
	if token != nil && (len(*token) < minToken || len(*token) > maxToken) {
		return invalidParameter("ClientRequestToken must be %d to %d characters long.", minToken, maxToken)
	}

	return nil
}

func checkStage(stage string) error {
	if stage == "" || len(stage) > maxStageLength {
		return invalidParameter("Staging labels must be 1 to %d characters long.", maxStageLength)
	}

	return nil
}

// checkKey accepts only the AWS managed key, the one TinyStack has.
func checkKey(keyID *string) error {
	if keyID != nil && *keyID != defaultKeyID {
		return invalidParameter("TinyStack supports only the AWS managed key %s.", defaultKeyID)
	}

	return nil
}

func checkDescription(description *string) error {
	if description != nil && len(*description) > maxDescription {
		return invalidParameter("Description must be at most %d characters long.", maxDescription)
	}

	return nil
}

// addVersion stores a new version of s with value, labeled stages. A
// request token that names an existing version is idempotent when the value
// matches. The oldest unlabeled versions beyond 100 are removed.
func addVersion(
	tx storage.WriteTx,
	scope request.Scope,
	s *secret,
	token *string,
	value *payload,
	stages []string,
	now int64,
) (string, error) {
	id := newVersionID()
	if token != nil {
		id = *token
	}

	if s.hasVersion(id) {
		existing, _, err := read[payload](tx, valueKey(scope, s.Name, id))
		if err != nil {
			return "", err
		}

		if !existing.equal(value) {
			return "", awserror.New("ResourceExistsException",
				"You can't modify an existing version, you can only create a new version.")
		}

		return id, nil
	}

	if err := write(tx, valueKey(scope, s.Name, id), value); err != nil {
		return "", err
	}

	s.Versions = append(s.Versions, versionInfo{ID: id, CreatedAt: now})
	for _, stage := range stages {
		s.setStage(stage, id)
	}

	if err := s.checkStageLimit(); err != nil {
		return "", err
	}

	labeled := s.stagesOf()
	for len(s.Versions) > maxVersions {
		i := slices.IndexFunc(s.Versions, func(v versionInfo) bool { return len(labeled[v.ID]) == 0 })
		if i < 0 {
			break
		}

		tx.Delete(valueKey(scope, s.Name, s.Versions[i].ID))
		s.Versions = slices.Delete(s.Versions, i, i+1)
	}

	s.ChangedAt = now
	return id, nil
}

// tagMap converts request tags, checked against the shared limits.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.Key == nil || t.Value == nil {
			return nil, invalidParameter("Tags must have a Key and a Value.")
		}

		out[*t.Key] = *t.Value
	}

	return out, tagError(tagging.Check(out))
}

func tagError(err error) error {
	if err != nil {
		return invalidParameter("%s", err.Error())
	}

	return nil
}

func tagList(tags map[string]string) []api.Tag {
	if len(tags) == 0 {
		return nil
	}

	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	out := make([]api.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return out
}

// page returns the items after token, at most size of them (default and
// maximum 100), and the token of the next page. items must be ordered by
// sortKey, descending when desc.
func page[T any](items []T, sortKey func(T) string, token *string, size *int64, desc bool) (
	selected []T,
	next *string,
	err error,
) {
	n := int64(maxPage)
	if size != nil {
		if *size < 1 || *size > maxPage {
			return nil, nil, invalidParameter("MaxResults must be between 1 and %d.", maxPage)
		}

		n = *size
	}

	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, awserror.New("InvalidNextTokenException", "The NextToken value is not valid.")
		}

		start := slices.IndexFunc(items, func(item T) bool {
			c := strings.Compare(sortKey(item), string(after))
			return (c > 0 && !desc) || (c < 0 && desc)
		})
		if start < 0 {
			start = len(items)
		}

		items = items[start:]
	}

	if int64(len(items)) <= n {
		return items, nil, nil
	}

	items = items[:n]
	return items, new(base64.RawURLEncoding.EncodeToString([]byte(sortKey(items[n-1])))), nil
}
