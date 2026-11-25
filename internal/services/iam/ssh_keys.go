package iam

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindSSHKey        = "ssh-public-key"
	maxSSHKeysPerUser = 5
	encodingSSH       = "SSH"
	encodingPEM       = "PEM"
)

type sshPublicKey struct {
	UserName    string
	ID          string
	Fingerprint string
	SSHBody     string
	PEMBody     string
	Status      string
	UploadDate  int64
}

func parseSSHPublicKey(raw string) (sshBody, pemBody, fingerprint string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", "", awserror.New("InvalidPublicKeyException", "SSH public key body must not be empty.")
	}

	var pub ssh.PublicKey
	if strings.HasPrefix(trimmed, "-----BEGIN ") {
		block, _ := pem.Decode([]byte(trimmed))
		if block == nil {
			return "", "", "", awserror.New("InvalidPublicKeyException", "Invalid PEM public key.")
		}

		parsed, parseErr := x509.ParsePKIXPublicKey(block.Bytes)
		if parseErr != nil {
			return "", "", "", awserror.New("InvalidPublicKeyException", "Invalid PEM public key: "+parseErr.Error())
		}

		if pub, parseErr = ssh.NewPublicKey(parsed); parseErr != nil {
			return "", "", "", awserror.New("InvalidPublicKeyException", "Unsupported public key type.")
		}
	} else {
		var parseErr error
		pub, _, _, _, parseErr = ssh.ParseAuthorizedKey([]byte(trimmed))
		if parseErr != nil {
			return "", "", "", awserror.New("InvalidPublicKeyException", "Invalid SSH public key: "+parseErr.Error())
		}
	}

	cryptoPub, ok := pub.(ssh.CryptoPublicKey)
	if !ok {
		return "", "", "", awserror.New("InvalidPublicKeyException", "Unsupported SSH public key type.")
	}

	der, err := x509.MarshalPKIXPublicKey(cryptoPub.CryptoPublicKey())
	if err != nil {
		return "", "", "", awserror.New("InvalidPublicKeyException", "Unable to encode public key as PEM.")
	}

	sshBody = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	pemBody = strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	fingerprint = ssh.FingerprintLegacyMD5(pub)
	return sshBody, pemBody, fingerprint, nil
}

func sshKeysOf(tx storage.ReadTx, account, userName string) ([]*sshPublicKey, error) {
	all, err := list[sshPublicKey](tx, account, kindSSHKey)
	if err != nil {
		return nil, err
	}

	var out []*sshPublicKey
	for _, k := range all {
		if strings.EqualFold(k.UserName, userName) {
			out = append(out, k)
		}
	}

	return out, nil
}

func renameSSHKeys(tx storage.WriteTx, account, from, to string) error {
	keys, err := sshKeysOf(tx, account, from)
	if err != nil {
		return err
	}

	for _, k := range keys {
		k.UserName = to
		if writeErr := storage.Write(tx, key(account, kindSSHKey, k.ID), k); writeErr != nil {
			return writeErr
		}
	}

	return nil
}

func loadUserSSHKey(tx storage.ReadTx, account, userName string, id *string) (*sshPublicKey, string, error) {
	if err := required("SSHPublicKeyId", id); err != nil {
		return nil, "", err
	}

	kk := key(account, kindSSHKey, *id)
	k, ok, err := storage.Read[sshPublicKey](tx, kk)
	if err != nil {
		return nil, "", err
	}

	if !ok || !strings.EqualFold(k.UserName, userName) {
		return nil, "", noSuchEntity("The SSH public key with id %s cannot be found.", *id)
	}

	return k, kk, nil
}

func (s *Service) UploadSSHPublicKey(
	ctx context.Context,
	in *api.UploadSSHPublicKeyRequest,
) (*api.UploadSSHPublicKeyResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := required("SSHPublicKeyBody", in.SSHPublicKeyBody); guardErr != nil {
		return nil, guardErr
	}

	sshBody, pemBody, fingerprint, err := parseSSHPublicKey(*in.SSHPublicKeyBody)
	if err != nil {
		return nil, err
	}

	var stored *sshPublicKey
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		existing, listErr := sshKeysOf(tx, m.AccountID, u.Name)
		if listErr != nil {
			return listErr
		}

		for _, k := range existing {
			if k.Fingerprint == fingerprint {
				return awserror.New(
					"DuplicateSSHPublicKeyException",
					"The SSH public key already exists for this user.",
				)
			}
		}

		if len(existing) >= maxSSHKeysPerUser {
			return limitExceeded("Cannot exceed quota for SSHPublicKeysPerUser: %d", maxSSHKeysPerUser)
		}

		stored = &sshPublicKey{
			UserName:    u.Name,
			ID:          newID("APKA"),
			Fingerprint: fingerprint,
			SSHBody:     sshBody,
			PEMBody:     pemBody,
			Status:      keyActive,
			UploadDate:  s.now().UnixMilli(),
		}
		return storage.Write(tx, key(m.AccountID, kindSSHKey, stored.ID), stored)
	})
	if err != nil {
		return nil, err
	}

	return &api.UploadSSHPublicKeyResponse{
		SSHPublicKey: &api.SSHPublicKey{
			UserName:         new(stored.UserName),
			SSHPublicKeyId:   new(stored.ID),
			Fingerprint:      new(stored.Fingerprint),
			SSHPublicKeyBody: new(stored.SSHBody),
			Status:           new(stored.Status),
			UploadDate:       millis(stored.UploadDate),
		},
	}, nil
}

func (s *Service) GetSSHPublicKey(
	ctx context.Context,
	in *api.GetSSHPublicKeyRequest,
) (*api.GetSSHPublicKeyResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	encoding := common.Value(in.Encoding)
	if encoding != encodingSSH && encoding != encodingPEM {
		return nil, awserror.New("UnrecognizedPublicKeyEncodingException", "Encoding must be SSH or PEM.")
	}

	var stored *sshPublicKey
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		var keyErr error
		stored, _, keyErr = loadUserSSHKey(tx, m.AccountID, u.Name, in.SSHPublicKeyId)
		return keyErr
	})
	if err != nil {
		return nil, err
	}

	body := stored.SSHBody
	if encoding == encodingPEM {
		body = stored.PEMBody
	}

	return &api.GetSSHPublicKeyResponse{
		SSHPublicKey: &api.SSHPublicKey{
			UserName:         new(stored.UserName),
			SSHPublicKeyId:   new(stored.ID),
			Fingerprint:      new(stored.Fingerprint),
			SSHPublicKeyBody: new(body),
			Status:           new(stored.Status),
			UploadDate:       millis(stored.UploadDate),
		},
	}, nil
}

func (s *Service) UpdateSSHPublicKey(
	ctx context.Context,
	in *api.UpdateSSHPublicKeyRequest,
) (*api.UpdateSSHPublicKeyOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	status := common.Value(in.Status)
	if status != keyActive && status != keyInactive {
		return nil, invalidInput("Status must be Active or Inactive")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		stored, kk, keyErr := loadUserSSHKey(tx, m.AccountID, u.Name, in.SSHPublicKeyId)
		if keyErr != nil {
			return keyErr
		}

		stored.Status = status
		return storage.Write(tx, kk, stored)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateSSHPublicKeyOutput{}, nil
}

func (s *Service) DeleteSSHPublicKey(
	ctx context.Context,
	in *api.DeleteSSHPublicKeyRequest,
) (*api.DeleteSSHPublicKeyOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		_, kk, keyErr := loadUserSSHKey(tx, m.AccountID, u.Name, in.SSHPublicKeyId)
		if keyErr != nil {
			return keyErr
		}

		tx.Delete(kk)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteSSHPublicKeyOutput{}, nil
}

func (s *Service) ListSSHPublicKeys(
	ctx context.Context,
	in *api.ListSSHPublicKeysRequest,
) (*api.ListSSHPublicKeysResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var keys []api.SSHPublicKeyMetadata
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		u, _, userErr := keyUser(tx, &m, in.UserName)
		if userErr != nil {
			return userErr
		}

		listKeys, listErr := sshKeysOf(tx, m.AccountID, u.Name)
		if listErr != nil {
			return listErr
		}

		for _, k := range listKeys {
			keys = append(keys, api.SSHPublicKeyMetadata{
				UserName:       new(u.Name),
				SSHPublicKeyId: new(k.ID),
				Status:         new(k.Status),
				UploadDate:     millis(k.UploadDate),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(k api.SSHPublicKeyMetadata) string { return *k.SSHPublicKeyId }
	slices.SortFunc(keys, func(a, b api.SSHPublicKeyMetadata) int { return strings.Compare(id(a), id(b)) })
	selected, truncated, next, err := page(keys, id, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.SSHPublicKeyMetadata{}
	}

	return &api.ListSSHPublicKeysResponse{SSHPublicKeys: selected, IsTruncated: truncated, Marker: next}, nil
}
