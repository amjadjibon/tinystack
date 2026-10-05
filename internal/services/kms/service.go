// Package kms implements symmetric encryption and HMAC KMS keys, aliases,
// envelope encryption, data key pairs, and MACs, without HTTP dependencies.
// Keys and aliases belong to an account and region:
//
//	<scope>/kms/key/<key ID>
//	<scope>/kms/alias/<name without the alias/ prefix>
//
// Each key record holds its key material (256 bits for encryption keys, the
// HMAC length for HMAC keys), which is never rotated.
// Policies are stored, never evaluated. A key past its scheduled deletion date
// is treated as gone.
package kms

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	stateEnabled         = "Enabled"
	stateDisabled        = "Disabled"
	statePendingDeletion = "PendingDeletion"
	symmetricDefault     = "SYMMETRIC_DEFAULT"
	encryptDecrypt       = "ENCRYPT_DECRYPT"
	generateVerifyMAC    = "GENERATE_VERIFY_MAC"
	originKMS            = "AWS_KMS"
	defaultPolicyName    = "default"
	maxDescription       = 8192
	maxPolicy            = 32768
	day                  = 24 * time.Hour
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	now   func() time.Time
}

func New(store storage.Store) *Service {
	return &Service{store: store, now: time.Now}
}

// key is a KMS key record. Times are Unix milliseconds.
type key struct {
	Tags        map[string]string `json:",omitempty"`
	ID          string
	Description string
	Policy      string
	State       string
	// Spec and Usage are empty for symmetric encryption keys.
	Spec     string `json:",omitempty"`
	Usage    string `json:",omitempty"`
	Material []byte
	Created  int64
	// DeletionDate and PendingDays are set while the key is pending deletion.
	DeletionDate int64 `json:",omitempty"`
	PendingDays  int64 `json:",omitempty"`
	// RotationDays is the rotation period, zero while rotation is disabled, and
	// RotationStart is when rotation was enabled.
	RotationDays  int64 `json:",omitempty"`
	RotationStart int64 `json:",omitempty"`
}

var keyID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func notFound(format string, args ...any) error {
	return awserror.New("NotFoundException", fmt.Sprintf(format, args...))
}

func invalidState(format string, args ...any) error {
	return awserror.New("KMSInvalidStateException", fmt.Sprintf(format, args...))
}

func unsupported(format string, args ...any) error {
	return awserror.New("UnsupportedOperationException", fmt.Sprintf(format, args...))
}

func keyKey(scope request.Scope, id string) string { return scope.Prefix("kms", "key") + id }

func keyARN(scope request.Scope, id string) string {
	return "arn:aws:kms:" + scope.Region + ":" + scope.AccountID + ":key/" + id
}

func seconds(ms int64) *float64 { return new(float64(ms) / 1000) }

func newKeyID() string { return uuid.NewV4().String() }

func (k *key) spec() string  { return cmp.Or(k.Spec, symmetricDefault) }
func (k *key) usage() string { return cmp.Or(k.Usage, encryptDecrypt) }

// requireUsage fails unless the key has the usage an operation needs.
func (k *key) requireUsage(scope request.Scope, usage string) error {
	if k.usage() != usage {
		return awserror.New("InvalidKeyUsageException", keyARN(scope, k.ID)+" key usage is "+k.usage()+
			" which is not valid for this operation.")
	}

	return nil
}

// deleted reports whether the key's scheduled deletion date has passed.
func (k *key) deleted(now time.Time) bool {
	return k.State == statePendingDeletion && now.UnixMilli() >= k.DeletionDate
}

// usable reports why the key cannot encrypt or decrypt, if it cannot.
func (k *key) usable(scope request.Scope) error {
	switch k.State {
	case stateDisabled:
		return awserror.New("DisabledException", keyARN(scope, k.ID)+" is disabled.")
	case statePendingDeletion:
		return invalidState("%s is pending deletion.", keyARN(scope, k.ID))
	}

	return nil
}

// active fails unless the key is enabled or disabled, as operations that change
// a key require.
func (k *key) active(scope request.Scope) error {
	if k.State == statePendingDeletion {
		return invalidState("%s is pending deletion.", keyARN(scope, k.ID))
	}

	return nil
}

func (k *key) metadata(scope request.Scope) *api.KeyMetadata {
	m := &api.KeyMetadata{
		AWSAccountId:          new(scope.AccountID),
		KeyId:                 new(k.ID),
		Arn:                   new(keyARN(scope, k.ID)),
		CreationDate:          seconds(k.Created),
		Enabled:               new(k.State == stateEnabled),
		Description:           new(k.Description),
		KeyUsage:              new(k.usage()),
		KeyState:              new(k.State),
		Origin:                new(originKMS),
		KeyManager:            new("CUSTOMER"),
		CustomerMasterKeySpec: new(k.spec()),
		KeySpec:               new(k.spec()),
		MultiRegion:           new(false),
	}
	if mac, ok := macKeys[k.spec()]; ok {
		m.MacAlgorithms = []string{mac.algorithm}
	} else {
		m.EncryptionAlgorithms, m.SigningAlgorithms = k.algorithms()
	}

	if k.State == statePendingDeletion {
		m.DeletionDate = seconds(k.DeletionDate)
		m.PendingDeletionWindowInDays = new(k.PendingDays)
	}

	return m
}

// resolve maps a key ID, key ARN, or (when aliases is true) an alias name or
// alias ARN to the ID of a key in the caller's scope, which may not exist.
func resolve(tx storage.ReadTx, scope request.Scope, ref string, aliases bool) (string, error) {
	name := ref
	if rest, ok := strings.CutPrefix(ref, "arn:"); ok {
		parts := strings.SplitN(rest, ":", 5)
		if len(parts) != 5 || parts[1] != "kms" {
			return "", awserror.New("InvalidArnException", "Invalid arn "+ref)
		}

		if parts[2] != scope.Region || parts[3] != scope.AccountID {
			return "", notFound("Invalid arn %s", ref)
		}

		name = parts[4]
		if id, isKey := strings.CutPrefix(name, "key/"); isKey {
			name = id
		}
	}

	aliasName, isAlias := strings.CutPrefix(name, "alias/")
	switch {
	case isAlias && aliases:
		a, found, err := storage.Read[alias](tx, aliasKey(scope, aliasName))
		if err != nil {
			return "", err
		}

		if !found {
			return "", notFound("Alias %s is not found.", aliasARN(scope, aliasName))
		}

		return a.Target, nil
	case keyID.MatchString(name):
		return name, nil
	default:
		return "", notFound("Invalid keyId '%s'", ref)
	}
}

// load resolves ref and reads the key it names.
func (s *Service) load(tx storage.ReadTx, scope request.Scope, ref *string, aliases bool) (*key, error) {
	if common.Value(ref) == "" {
		return nil, awserror.Validation("KeyId is required.")
	}

	id, err := resolve(tx, scope, *ref, aliases)
	if err != nil {
		return nil, err
	}

	k, found, err := storage.Read[key](tx, keyKey(scope, id))
	if err != nil {
		return nil, err
	}

	if !found || k.deleted(s.now()) {
		return nil, notFound("Key '%s' does not exist", keyARN(scope, id))
	}

	return k, nil
}

// view runs fn on the key that ref names.
func (s *Service) view(ctx context.Context, ref *string, aliases bool, fn func(request.Scope, *key) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error {
		k, loadErr := s.load(tx, scope, ref, aliases)
		if loadErr != nil {
			return loadErr
		}

		return fn(scope, k)
	})
}

// change runs fn on the key that ref (a key ID or ARN) names and saves it.
func (s *Service) change(ctx context.Context, ref *string, fn func(request.Scope, *key) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, loadErr := s.load(tx, scope, ref, false)
		if loadErr != nil {
			return loadErr
		}

		if fnErr := fn(scope, k); fnErr != nil {
			return fnErr
		}

		return storage.Write(tx, keyKey(scope, k.ID), k)
	})
}

// Check reports why the key that ref (a key ID, key ARN, alias name, or alias
// ARN) names in scope cannot encrypt or decrypt, for services that accept a
// customer managed key. It returns KMS's own error, which callers translate.
func (s *Service) Check(ctx context.Context, scope request.Scope, ref string) error {
	return s.store.View(ctx, func(tx storage.ReadTx) error {
		k, err := s.load(tx, scope, &ref, true)
		if err != nil {
			return err
		}

		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		return k.requireSymmetric(scope)
	})
}

// page returns the items after marker, at most limit of them (from 1 to most,
// def by default), and the marker of the next page. items must be sorted by
// id, and a marker holds the last returned item's id.
func page[T any](items []T, id func(T) string, marker *string, limit *int64, def, most int64) (
	selected []T,
	next *string,
	err error,
) {
	n := def
	if limit != nil {
		if *limit < 1 || *limit > most {
			return nil, nil, awserror.Validation("Limit must be between 1 and %d.", most)
		}

		n = *limit
	}

	if marker != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*marker)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, awserror.New("InvalidMarkerException", "Invalid marker "+*marker)
		}

		start := slices.IndexFunc(items, func(item T) bool { return id(item) > string(after) })
		if start < 0 {
			start = len(items)
		}

		items = items[start:]
	}

	if int64(len(items)) <= n {
		return items, nil, nil
	}

	items = items[:n]
	return items, new(base64.RawURLEncoding.EncodeToString([]byte(id(items[n-1])))), nil
}

func checkDescription(description *string) error {
	if len(common.Value(description)) > maxDescription {
		return awserror.Validation("Description must be at most %d characters.", maxDescription)
	}

	return nil
}

// checkPolicy validates a key policy document; policies are never evaluated.
func checkPolicy(policy string) error {
	var document map[string]any
	if len(policy) > maxPolicy || json.Unmarshal([]byte(policy), &document) != nil || document == nil {
		return awserror.New("MalformedPolicyDocumentException", "The policy is not a valid JSON policy document.")
	}

	return nil
}

// defaultPolicy is the policy KMS gives a key created without one: the account
// may use the key through IAM.
func defaultPolicy(scope request.Scope) string {
	policy := map[string]any{
		"Version": "2012-10-17",
		"Id":      "key-default-1",
		"Statement": []map[string]any{{
			"Sid":       "Enable IAM User Permissions",
			"Effect":    "Allow",
			"Principal": map[string]string{"AWS": "arn:aws:iam::" + scope.AccountID + ":root"},
			"Action":    "kms:*",
			"Resource":  "*",
		}},
	}

	b, _ := json.Marshal(policy)
	return string(b)
}

// checkKeySpec accepts only what TinyStack implements: symmetric encryption,
// HMAC, and asymmetric keys that KMS generates, in one region. It returns the
// key's spec and usage as the record stores them, empty for symmetric
// encryption.
func checkKeySpec(in *api.CreateKeyRequest) (spec, usage string, err error) {
	if in.KeySpec != nil && in.CustomerMasterKeySpec != nil {
		return "", "", awserror.Validation("You cannot specify KeySpec and CustomerMasterKeySpec in the same " +
			"request.")
	}

	spec = cmp.Or(common.Value(in.KeySpec), common.Value(in.CustomerMasterKeySpec), symmetricDefault)
	usage = common.Value(in.KeyUsage)
	if asymmetric, ok := asymmetricKeys[spec]; ok {
		switch {
		case usage == "":
			return "", "", awserror.Validation("Asymmetric keys need a KeyUsage.")
		case !slices.Contains(asymmetric.usages, usage):
			return "", "", awserror.Validation("%s keys support the %s key usages, not %s.", spec,
				strings.Join(asymmetric.usages, " and "), usage)
		case in.Origin != nil && *in.Origin != originKMS:
			return "", "", unsupported("TinyStack supports only %s key material, not %s.", originKMS, *in.Origin)
		case common.Value(in.MultiRegion):
			return "", "", unsupported("TinyStack does not support multi-Region keys.")
		}

		return spec, usage, nil
	}

	_, isMAC := macKeys[spec]
	switch {
	case isMAC && usage != generateVerifyMAC:
		return "", "", awserror.Validation("HMAC keys must have the %s key usage.", generateVerifyMAC)
	case !isMAC && spec != symmetricDefault:
		return "", "", unsupported("TinyStack supports only %s, HMAC, RSA, ECC_NIST, and %s keys, not %s.",
			symmetricDefault, edwards, spec)
	case usage != "" && usage != encryptDecrypt && usage != generateVerifyMAC:
		return "", "", unsupported("TinyStack supports only %s and %s keys, not %s.", encryptDecrypt,
			generateVerifyMAC, usage)
	case !isMAC && usage == generateVerifyMAC:
		return "", "", awserror.Validation("%s keys must have the %s key usage.", symmetricDefault, encryptDecrypt)
	case in.Origin != nil && *in.Origin != originKMS:
		return "", "", unsupported("TinyStack supports only %s key material, not %s.", originKMS, *in.Origin)
	case common.Value(in.MultiRegion):
		return "", "", unsupported("TinyStack does not support multi-Region keys.")
	case !isMAC:
		return "", "", nil
	}

	return spec, usage, nil
}

func (s *Service) CreateKey(ctx context.Context, in *api.CreateKeyRequest) (*api.CreateKeyResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	spec, usage, err := checkKeySpec(in)
	if err != nil {
		return nil, err
	}

	if guardErr := checkDescription(in.Description); guardErr != nil {
		return nil, guardErr
	}

	policy := common.Value(in.Policy)
	if policy == "" {
		policy = defaultPolicy(scope)
	} else if guardErr := checkPolicy(policy); guardErr != nil {
		return nil, guardErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if tagErr := tagging.Check(tags); tagErr != nil {
		return nil, tagError(tagErr)
	}

	material, err := newMaterial(spec)
	if err != nil {
		return nil, err
	}

	k := &key{
		Tags: tags, ID: newKeyID(), Description: common.Value(in.Description), Policy: policy, State: stateEnabled,
		Material: material, Created: s.now().UnixMilli(), Spec: spec, Usage: usage,
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, keyKey(scope, k.ID), k)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateKeyResponse{KeyMetadata: k.metadata(scope)}, nil
}

func (s *Service) DescribeKey(ctx context.Context, in *api.DescribeKeyRequest) (*api.DescribeKeyResponse, error) {
	out := &api.DescribeKeyResponse{}
	err := s.view(ctx, in.KeyId, true, func(scope request.Scope, k *key) error {
		out.KeyMetadata = k.metadata(scope)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ListKeys lists the caller's keys, including those pending deletion, in key ID order.
func (s *Service) ListKeys(ctx context.Context, in *api.ListKeysRequest) (*api.ListKeysResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var ids []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(scope.Prefix("kms", "key")) {
			record, _, readErr := storage.Read[key](tx, k)
			if readErr != nil {
				return readErr
			}

			if !record.deleted(s.now()) {
				ids = append(ids, record.ID)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	ids, next, err := page(ids, func(id string) string { return id }, in.Marker, in.Limit, 100, 1000)
	if err != nil {
		return nil, err
	}

	out := &api.ListKeysResponse{Keys: []api.KeyListEntry{}, NextMarker: next, Truncated: new(next != nil)}
	for _, id := range ids {
		out.Keys = append(out.Keys, api.KeyListEntry{KeyId: new(id), KeyArn: new(keyARN(scope, id))})
	}

	return out, nil
}

func (s *Service) EnableKey(ctx context.Context, in *api.EnableKeyRequest) (*api.EnableKeyOutput, error) {
	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if activeErr := k.active(scope); activeErr != nil {
			return activeErr
		}

		k.State = stateEnabled
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.EnableKeyOutput{}, nil
}

func (s *Service) DisableKey(ctx context.Context, in *api.DisableKeyRequest) (*api.DisableKeyOutput, error) {
	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if activeErr := k.active(scope); activeErr != nil {
			return activeErr
		}

		k.State = stateDisabled
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DisableKeyOutput{}, nil
}

// ScheduleKeyDeletion deletes the key after 7 to 30 days (30 by default).
func (s *Service) ScheduleKeyDeletion(
	ctx context.Context,
	in *api.ScheduleKeyDeletionRequest,
) (*api.ScheduleKeyDeletionResponse, error) {
	days := int64(30)
	if in.PendingWindowInDays != nil {
		days = *in.PendingWindowInDays
		if days < 7 || days > 30 {
			return nil, awserror.Validation("PendingWindowInDays must be between 7 and 30.")
		}
	}

	out := &api.ScheduleKeyDeletionResponse{}
	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if activeErr := k.active(scope); activeErr != nil {
			return activeErr
		}

		k.State = statePendingDeletion
		k.PendingDays = days
		k.DeletionDate = s.now().Add(time.Duration(days) * day).UnixMilli()
		out = &api.ScheduleKeyDeletionResponse{
			KeyId: new(keyARN(scope, k.ID)), DeletionDate: seconds(k.DeletionDate),
			KeyState: new(k.State), PendingWindowInDays: new(days),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// CancelKeyDeletion leaves a key pending deletion disabled, as KMS does.
func (s *Service) CancelKeyDeletion(
	ctx context.Context,
	in *api.CancelKeyDeletionRequest,
) (*api.CancelKeyDeletionResponse, error) {
	out := &api.CancelKeyDeletionResponse{}
	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if k.State != statePendingDeletion {
			return invalidState("%s is not pending deletion.", keyARN(scope, k.ID))
		}

		k.State, k.DeletionDate, k.PendingDays = stateDisabled, 0, 0
		out.KeyId = new(keyARN(scope, k.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateKeyDescription(
	ctx context.Context,
	in *api.UpdateKeyDescriptionRequest,
) (*api.UpdateKeyDescriptionOutput, error) {
	if in.Description == nil {
		return nil, awserror.Validation("Description is required.")
	}

	if guardErr := checkDescription(in.Description); guardErr != nil {
		return nil, guardErr
	}

	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if activeErr := k.active(scope); activeErr != nil {
			return activeErr
		}

		k.Description = *in.Description
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateKeyDescriptionOutput{}, nil
}

// checkPolicyName accepts the only key policy name, "default".
func checkPolicyName(name *string) error {
	if name != nil && *name != defaultPolicyName {
		return notFound("Policy %s does not exist.", *name)
	}

	return nil
}

func (s *Service) GetKeyPolicy(ctx context.Context, in *api.GetKeyPolicyRequest) (*api.GetKeyPolicyResponse, error) {
	if guardErr := checkPolicyName(in.PolicyName); guardErr != nil {
		return nil, guardErr
	}

	out := &api.GetKeyPolicyResponse{PolicyName: new(defaultPolicyName)}
	err := s.view(ctx, in.KeyId, false, func(_ request.Scope, k *key) error {
		out.Policy = new(k.Policy)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) PutKeyPolicy(ctx context.Context, in *api.PutKeyPolicyRequest) (*api.PutKeyPolicyOutput, error) {
	if guardErr := checkPolicyName(in.PolicyName); guardErr != nil {
		return nil, guardErr
	}

	if guardErr := checkPolicy(common.Value(in.Policy)); guardErr != nil {
		return nil, guardErr
	}

	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if activeErr := k.active(scope); activeErr != nil {
			return activeErr
		}

		k.Policy = *in.Policy
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutKeyPolicyOutput{}, nil
}

func (s *Service) ListKeyPolicies(
	ctx context.Context,
	in *api.ListKeyPoliciesRequest,
) (*api.ListKeyPoliciesResponse, error) {
	err := s.view(ctx, in.KeyId, false, func(request.Scope, *key) error { return nil })
	if err != nil {
		return nil, err
	}

	names, next, err := page(
		[]string{defaultPolicyName}, func(name string) string { return name }, in.Marker, in.Limit, 100, 1000,
	)
	if err != nil {
		return nil, err
	}

	return &api.ListKeyPoliciesResponse{PolicyNames: names, NextMarker: next, Truncated: new(next != nil)}, nil
}

func (s *Service) GetKeyRotationStatus(
	ctx context.Context,
	in *api.GetKeyRotationStatusRequest,
) (*api.GetKeyRotationStatusResponse, error) {
	out := &api.GetKeyRotationStatusResponse{}
	err := s.view(ctx, in.KeyId, false, func(scope request.Scope, k *key) error {
		out.KeyId = new(keyARN(scope, k.ID))
		out.KeyRotationEnabled = new(k.RotationDays > 0)
		if k.RotationDays > 0 {
			out.RotationPeriodInDays = new(k.RotationDays)
			out.NextRotationDate = seconds(k.RotationStart + (time.Duration(k.RotationDays) * day).Milliseconds())
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// EnableKeyRotation records a rotation period of 90 to 2560 days (365 by
// default). TinyStack never rotates the key material.
func (s *Service) EnableKeyRotation(
	ctx context.Context,
	in *api.EnableKeyRotationRequest,
) (*api.EnableKeyRotationOutput, error) {
	days := int64(365)
	if in.RotationPeriodInDays != nil {
		days = *in.RotationPeriodInDays
		if days < 90 || days > 2560 {
			return nil, awserror.Validation("RotationPeriodInDays must be between 90 and 2560.")
		}
	}

	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		if k.spec() != symmetricDefault {
			return unsupported("%s does not support automatic key rotation.", keyARN(scope, k.ID))
		}

		if k.RotationDays == 0 {
			k.RotationStart = s.now().UnixMilli()
		}

		k.RotationDays = days
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.EnableKeyRotationOutput{}, nil
}

func (s *Service) DisableKeyRotation(
	ctx context.Context,
	in *api.DisableKeyRotationRequest,
) (*api.DisableKeyRotationOutput, error) {
	err := s.change(ctx, in.KeyId, func(scope request.Scope, k *key) error {
		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		k.RotationDays, k.RotationStart = 0, 0
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DisableKeyRotationOutput{}, nil
}
