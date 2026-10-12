package transfer

import (
	"context"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// publicKey accepts one OpenSSH public-key line, without authorized_keys options.
// Parsing verifies the encoded algorithm, curve/point or key structure, not just a prefix.
func publicKey(body string) (string, error) {
	if body == "" || len(body) > 2048 {
		return "", invalid("SshPublicKeyBody requires 1-2048 bytes")
	}
	line := strings.TrimSpace(body)
	if strings.ContainsAny(line, "\r\n") {
		return "", invalid("Only one OpenSSH public-key line is supported")
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", invalid("Invalid SSH public key")
	}
	key, _, options, rest, e := ssh.ParseAuthorizedKey([]byte(line))
	if e != nil || len(options) != 0 || strings.TrimSpace(string(rest)) != "" || key.Type() != fields[0] {
		return "", invalid("Invalid SSH public key or encoded key type mismatch")
	}
	switch key.Type() {
	case ssh.KeyAlgoRSA, ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	default:
		return "", invalid("Only RSA, ECDSA and ED25519 public keys are supported")
	}
	// Store canonical key bytes; comments are not part of key identity.
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), nil
}

func newPublicKey(body string) api.SshPublicKey {
	return api.SshPublicKey{
		SshPublicKeyId:   new(identifier("key-")),
		SshPublicKeyBody: new(body),
		DateImported:     new(float64(time.Now().UTC().UnixMicro()) / 1e6),
	}
}

func (s *Service) ImportSshPublicKey(
	ctx context.Context,
	in *api.ImportSshPublicKeyRequest,
) (*api.ImportSshPublicKeyResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("ImportSshPublicKey input is required")
	}
	in = common.Clone(in)
	body, e := publicKey(common.Value(in.SshPublicKeyBody))
	if e != nil {
		return nil, e
	}
	key := newPublicKey(body)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readUser(tx, scope, common.Value(in.ServerId), common.Value(in.UserName))
		if err != nil {
			return err
		}
		for _, k := range r.User.SshPublicKeys {
			if *k.SshPublicKeyBody == body {
				return duplicate()
			}
		}
		if len(r.User.SshPublicKeys) >= 50 {
			return invalid("Local limit is 50 SSH public keys per user")
		}
		r.User.SshPublicKeys = append(r.User.SshPublicKeys, key)
		r.Revision = revision()
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.ImportSshPublicKeyResponse{
		ServerId:       in.ServerId,
		UserName:       in.UserName,
		SshPublicKeyId: common.Clone(key.SshPublicKeyId),
	}, nil
}

func (s *Service) DeleteSshPublicKey(
	ctx context.Context,
	in *api.DeleteSshPublicKeyRequest,
) (*api.DeleteSshPublicKeyOutput, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("DeleteSshPublicKey input is required")
	}
	in = common.Clone(in)
	if !keyPattern.MatchString(common.Value(in.SshPublicKeyId)) {
		return nil, invalid("SshPublicKeyId must be key- followed by 17 lowercase hexadecimal digits")
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readUser(tx, scope, common.Value(in.ServerId), common.Value(in.UserName))
		if err != nil {
			return err
		}
		for i, k := range r.User.SshPublicKeys {
			if *k.SshPublicKeyId == *in.SshPublicKeyId {
				r.User.SshPublicKeys = append(r.User.SshPublicKeys[:i], r.User.SshPublicKeys[i+1:]...)
				r.Revision = revision()
				return save(tx, scope, r)
			}
		}
		return missing()
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteSshPublicKeyOutput{}, nil
}
