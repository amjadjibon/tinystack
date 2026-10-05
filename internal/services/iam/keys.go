package iam

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// accessKeyPrefix holds every access key by ID. Access key IDs are global,
	// so the gateway can resolve any request's key without knowing its account.
	accessKeyPrefix = "aws/iam/access-key/"
	keyActive       = "Active"
	keyInactive     = "Inactive"
	maxAccessKeys   = 2
)

// accessKey is a user's long-term credential.
type accessKey struct {
	Account   string
	UserName  string
	Secret    string
	Status    string
	CreatedAt int64
}

func noSuchKey(id string) error {
	return noSuchEntity("The Access Key with id %s cannot be found.", id)
}

// keyUser resolves the user an access key request names: UserName, or the IAM
// user that signed the request. TinyStack does not issue root access keys.
func keyUser(tx storage.ReadTx, m *request.Metadata, name *string) (*user, string, error) {
	if name == nil {
		caller, isUser, err := callerUser(tx, m)
		if err != nil {
			return nil, "", err
		}

		if !isUser {
			return nil, "", validation("TinyStack does not manage root access keys; specify a UserName.")
		}

		name = &caller
	}

	return loadUser(tx, m.AccountID, name)
}

// userKey returns the access key id of user u.
func userKey(tx storage.ReadTx, u *user, id *string) (*accessKey, error) {
	if err := required("accessKeyId", id); err != nil {
		return nil, err
	}

	if !slices.Contains(u.Keys, *id) {
		return nil, noSuchKey(*id)
	}

	k, ok, err := storage.Read[accessKey](tx, accessKeyPrefix+*id)
	if err == nil && !ok {
		err = noSuchKey(*id)
	}

	return k, err
}

// renameKeys records a user's new name in its access keys.
func renameKeys(tx storage.WriteTx, u *user, name string) error {
	for _, id := range u.Keys {
		k, ok, err := storage.Read[accessKey](tx, accessKeyPrefix+id)
		if err != nil || !ok {
			return err
		}

		k.UserName = name
		if writeErr := storage.Write(tx, accessKeyPrefix+id, k); writeErr != nil {
			return writeErr
		}
	}

	return nil
}

// CreateAccessKey issues an active access key, at most two per user. Only
// this response carries the secret.
func (s *Service) CreateAccessKey(
	ctx context.Context,
	in *api.CreateAccessKeyRequest,
) (*api.CreateAccessKeyResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	secret := make([]byte, 30)
	_, _ = rand.Read(secret)
	id := newID("AKIA")
	k := &accessKey{
		Account:   m.AccountID,
		Secret:    base64.StdEncoding.EncodeToString(secret),
		Status:    keyActive,
		CreatedAt: s.now().UnixMilli(),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, uk, loadErr := keyUser(tx, &m, in.UserName)
		if loadErr != nil {
			return loadErr
		}

		if len(u.Keys) >= maxAccessKeys {
			return limitExceeded("Cannot exceed quota for AccessKeysPerUser: %d", maxAccessKeys)
		}

		k.UserName = u.Name
		u.Keys = append(u.Keys, id)
		if writeErr := storage.Write(tx, accessKeyPrefix+id, k); writeErr != nil {
			return writeErr
		}

		return storage.Write(tx, uk, u)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAccessKeyResponse{AccessKey: &api.AccessKey{
		UserName:        new(k.UserName),
		AccessKeyId:     new(id),
		Status:          new(k.Status),
		SecretAccessKey: new(k.Secret),
		CreateDate:      millis(k.CreatedAt),
	}}, nil
}

func (s *Service) ListAccessKeys(
	ctx context.Context,
	in *api.ListAccessKeysRequest,
) (*api.ListAccessKeysResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var keys []api.AccessKeyMetadata
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		u, _, loadErr := keyUser(tx, &m, in.UserName)
		if loadErr != nil {
			return loadErr
		}

		for _, id := range u.Keys {
			k, _, readErr := storage.Read[accessKey](tx, accessKeyPrefix+id)
			if readErr != nil {
				return readErr
			}

			keys = append(keys, api.AccessKeyMetadata{
				UserName: new(u.Name), AccessKeyId: new(id), Status: new(k.Status), CreateDate: millis(k.CreatedAt),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Keys are listed by ID, which is also the page marker.
	id := func(k api.AccessKeyMetadata) string { return *k.AccessKeyId }
	slices.SortFunc(keys, func(a, b api.AccessKeyMetadata) int { return strings.Compare(id(a), id(b)) })
	out := &api.ListAccessKeysResponse{}
	if out.AccessKeyMetadata, out.IsTruncated, out.Marker, err = page(keys, id, in.Marker, in.MaxItems); err != nil {
		return nil, err
	}

	if out.AccessKeyMetadata == nil {
		out.AccessKeyMetadata = []api.AccessKeyMetadata{}
	}

	return out, nil
}

// UpdateAccessKey activates or deactivates a key; an inactive key cannot sign
// requests.
func (s *Service) UpdateAccessKey(
	ctx context.Context,
	in *api.UpdateAccessKeyRequest,
) (*api.UpdateAccessKeyOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	status := common.Value(in.Status)
	if status != keyActive && status != keyInactive {
		return nil, validation("Status must be Active or Inactive")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, loadErr := keyUser(tx, &m, in.UserName)
		if loadErr != nil {
			return loadErr
		}

		k, keyErr := userKey(tx, u, in.AccessKeyId)
		if keyErr != nil {
			return keyErr
		}

		k.Status = status
		return storage.Write(tx, accessKeyPrefix+*in.AccessKeyId, k)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateAccessKeyOutput{}, nil
}

func (s *Service) DeleteAccessKey(
	ctx context.Context,
	in *api.DeleteAccessKeyRequest,
) (*api.DeleteAccessKeyOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, uk, loadErr := keyUser(tx, &m, in.UserName)
		if loadErr != nil {
			return loadErr
		}

		if _, keyErr := userKey(tx, u, in.AccessKeyId); keyErr != nil {
			return keyErr
		}

		tx.Delete(accessKeyPrefix + *in.AccessKeyId)
		u.Keys = slices.DeleteFunc(u.Keys, func(id string) bool { return id == *in.AccessKeyId })
		return storage.Write(tx, uk, u)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAccessKeyOutput{}, nil
}

// GetAccessKeyLastUsed names the key's user. TinyStack does not record when a
// key is used, so it reports the key as never used, as IAM does for a new key.
func (s *Service) GetAccessKeyLastUsed(
	ctx context.Context,
	in *api.GetAccessKeyLastUsedRequest,
) (*api.GetAccessKeyLastUsedResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := required("accessKeyId", in.AccessKeyId); guardErr != nil {
		return nil, guardErr
	}

	var name string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		k, ok, readErr := storage.Read[accessKey](tx, accessKeyPrefix+*in.AccessKeyId)
		if readErr != nil {
			return readErr
		}

		if !ok || k.Account != m.AccountID {
			return noSuchKey(*in.AccessKeyId)
		}

		name = k.UserName
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetAccessKeyLastUsedResponse{
		UserName:          &name,
		AccessKeyLastUsed: &api.AccessKeyLastUsed{ServiceName: new("N/A"), Region: new("N/A")},
	}, nil
}
