package ec2

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/md5" // #nosec G501 -- EC2 fingerprints imported RSA keys with MD5.
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- EC2 fingerprints created RSA keys with SHA-1.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindKeyPair    = "key-pair"
	keyTypeRSA     = "rsa"
	keyTypeED25519 = "ed25519"
	rsaKeyBits     = 2048
	maxKeyName     = 255
)

// keyPair is a key pair's public half; EC2 never keeps a private key.
type keyPair struct {
	CreatedAt   time.Time
	Tags        map[string]string `json:",omitempty"`
	ID          string
	Name        string
	Type        string
	Fingerprint string
	PublicKey   string
}

func keyPairNotFound(name string) error {
	return awserror.New("InvalidKeyPair.NotFound", fmt.Sprintf("The key pair '%s' does not exist", name))
}

func keyPairIDNotFound(id string) error { return notFound("InvalidKeyPair.NotFound", "key pair", id) }

func keyPairInfo(k *keyPair, withPublicKey bool) api.KeyPairInfo {
	out := api.KeyPairInfo{
		KeyPairId: new(k.ID), KeyName: new(k.Name), KeyType: new(k.Type), KeyFingerprint: new(k.Fingerprint),
		CreateTime: new(k.CreatedAt), Tags: tagList(k.Tags),
	}
	if withPublicKey {
		out.PublicKey = new(k.PublicKey)
	}

	return out
}

func keyPairValues(k *keyPair) values {
	return values{
		"key-pair-id": {k.ID}, "key-name": {k.Name}, "fingerprint": {k.Fingerprint}, "key-type": {k.Type},
	}.withTags(k.Tags)
}

// colonHex writes a digest as colon-separated hex pairs.
func colonHex(sum []byte) string {
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = hex.EncodeToString([]byte{b})
	}

	return strings.Join(pairs, ":")
}

// ed25519Fingerprint is the base64 SHA-256 digest OpenSSH shows for a key.
func ed25519Fingerprint(pub ssh.PublicKey) string {
	sum := sha256.Sum256(pub.Marshal())
	return base64.StdEncoding.EncodeToString(sum[:])
}

func checkKeyName(name string) error {
	if name == "" {
		return awserror.New("MissingParameter", "The request must contain the parameter KeyName")
	}

	if len(name) > maxKeyName {
		return invalidParameter("Key pair names must be at most 255 characters")
	}

	return nil
}

// addKeyPair stores a new key pair unless its name is taken.
func (s *Service) addKeyPair(ctx context.Context, dry *bool, k *keyPair) error {
	return s.update(ctx, dry, func(tx storage.WriteTx, scope request.Scope) error {
		pairs, err := list[keyPair](tx, scope, kindKeyPair)
		if err != nil {
			return err
		}

		if slices.ContainsFunc(pairs, func(o *keyPair) bool { return o.Name == k.Name }) {
			return awserror.New("InvalidKeyPair.Duplicate", fmt.Sprintf("The keypair '%s' already exists.", k.Name))
		}

		return write(tx, key(scope, kindKeyPair, k.ID), k)
	})
}

// newKey generates a private key of keyType, returning its PEM encoding,
// public key, and EC2 fingerprint: the SHA-1 digest of an RSA key's PKCS#8
// encoding, or an ED25519 key's OpenSSH SHA-256 digest.
func newKey(keyType string) (material string, pub ssh.PublicKey, fingerprint string, err error) {
	var signer crypto.Signer
	var block *pem.Block
	switch keyType {
	case keyTypeRSA:
		rsaKey, genErr := rsa.GenerateKey(rand.Reader, rsaKeyBits)
		if genErr != nil {
			return "", nil, "", genErr
		}

		der, marshalErr := x509.MarshalPKCS8PrivateKey(rsaKey)
		if marshalErr != nil {
			return "", nil, "", marshalErr
		}

		sum := sha1.Sum(der) // #nosec G401 -- the fingerprint EC2 reports.
		signer, fingerprint = rsaKey, colonHex(sum[:])
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}
	case keyTypeED25519:
		_, edKey, genErr := ed25519.GenerateKey(rand.Reader)
		if genErr != nil {
			return "", nil, "", genErr
		}

		if block, err = ssh.MarshalPrivateKey(edKey, ""); err != nil {
			return "", nil, "", err
		}

		signer = edKey
	default:
		return "", nil, "", invalidParameter(fmt.Sprintf("Value (%s) for parameter keyType is invalid.", keyType))
	}

	if pub, err = ssh.NewPublicKey(signer.Public()); err != nil {
		return "", nil, "", err
	}

	if keyType == keyTypeED25519 {
		fingerprint = ed25519Fingerprint(pub)
	}

	return strings.TrimSpace(string(pem.EncodeToMemory(block))), pub, fingerprint, nil
}

// authorizedKey is a public key as EC2 reports it: in OpenSSH format,
// followed by the key pair's name.
func authorizedKey(pub ssh.PublicKey, name string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + name
}

func (s *Service) CreateKeyPair(ctx context.Context, in *api.CreateKeyPairRequest) (*api.KeyPair, error) {
	name, keyType := value(in.KeyName), value(in.KeyType)
	if err := checkKeyName(name); err != nil {
		return nil, err
	}

	if format := value(in.KeyFormat); format != "" && format != "pem" {
		return nil, invalidParameter("TinyStack supports only the pem key format")
	}

	if keyType == "" {
		keyType = keyTypeRSA
	}

	tags, err := specTags(in.TagSpecifications, "key-pair")
	if err != nil {
		return nil, err
	}

	material, pub, fingerprint, err := newKey(keyType)
	if err != nil {
		return nil, err
	}

	k := &keyPair{
		ID: newID("key"), Name: name, Type: keyType, Fingerprint: fingerprint, PublicKey: authorizedKey(pub, name),
		CreatedAt: s.now().UTC(), Tags: tags,
	}
	if addErr := s.addKeyPair(ctx, in.DryRun, k); addErr != nil {
		return nil, addErr
	}

	return &api.KeyPair{
		KeyPairId: new(k.ID), KeyName: new(name), KeyFingerprint: new(fingerprint), KeyMaterial: new(material),
		Tags: tagList(tags),
	}, nil
}

// ImportKeyPair stores an RSA or ED25519 public key given in OpenSSH format.
// An RSA key's fingerprint is the MD5 digest of its public key's DER
// encoding.
func (s *Service) ImportKeyPair(
	ctx context.Context,
	in *api.ImportKeyPairRequest,
) (*api.ImportKeyPairResult, error) {
	name := value(in.KeyName)
	if err := checkKeyName(name); err != nil {
		return nil, err
	}

	invalid := awserror.New("InvalidKey.Format", "Key is not in valid OpenSSH public key format")
	pub, _, _, _, err := ssh.ParseAuthorizedKey(in.PublicKeyMaterial)
	if err != nil {
		return nil, invalid
	}

	k := &keyPair{ID: newID("key"), Name: name, PublicKey: authorizedKey(pub, name), CreatedAt: s.now().UTC()}
	switch pub.Type() {
	case ssh.KeyAlgoRSA:
		cryptoKey, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return nil, invalid
		}

		der, marshalErr := x509.MarshalPKIXPublicKey(cryptoKey.CryptoPublicKey())
		if marshalErr != nil {
			return nil, invalid
		}

		sum := md5.Sum(der) // #nosec G401 -- the fingerprint EC2 reports.
		k.Type, k.Fingerprint = keyTypeRSA, colonHex(sum[:])
	case ssh.KeyAlgoED25519:
		k.Type, k.Fingerprint = keyTypeED25519, ed25519Fingerprint(pub)
	default:
		return nil, invalid
	}

	if k.Tags, err = specTags(in.TagSpecifications, "key-pair"); err != nil {
		return nil, err
	}

	if addErr := s.addKeyPair(ctx, in.DryRun, k); addErr != nil {
		return nil, addErr
	}

	return &api.ImportKeyPairResult{
		KeyPairId: new(k.ID), KeyName: new(name), KeyFingerprint: new(k.Fingerprint), Tags: tagList(k.Tags),
	}, nil
}

func (s *Service) DescribeKeyPairs(
	ctx context.Context,
	in *api.DescribeKeyPairsRequest,
) (*api.DescribeKeyPairsResult, error) {
	match, err := compileFilters(in.Filters, keyPairValues(&keyPair{}).names()...)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeKeyPairsResult{KeyPairs: []api.KeyPairInfo{}}
	err = s.view(ctx, in.DryRun, func(tx storage.ReadTx, scope request.Scope) error {
		all, listErr := list[keyPair](tx, scope, kindKeyPair)
		if listErr != nil {
			return listErr
		}

		selected, selectErr := selectIDs(all, func(k *keyPair) string { return k.ID }, in.KeyPairIds, keyPairIDNotFound)
		if selectErr != nil {
			return selectErr
		}

		selected, selectErr = selectIDs(selected, func(k *keyPair) string { return k.Name }, in.KeyNames,
			keyPairNotFound)
		if selectErr != nil {
			return selectErr
		}

		for _, k := range selected {
			if match.match(keyPairValues(k)) {
				out.KeyPairs = append(out.KeyPairs, keyPairInfo(k, value(in.IncludePublicKey)))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteKeyPair deletes a key pair by name or ID; deleting one that does not
// exist succeeds.
func (s *Service) DeleteKeyPair(
	ctx context.Context,
	in *api.DeleteKeyPairRequest,
) (*api.DeleteKeyPairResult, error) {
	name, id := value(in.KeyName), value(in.KeyPairId)
	if name == "" && id == "" {
		return nil, awserror.New("MissingParameter", "The request must contain the parameter KeyName or KeyPairId")
	}

	out := &api.DeleteKeyPairResult{Return: new(true)}
	err := s.update(ctx, in.DryRun, func(tx storage.WriteTx, scope request.Scope) error {
		pairs, err := list[keyPair](tx, scope, kindKeyPair)
		if err != nil {
			return err
		}

		for _, k := range pairs {
			if (id == "" || k.ID == id) && (name == "" || k.Name == name) {
				out.KeyPairId = new(k.ID)
				tx.Delete(key(scope, kindKeyPair, k.ID))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
