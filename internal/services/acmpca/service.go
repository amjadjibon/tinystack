// Package acmpca stores AWS Private CA certificate authorities and their tags. It is records-only: no key pair is
// generated and no certificate is imported or issued, so a new authority stays PENDING_CERTIFICATE, and the CRL and
// OCSP settings are validated metadata that no revocation list or responder serves.
package acmpca

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/acmpca"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName   = "acmpca"
	authorityKind = "certificate-authority"
	tokenKind     = "token"

	statusPending  = "PENDING_CERTIFICATE"
	statusActive   = "ACTIVE"
	statusDisabled = "DISABLED"
	statusDeleted  = "DELETED"

	defaultRestorableDays = 30
	idempotencyWindow     = 5 * time.Minute
	defaultPageSize       = 100
)

var _ api.API = (*Service)(nil)

type (
	// Buckets checks that a CRL bucket exists in the caller's account and Region.
	Buckets interface {
		HeadBucket(context.Context, *s3api.HeadBucketRequest) (*s3api.HeadBucketOutput, error)
	}

	Service struct {
		store   storage.Store
		buckets Buckets
	}

	// authorityRecord holds one authority. PreviousStatus is the status a deleted authority returns to on restore.
	authorityRecord struct {
		Authority      *api.CertificateAuthority
		Tags           map[string]string
		PreviousStatus string
	}

	// tokenRecord remembers the authority a CreateCertificateAuthority idempotency token created.
	tokenRecord struct {
		ExpiresAt time.Time
		ID        string
	}
)

func New(store storage.Store, buckets Buckets) *Service {
	return &Service{store: store, buckets: buckets}
}

func invalidArgs(format string, args ...any) error {
	return awserror.New("InvalidArgsException", fmt.Sprintf(format, args...))
}

func invalidArn(format string, args ...any) error {
	return awserror.New("InvalidArnException", fmt.Sprintf(format, args...))
}

func invalidState(format string, args ...any) error {
	return awserror.New("InvalidStateException", fmt.Sprintf(format, args...))
}

func invalidTag(format string, args ...any) error {
	return awserror.New("InvalidTagException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

func invalidToken() error {
	return awserror.New("InvalidNextTokenException", "The NextToken is not valid")
}

func authorityARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:acm-pca:%s:%s:%s/%s", scope.Region, scope.AccountID, authorityKind, id)
}

// expired reports whether a deleted authority is past its restoration period, which makes it permanently gone.
func (r *authorityRecord) expired(now time.Time) bool {
	return common.Value(r.Authority.Status) == statusDeleted &&
		common.Unix(now) >= common.Value(r.Authority.RestorableUntil)
}

// load reads the authority an ARN names in scope. An authority past its restoration period is not found, which
// removes deleted authorities lazily.
func load(
	tx storage.ReadTx,
	scope request.Scope,
	arn *string,
	now time.Time,
) (rec *authorityRecord, key string, err error) {
	id, err := parseARN(scope, arn)
	if err != nil {
		return nil, "", err
	}

	key = scope.Key(serviceName, authorityKind, id)
	rec, found, err := storage.Read[authorityRecord](tx, key)
	if err != nil {
		return nil, "", err
	}

	if !found || rec == nil || rec.expired(now) {
		return nil, "", notFound("Certificate authority %s was not found", *arn)
	}

	return rec, key, nil
}

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TooManyTagsException", err.Error())
	}

	return invalidTag("%s", err.Error())
}
