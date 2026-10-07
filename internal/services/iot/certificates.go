package iot

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/pki"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	statusActive     = "ACTIVE"
	statusInactive   = "INACTIVE"
	statusRevoked    = "REVOKED"
	modeDefault      = "DEFAULT"
	modeSNIOnly      = "SNI_ONLY"
	issuedSubject    = "AWS IoT Certificate"
	certificateIndex = "aws/" + service + "/" + kindCertificate + "/"
)

// issuedUntil is when certificates that IoT issues expire, as on AWS.
var issuedUntil = time.Date(2049, 12, 31, 23, 59, 59, 0, time.UTC)

// certificate is a device certificate's record. CA is the ID of the
// registered CA certificate that signed it, if any.
type certificate struct {
	ID         string
	PEM        string
	Status     string
	CA         string `json:",omitempty"`
	Mode       string
	Owner      string
	Generation string
	Version    int64
	Created    float64
	Modified   float64
	NotBefore  float64
	NotAfter   float64
}

// certificateIndexKey is the key of the partition-wide index entry that
// holds the account and Region in which a certificate is active, so MQTT over
// TLS can find a client's scope from its certificate.
func certificateIndexKey(id string) string {
	return certificateIndex + id
}

// indexCertificate keeps a certificate's index entry in step with its status.
// A certificate active in two accounts keeps the first one's entry (a server
// name can pick the other), and only its own account's change removes it.
func indexCertificate(tx storage.WriteTx, scope request.Scope, c *certificate) error {
	key := certificateIndexKey(c.ID)
	held, found, err := storage.Read[request.Scope](tx, key)
	switch {
	case err != nil:
		return err
	case c.Status == statusActive && !found:
		return storage.Write(tx, key, scope)
	case c.Status != statusActive && found && *held == scope:
		tx.Delete(key)
	}

	return nil
}

func loadCertificate(tx storage.ReadTx, scope request.Scope, id string) (*certificate, error) {
	c, ok, err := storage.Read[certificate](tx, scope.Key(service, kindCertificate, id))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Certificate %s cannot be found.", id))
	}

	return c, nil
}

func validation(format string, args ...any) error {
	return awserror.New("CertificateValidationException", "The certificate is invalid: "+fmt.Sprintf(format, args...))
}

// certificateID is a certificate's ID: the hex SHA-256 of its DER bytes.
func certificateID(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// parseCertificate reads the one PEM certificate in data.
func parseCertificate(data *string) (*x509.Certificate, error) {
	certs, err := pki.ParseCertificates([]byte(common.Value(data)))
	if err != nil {
		return nil, validation("%v.", err)
	}

	if len(certs) != 1 {
		return nil, validation("expected one certificate, got %d.", len(certs))
	}

	return certs[0], nil
}

// newCertificate is the record of a certificate, stamped now.
func (s *Service) newCertificate(scope request.Scope, c *x509.Certificate, status, ca, mode string) *certificate {
	now := common.Unix(s.now())
	return &certificate{
		ID: certificateID(c), PEM: pki.Encode(pki.BlockCertificate, c.Raw), Status: status, CA: ca, Mode: mode,
		Owner: scope.AccountID, Generation: common.RandomID(32, common.AlphabetLowerHex), Version: 1,
		Created: now, Modified: now, NotBefore: common.Unix(c.NotBefore), NotAfter: common.Unix(c.NotAfter),
	}
}

// store writes a new certificate, failing if it is already registered.
func (c *certificate) store(tx storage.WriteTx, scope request.Scope) error {
	key := scope.Key(service, kindCertificate, c.ID)
	if exists(tx, key) {
		return alreadyExists("Certificate %s already exists.", c.ID)
	}

	if err := indexCertificate(tx, scope, c); err != nil {
		return err
	}

	return storage.Write(tx, key, c)
}

// issue signs a client certificate for pub with TinyStack's IoT certificate
// authority.
func (s *Service) issue(ctx context.Context, pub any, subject *pkix.Name) (*x509.Certificate, error) {
	now := s.now()
	ca, err := s.ca.Get(ctx, now)
	if err != nil {
		return nil, err
	}

	c, _, err := ca.Issue(&x509.Certificate{
		Subject:     *subject,
		NotBefore:   now.Add(-time.Minute),
		NotAfter:    issuedUntil,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, pub)
	return c, err
}

func activeIf(set *bool) string {
	if common.Value(set) {
		return statusActive
	}

	return statusInactive
}

func (s *Service) CreateKeysAndCertificate(
	ctx context.Context,
	in *api.CreateKeysAndCertificateRequest,
) (*api.CreateKeysAndCertificateResponse, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	c, err := s.issue(ctx, &k.PublicKey, &pkix.Name{CommonName: issuedSubject})
	if err != nil {
		return nil, err
	}

	pub, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return nil, err
	}

	var rec *certificate
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		rec = s.newCertificate(scope, c, activeIf(in.SetAsActive), "", modeDefault)
		return rec.store(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateKeysAndCertificateResponse{
		CertificateArn: new(arn(scope, "cert", rec.ID)), CertificateId: new(rec.ID), CertificatePem: new(rec.PEM),
		KeyPair: &api.KeyPair{
			PublicKey:  new(pki.Encode("PUBLIC KEY", pub)),
			PrivateKey: new(pki.Encode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(k))),
		},
	}, nil
}

func (s *Service) CreateCertificateFromCsr(
	ctx context.Context,
	in *api.CreateCertificateFromCsrRequest,
) (*api.CreateCertificateFromCsrResponse, error) {
	csr, err := parseCSR(common.Value(in.CertificateSigningRequest))
	if err != nil {
		return nil, err
	}

	c, err := s.issue(ctx, csr.PublicKey, &csr.Subject)
	if err != nil {
		return nil, err
	}

	var rec *certificate
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		rec = s.newCertificate(scope, c, activeIf(in.SetAsActive), "", modeDefault)
		return rec.store(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateCertificateFromCsrResponse{
		CertificateArn: new(arn(scope, "cert", rec.ID)), CertificateId: new(rec.ID), CertificatePem: new(rec.PEM),
	}, nil
}

// parseCSR reads a signed PEM certificate signing request for an RSA key of
// at least 2048 bits or an ECDSA key on P-256, P-384, or P-521.
func parseCSR(data string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil || (block.Type != "CERTIFICATE REQUEST" && block.Type != "NEW CERTIFICATE REQUEST") {
		return nil, invalid("The certificate signing request must be one PEM CERTIFICATE REQUEST.")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, invalid("The certificate signing request is invalid: %v.", err)
	}

	if err := csr.CheckSignature(); err != nil {
		return nil, invalid("The certificate signing request's signature is invalid: %v.", err)
	}

	switch k := csr.PublicKey.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() >= 2048 {
			return csr, nil
		}
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() || k.Curve == elliptic.P384() || k.Curve == elliptic.P521() {
			return csr, nil
		}
	}

	return nil, invalid("The certificate signing request's key must be RSA of at least 2048 bits, or ECDSA on " +
		"P-256, P-384, or P-521.")
}

func describeCertificate(scope request.Scope, c *certificate) *api.CertificateDescription {
	d := &api.CertificateDescription{
		CertificateArn: new(arn(scope, "cert", c.ID)), CertificateId: new(c.ID), Status: new(c.Status),
		CertificatePem: new(c.PEM), OwnedBy: new(c.Owner), CreationDate: new(c.Created),
		LastModifiedDate: new(c.Modified), CustomerVersion: new(c.Version), GenerationId: new(c.Generation),
		Validity:        &api.CertificateValidity{NotBefore: new(c.NotBefore), NotAfter: new(c.NotAfter)},
		CertificateMode: new(c.Mode),
	}
	if c.CA != "" {
		d.CaCertificateId = new(c.CA)
	}

	return d
}

func (s *Service) DescribeCertificate(
	ctx context.Context,
	in *api.DescribeCertificateRequest,
) (*api.DescribeCertificateResponse, error) {
	var out *api.CertificateDescription
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		c, err := loadCertificate(tx, scope, common.Value(in.CertificateId))
		if err != nil {
			return err
		}

		out = describeCertificate(scope, c)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeCertificateResponse{CertificateDescription: out}, nil
}

// UpdateCertificate activates, deactivates, or revokes a certificate.
// Certificate transfers between accounts are not supported.
func (s *Service) UpdateCertificate(
	ctx context.Context,
	in *api.UpdateCertificateRequest,
) (*api.UpdateCertificateOutput, error) {
	status := common.Value(in.NewStatus)
	if status != statusActive && status != statusInactive && status != statusRevoked {
		return nil, invalid("The new status must be ACTIVE, INACTIVE, or REVOKED; certificate transfers are not " +
			"supported.")
	}

	var changed request.Scope
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		changed = scope
		c, err := loadCertificate(tx, scope, common.Value(in.CertificateId))
		if err != nil {
			return err
		}

		if c.Status == statusRevoked && status != statusRevoked {
			return certificateState("Certificate %s is revoked.", c.ID)
		}

		if c.Status == status {
			return nil
		}

		c.Status, c.Modified = status, common.Unix(s.now())
		c.Version++
		if err := indexCertificate(tx, scope, c); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key(service, kindCertificate, c.ID), c)
	})
	if err != nil {
		return nil, err
	}

	if status != statusActive {
		// Disconnecting an already disconnected or unconnected certificate is a no-op.
		s.disconnectCertificates(changed, common.Value(in.CertificateId))
	}

	return &api.UpdateCertificateOutput{}, nil
}

// DeleteCertificate deletes a certificate that is not active and not attached
// to a thing. Attached policies stop it unless forceDelete detaches them.
func (s *Service) DeleteCertificate(
	ctx context.Context,
	in *api.DeleteCertificateRequest,
) (*api.DeleteCertificateOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		c, err := loadCertificate(tx, scope, common.Value(in.CertificateId))
		if err != nil {
			return err
		}

		if c.Status == statusActive {
			return certificateState("Certificate %s must be deactivated before it is deleted.", c.ID)
		}

		id := arn(scope, "cert", c.ID)
		p, err := loadPrincipal(tx, scope, id)
		if err != nil {
			return err
		}

		if len(p.Things) > 0 {
			return conflict("Certificate %s cannot be deleted while it is attached to thing %s.", c.ID, p.Things[0])
		}

		if len(p.Policies) > 0 && !common.Value(in.ForceDelete) {
			return conflict("Certificate %s cannot be deleted while policy %s is attached to it.", c.ID, p.Policies[0])
		}

		if err := detachAll(tx, scope, id); err != nil {
			return err
		}

		tx.Delete(scope.Key(service, kindCertificate, c.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteCertificateOutput{}, nil
}

// listCertificates lists the certificates keep accepts, ordered by creation
// date.
func listCertificates(tx storage.ReadTx, scope request.Scope, ascending bool, keep func(*certificate) bool) (
	[]api.Certificate, error,
) {
	records, err := storage.Records[certificate](tx, scope.Prefix(service, kindCertificate))
	if err != nil {
		return nil, err
	}

	records = slices.DeleteFunc(records, func(c *certificate) bool { return !keep(c) })
	slices.SortStableFunc(records, func(a, b *certificate) int {
		if ascending {
			return cmp.Compare(a.Created, b.Created)
		}

		return cmp.Compare(b.Created, a.Created)
	})

	out := make([]api.Certificate, 0, len(records))
	for _, c := range records {
		out = append(out, api.Certificate{
			CertificateArn: new(arn(scope, "cert", c.ID)), CertificateId: new(c.ID), Status: new(c.Status),
			CertificateMode: new(c.Mode), CreationDate: new(c.Created),
		})
	}

	return out, nil
}

func (s *Service) ListCertificates(
	ctx context.Context,
	in *api.ListCertificatesRequest,
) (*api.ListCertificatesResponse, error) {
	var all []api.Certificate
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = listCertificates(tx, scope, common.Value(in.AscendingOrder),
			func(*certificate) bool { return true })
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(all, func(c api.Certificate) string { return *c.CertificateId }, in.Marker,
		in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.ListCertificatesResponse{Certificates: selected, NextMarker: next}, nil
}

func (s *Service) ListCertificatesByCA(
	ctx context.Context,
	in *api.ListCertificatesByCARequest,
) (*api.ListCertificatesByCAResponse, error) {
	ca := common.Value(in.CaCertificateId)
	var all []api.Certificate
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = listCertificates(tx, scope, common.Value(in.AscendingOrder),
			func(c *certificate) bool { return c.CA == ca })
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(all, func(c api.Certificate) string { return *c.CertificateId }, in.Marker,
		in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.ListCertificatesByCAResponse{Certificates: selected, NextMarker: next}, nil
}

// registerStatus is a registered certificate's initial status: status if
// given, else ACTIVE when setAsActive is set, else INACTIVE.
func registerStatus(status *string, setAsActive *bool) (string, error) {
	if status == nil {
		return activeIf(setAsActive), nil
	}

	if *status != statusActive && *status != statusInactive {
		return "", invalid("A certificate can be registered only as ACTIVE or INACTIVE.")
	}

	return *status, nil
}

// parseValid reads the one PEM certificate in data, which must be valid now.
func parseValid(data *string, now time.Time) (*x509.Certificate, error) {
	c, err := parseCertificate(data)
	if err != nil {
		return nil, err
	}

	if now.Before(c.NotBefore) || now.After(c.NotAfter) {
		return nil, validation("it is not valid at the current time.")
	}

	return c, nil
}

func (s *Service) RegisterCertificate(
	ctx context.Context,
	in *api.RegisterCertificateRequest,
) (*api.RegisterCertificateResponse, error) {
	c, err := parseValid(in.CertificatePem, s.now())
	if err != nil {
		return nil, err
	}

	status, err := registerStatus(in.Status, in.SetAsActive)
	if err != nil {
		return nil, err
	}

	var named *x509.Certificate
	if in.CaCertificatePem != nil {
		if named, err = parseCertificate(in.CaCertificatePem); err != nil {
			return nil, err
		}
	}

	var rec *certificate
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		ca, signErr := signer(tx, scope, c, named)
		if signErr != nil {
			return signErr
		}

		rec = s.newCertificate(scope, c, status, ca, modeDefault)
		return rec.store(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.RegisterCertificateResponse{
		CertificateArn: new(arn(scope, "cert", rec.ID)), CertificateId: new(rec.ID),
	}, nil
}

// signer returns the ID of the registered CA certificate that signed c: the
// one named, or else whichever registered CA's signature checks.
func signer(tx storage.ReadTx, scope request.Scope, c, named *x509.Certificate) (string, error) {
	if named != nil {
		id := certificateID(named)
		if _, err := loadCA(tx, scope, id); err != nil {
			return "", err
		}

		if err := c.CheckSignatureFrom(named); err != nil {
			return "", validation("it is not signed by CA certificate %s.", id)
		}

		return id, nil
	}

	cas, err := storage.Records[caCertificate](tx, scope.Prefix(service, kindCA))
	if err != nil {
		return "", err
	}

	for _, ca := range cas {
		parsed, err := parseCertificate(&ca.PEM)
		if err != nil {
			return "", err
		}

		if c.CheckSignatureFrom(parsed) == nil {
			return ca.ID, nil
		}
	}

	return "", validation("it is not signed by a registered CA certificate.")
}

// RegisterCertificateWithoutCA registers a certificate whose CA is not
// registered; such certificates connect with SNI.
func (s *Service) RegisterCertificateWithoutCA(
	ctx context.Context,
	in *api.RegisterCertificateWithoutCARequest,
) (*api.RegisterCertificateWithoutCAResponse, error) {
	c, err := parseValid(in.CertificatePem, s.now())
	if err != nil {
		return nil, err
	}

	status, err := registerStatus(in.Status, nil)
	if err != nil {
		return nil, err
	}

	var rec *certificate
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		rec = s.newCertificate(scope, c, status, "", modeSNIOnly)
		return rec.store(tx, scope)
	})
	if err != nil {
		return nil, err
	}

	return &api.RegisterCertificateWithoutCAResponse{
		CertificateArn: new(arn(scope, "cert", rec.ID)), CertificateId: new(rec.ID),
	}, nil
}
