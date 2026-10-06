package iot

import (
	"cmp"
	"context"
	"crypto/x509"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/pki"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// caCertificate is a registered CA certificate's record.
type caCertificate struct {
	Tags       map[string]string
	ID         string
	PEM        string
	Status     string
	Mode       string
	Owner      string
	Generation string
	Version    int64
	Created    float64
	Modified   float64
	NotBefore  float64
	NotAfter   float64
}

func loadCA(tx storage.ReadTx, scope request.Scope, id string) (*caCertificate, error) {
	c, ok, err := storage.Read[caCertificate](tx, scope.Key(service, kindCA, id))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("CA certificate %s cannot be found.", id))
	}

	return c, nil
}

func registrationKey(scope request.Scope) string { return scope.Key(service, kindRegistration, "code") }

func (s *Service) GetRegistrationCode(
	ctx context.Context,
	_ *api.GetRegistrationCodeRequest,
) (*api.GetRegistrationCodeResponse, error) {
	var code string
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		stored, found, err := storage.Read[string](tx, registrationKey(scope))
		if err != nil || found {
			code = common.Value(stored)
			return err
		}

		code = common.RandomID(64, "0123456789abcdef")
		return storage.Write(tx, registrationKey(scope), code)
	})
	if err != nil {
		return nil, err
	}

	return &api.GetRegistrationCodeResponse{RegistrationCode: new(code)}, nil
}

func (s *Service) DeleteRegistrationCode(
	ctx context.Context,
	_ *api.DeleteRegistrationCodeRequest,
) (*api.DeleteRegistrationCodeResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		tx.Delete(registrationKey(scope))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRegistrationCodeResponse{}, nil
}

// RegisterCACertificate registers a CA certificate. In DEFAULT mode its
// verification certificate must be signed by it for the account's
// registration code; SNI_ONLY CAs need none.
func (s *Service) RegisterCACertificate(
	ctx context.Context,
	in *api.RegisterCACertificateRequest,
) (*api.RegisterCACertificateResponse, error) {
	ca, err := parseValid(in.CaCertificate, s.now())
	if err != nil {
		return nil, err
	}

	if !ca.BasicConstraintsValid || !ca.IsCA {
		return nil, validation("it is not a CA certificate.")
	}

	mode := cmp.Or(common.Value(in.CertificateMode), modeDefault)
	if mode != modeDefault && mode != modeSNIOnly {
		return nil, invalid("The certificate mode must be DEFAULT or SNI_ONLY.")
	}

	if mode == modeDefault && in.VerificationCertificate == nil {
		return nil, invalid("A CA certificate in DEFAULT mode needs a verification certificate.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	now := common.Unix(s.now())
	rec := &caCertificate{
		ID: certificateID(ca), PEM: pki.Encode(pki.BlockCertificate, ca.Raw), Status: activeIf(in.SetAsActive),
		Mode: mode, Generation: common.RandomID(32, common.AlphabetLowerHex), Version: 1, Created: now,
		Modified: now, NotBefore: common.Unix(ca.NotBefore), NotAfter: common.Unix(ca.NotAfter), Tags: tags,
	}
	var scope request.Scope
	err = s.update(ctx, func(tx storage.WriteTx, sc request.Scope) error {
		scope = sc
		rec.Owner = scope.AccountID
		if in.VerificationCertificate != nil {
			if verifyErr := verifyRegistration(tx, scope, ca, in.VerificationCertificate); verifyErr != nil {
				return verifyErr
			}
		}

		key := scope.Key(service, kindCA, rec.ID)
		if exists(tx, key) {
			return alreadyExists("CA certificate %s already exists.", rec.ID)
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.RegisterCACertificateResponse{
		CertificateArn: new(arn(scope, "cacert", rec.ID)), CertificateId: new(rec.ID),
	}, nil
}

// verifyRegistration checks that a verification certificate is signed by the
// CA and names the registration code as its common name.
func verifyRegistration(tx storage.ReadTx, scope request.Scope, ca *x509.Certificate, pemData *string) error {
	v, err := parseCertificate(pemData)
	if err != nil {
		return err
	}

	code, found, err := storage.Read[string](tx, registrationKey(scope))
	if err != nil {
		return err
	}

	if !found || v.Subject.CommonName != *code {
		return awserror.New("RegistrationCodeValidationException",
			"The verification certificate's common name is not the registration code.")
	}

	if err := v.CheckSignatureFrom(ca); err != nil {
		return validation("the verification certificate is not signed by the CA certificate.")
	}

	return nil
}

func (s *Service) DescribeCACertificate(
	ctx context.Context,
	in *api.DescribeCACertificateRequest,
) (*api.DescribeCACertificateResponse, error) {
	var out *api.CACertificateDescription
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		c, err := loadCA(tx, scope, common.Value(in.CertificateId))
		if err != nil {
			return err
		}

		out = &api.CACertificateDescription{
			CertificateArn: new(arn(scope, "cacert", c.ID)), CertificateId: new(c.ID), Status: new(c.Status),
			CertificatePem: new(c.PEM), OwnedBy: new(c.Owner), CreationDate: new(c.Created),
			AutoRegistrationStatus: new("DISABLE"), LastModifiedDate: new(c.Modified),
			CustomerVersion: new(c.Version), GenerationId: new(c.Generation), CertificateMode: new(c.Mode),
			Validity: &api.CertificateValidity{NotBefore: new(c.NotBefore), NotAfter: new(c.NotAfter)},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeCACertificateResponse{CertificateDescription: out}, nil
}

func (s *Service) UpdateCACertificate(
	ctx context.Context,
	in *api.UpdateCACertificateRequest,
) (*api.UpdateCACertificateOutput, error) {
	status := common.Value(in.NewStatus)
	if in.NewStatus != nil && status != statusActive && status != statusInactive {
		return nil, invalid("The new status must be ACTIVE or INACTIVE.")
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		c, err := loadCA(tx, scope, common.Value(in.CertificateId))
		if err != nil {
			return err
		}

		if in.NewStatus == nil || c.Status == status {
			return nil
		}

		c.Status, c.Modified = status, common.Unix(s.now())
		c.Version++
		return storage.Write(tx, scope.Key(service, kindCA, c.ID), c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateCACertificateOutput{}, nil
}

// DeleteCACertificate deletes an inactive CA certificate that no registered
// certificate names.
func (s *Service) DeleteCACertificate(
	ctx context.Context,
	in *api.DeleteCACertificateRequest,
) (*api.DeleteCACertificateResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		c, err := loadCA(tx, scope, common.Value(in.CertificateId))
		if err != nil {
			return err
		}

		if c.Status == statusActive {
			return certificateState("CA certificate %s must be deactivated before it is deleted.", c.ID)
		}

		certs, err := storage.Records[certificate](tx, scope.Prefix(service, kindCertificate))
		if err != nil {
			return err
		}

		for _, d := range certs {
			if d.CA == c.ID {
				return certificateState("CA certificate %s still signs certificate %s.", c.ID, d.ID)
			}
		}

		tx.Delete(scope.Key(service, kindCA, c.ID))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteCACertificateResponse{}, nil
}

func (s *Service) ListCACertificates(
	ctx context.Context,
	in *api.ListCACertificatesRequest,
) (*api.ListCACertificatesResponse, error) {
	var out []api.CACertificate
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		records, err := storage.Records[caCertificate](tx, scope.Prefix(service, kindCA))
		if err != nil {
			return err
		}

		ascending := common.Value(in.AscendingOrder)
		slices.SortStableFunc(records, func(a, b *caCertificate) int {
			if ascending {
				return cmp.Compare(a.Created, b.Created)
			}

			return cmp.Compare(b.Created, a.Created)
		})

		for _, c := range records {
			out = append(out, api.CACertificate{
				CertificateArn: new(arn(scope, "cacert", c.ID)), CertificateId: new(c.ID), Status: new(c.Status),
				CreationDate: new(c.Created),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(out, func(c api.CACertificate) string { return *c.CertificateId }, in.Marker,
		in.PageSize)
	if err != nil {
		return nil, err
	}

	return &api.ListCACertificatesResponse{Certificates: selected, NextMarker: next}, nil
}
