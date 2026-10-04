package kms

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Grants are "<account>/<region>/kms/grant/<key ID>/<grant ID>". Like key
// policies, they are stored, never evaluated, and a grant token is the key ID
// and grant ID it stands for.

const (
	opCreateGrant = "CreateGrant"
	opRetireGrant = "RetireGrant"
	opDescribeKey = "DescribeKey"
)

// grantOperations are the operations a grant can allow on each kind of key.
var grantOperations = map[string][]string{
	symmetricDefault: {
		"Decrypt", "Encrypt", "GenerateDataKey", "GenerateDataKeyWithoutPlaintext", "ReEncryptFrom", "ReEncryptTo",
		"GenerateDataKeyPair", "GenerateDataKeyPairWithoutPlaintext",
	},
	encryptDecrypt:    {"Decrypt", "Encrypt", "ReEncryptFrom", "ReEncryptTo", "GetPublicKey"},
	signVerify:        {"Sign", "Verify", "GetPublicKey"},
	generateVerifyMAC: {"GenerateMac", "VerifyMac"},
}

// grant is a grant's record. Created is Unix milliseconds.
type grant struct {
	Constraints *api.GrantConstraints `json:",omitempty"`
	ID          string
	Name        string `json:",omitempty"`
	Grantee     string
	Retiring    string `json:",omitempty"`
	Operations  []string
	Created     int64
}

func grantPrefix(scope request.Scope, keyID string) string {
	return prefix(scope, "grant") + keyID + "/"
}

func grantToken(keyID, grantID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(keyID + "/" + grantID))
}

func invalidGrantID() error {
	return awserror.New("InvalidGrantIdException", "Grant ID not valid.")
}

// allowedOperations returns the operations a grant may allow on a key.
func (k *key) allowedOperations() []string {
	kind := k.usage()
	if k.spec() == symmetricDefault {
		kind = symmetricDefault
	}

	return append([]string{opCreateGrant, opRetireGrant, opDescribeKey}, grantOperations[kind]...)
}

func checkGrant(k *key, in *api.CreateGrantRequest) error {
	switch {
	case value(in.GranteePrincipal) == "" || len(*in.GranteePrincipal) > 256:
		return awserror.Validation("GranteePrincipal must be 1 to 256 characters.")
	case len(value(in.RetiringPrincipal)) > 256:
		return awserror.Validation("RetiringPrincipal must be at most 256 characters.")
	case len(value(in.Name)) > 256:
		return awserror.Validation("Name must be at most 256 characters.")
	case len(in.Operations) == 0:
		return awserror.Validation("Operations must name at least one operation.")
	case in.Constraints != nil && len(in.Constraints.EncryptionContextEquals) > 0 &&
		len(in.Constraints.EncryptionContextSubset) > 0:
		return awserror.Validation("Constraints cannot have both EncryptionContextEquals and " +
			"EncryptionContextSubset.")
	}

	allowed := k.allowedOperations()
	for _, op := range in.Operations {
		if !slices.Contains(allowed, op) {
			return awserror.Validation("Grant operation %s is not valid for a %s key with the %s key usage.", op,
				k.spec(), k.usage())
		}
	}

	return nil
}

// same reports whether a grant has a CreateGrant request's parameters, which
// makes a named CreateGrant return it again.
func (g *grant) same(in *api.CreateGrantRequest) bool {
	var equals, subset map[string]string
	if in.Constraints != nil {
		equals, subset = in.Constraints.EncryptionContextEquals, in.Constraints.EncryptionContextSubset
	}

	var gEquals, gSubset map[string]string
	if g.Constraints != nil {
		gEquals, gSubset = g.Constraints.EncryptionContextEquals, g.Constraints.EncryptionContextSubset
	}

	return g.Name == value(in.Name) && g.Grantee == value(in.GranteePrincipal) &&
		g.Retiring == value(in.RetiringPrincipal) && slices.Equal(g.Operations, in.Operations) &&
		maps.Equal(equals, gEquals) && maps.Equal(subset, gSubset)
}

// CreateGrant records a grant on a key. A named grant with the same
// parameters as an existing one returns that one.
func (s *Service) CreateGrant(ctx context.Context, in *api.CreateGrantRequest) (*api.CreateGrantResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.CreateGrantResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, loadErr := s.load(tx, scope, in.KeyId, false)
		if loadErr != nil {
			return loadErr
		}

		if usableErr := k.usable(scope); usableErr != nil {
			return usableErr
		}

		if checkErr := checkGrant(k, in); checkErr != nil {
			return checkErr
		}

		if dryErr := dryRun(in.DryRun); dryErr != nil {
			return dryErr
		}

		if in.Name != nil {
			for _, gk := range tx.Keys(grantPrefix(scope, k.ID)) {
				g, _, readErr := read[grant](tx, gk)
				if readErr != nil {
					return readErr
				}

				if g.same(in) {
					out.GrantId, out.GrantToken = new(g.ID), new(grantToken(k.ID, g.ID))
					return nil
				}
			}
		}

		id := make([]byte, 32)
		_, _ = rand.Read(id)
		g := &grant{
			Constraints: in.Constraints, ID: hex.EncodeToString(id), Name: value(in.Name),
			Grantee: value(in.GranteePrincipal), Retiring: value(in.RetiringPrincipal), Operations: in.Operations,
			Created: s.now().UnixMilli(),
		}
		out.GrantId, out.GrantToken = new(g.ID), new(grantToken(k.ID, g.ID))
		return write(tx, grantPrefix(scope, k.ID)+g.ID, g)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// entry renders a grant as ListGrants does.
func (g *grant) entry(scope request.Scope, keyID string) api.GrantListEntry {
	e := api.GrantListEntry{
		KeyId: new(keyARN(scope, keyID)), GrantId: new(g.ID), CreationDate: seconds(g.Created),
		GranteePrincipal: new(g.Grantee), IssuingAccount: new("arn:aws:iam::" + scope.AccountID + ":root"),
		Operations: g.Operations, Constraints: g.Constraints,
	}
	if g.Name != "" {
		e.Name = new(g.Name)
	}

	if g.Retiring != "" {
		e.RetiringPrincipal = new(g.Retiring)
	}

	return e
}

// ListGrants lists a key's grants in grant ID order, optionally one grant or
// one grantee's.
func (s *Service) ListGrants(ctx context.Context, in *api.ListGrantsRequest) (*api.ListGrantsResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var entries []api.GrantListEntry
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		k, loadErr := s.load(tx, scope, in.KeyId, false)
		if loadErr != nil {
			return loadErr
		}

		for _, gk := range tx.Keys(grantPrefix(scope, k.ID)) {
			g, _, readErr := read[grant](tx, gk)
			if readErr != nil {
				return readErr
			}

			if (in.GrantId == nil || *in.GrantId == g.ID) &&
				(in.GranteePrincipal == nil || *in.GranteePrincipal == g.Grantee) {
				entries = append(entries, g.entry(scope, k.ID))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return grantPage(entries, in.Marker, in.Limit)
}

// ListRetirableGrants lists the grants a principal may retire, on every key
// in the caller's account and region.
func (s *Service) ListRetirableGrants(
	ctx context.Context,
	in *api.ListRetirableGrantsRequest,
) (*api.ListGrantsResponse, error) {
	scope, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if value(in.RetiringPrincipal) == "" {
		return nil, awserror.Validation("RetiringPrincipal is required.")
	}

	var entries []api.GrantListEntry
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, gk := range tx.Keys(prefix(scope, "grant")) {
			g, _, readErr := read[grant](tx, gk)
			if readErr != nil {
				return readErr
			}

			if g.Retiring == *in.RetiringPrincipal {
				keyID, _, _ := strings.Cut(strings.TrimPrefix(gk, prefix(scope, "grant")), "/")
				entries = append(entries, g.entry(scope, keyID))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return grantPage(entries, in.Marker, in.Limit)
}

func grantPage(entries []api.GrantListEntry, marker *string, limit *int64) (*api.ListGrantsResponse, error) {
	entries, next, err := page(entries, func(e api.GrantListEntry) string { return *e.KeyId + "/" + *e.GrantId },
		marker, limit, 50, 100)
	if err != nil {
		return nil, err
	}

	out := &api.ListGrantsResponse{Grants: []api.GrantListEntry{}, NextMarker: next, Truncated: new(next != nil)}
	out.Grants = append(out.Grants, entries...)
	return out, nil
}

// RetireGrant deletes a grant given by its token, or by its key and ID.
func (s *Service) RetireGrant(ctx context.Context, in *api.RetireGrantRequest) (*api.RetireGrantOutput, error) {
	keyRef, grantID := in.KeyId, value(in.GrantId)
	if in.GrantToken != nil {
		decoded, err := base64.RawURLEncoding.DecodeString(*in.GrantToken)
		id, gid, ok := strings.Cut(string(decoded), "/")
		if err != nil || !ok || !keyID.MatchString(id) {
			return nil, awserror.New("InvalidGrantTokenException", "The grant token is not valid.")
		}

		keyRef, grantID = &id, gid
	} else if keyRef == nil || grantID == "" {
		return nil, awserror.Validation("Give either a GrantToken, or a KeyId and a GrantId.")
	}

	if err := s.deleteGrant(ctx, keyRef, grantID, in.DryRun); err != nil {
		return nil, err
	}

	return &api.RetireGrantOutput{}, nil
}

// RevokeGrant deletes a key's grant.
func (s *Service) RevokeGrant(ctx context.Context, in *api.RevokeGrantRequest) (*api.RevokeGrantOutput, error) {
	if err := s.deleteGrant(ctx, in.KeyId, value(in.GrantId), in.DryRun); err != nil {
		return nil, err
	}

	return &api.RevokeGrantOutput{}, nil
}

func (s *Service) deleteGrant(ctx context.Context, keyRef *string, grantID string, dry *bool) error {
	scope, err := identity(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		k, loadErr := s.load(tx, scope, keyRef, false)
		if loadErr != nil {
			return loadErr
		}

		gk := grantPrefix(scope, k.ID) + grantID
		if _, found := tx.Get(gk); !found || grantID == "" {
			return invalidGrantID()
		}

		if dryErr := dryRun(dry); dryErr != nil {
			return dryErr
		}

		tx.Delete(gk)
		return nil
	})
}

// ReEncrypt decrypts a ciphertext with its key and encrypts the plaintext
// with another, as Decrypt and Encrypt do, without returning the plaintext.
func (s *Service) ReEncrypt(ctx context.Context, in *api.ReEncryptRequest) (*api.ReEncryptResponse, error) {
	opened, err := s.Decrypt(ctx, &api.DecryptRequest{
		CiphertextBlob: in.CiphertextBlob, EncryptionContext: in.SourceEncryptionContext, KeyId: in.SourceKeyId,
		EncryptionAlgorithm: in.SourceEncryptionAlgorithm,
	})
	if err != nil {
		return nil, err
	}

	sealed, err := s.Encrypt(ctx, &api.EncryptRequest{
		KeyId: in.DestinationKeyId, Plaintext: opened.Plaintext, EncryptionContext: in.DestinationEncryptionContext,
		EncryptionAlgorithm: in.DestinationEncryptionAlgorithm, DryRun: in.DryRun,
	})
	if err != nil {
		return nil, err
	}

	return &api.ReEncryptResponse{
		CiphertextBlob:                 sealed.CiphertextBlob,
		SourceKeyId:                    opened.KeyId,
		KeyId:                          sealed.KeyId,
		SourceEncryptionAlgorithm:      opened.EncryptionAlgorithm,
		DestinationEncryptionAlgorithm: sealed.EncryptionAlgorithm,
	}, nil
}
