package acmpca

import (
	"context"
	"encoding/base64"
	"time"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/acmpca"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// missingBucket returns the CRL bucket that rev enables and that does not exist, or "" when there is none.
func (s *Service) missingBucket(ctx context.Context, rev *api.RevocationConfiguration) string {
	if rev == nil || rev.CrlConfiguration == nil || !common.Value(rev.CrlConfiguration.Enabled) {
		return ""
	}

	bucket := *rev.CrlConfiguration.S3BucketName
	if _, err := s.buckets.HeadBucket(ctx, &s3api.HeadBucketRequest{Bucket: new(bucket)}); err != nil {
		return bucket
	}

	return ""
}

func (s *Service) CreateCertificateAuthority(
	ctx context.Context,
	in *api.CreateCertificateAuthorityRequest,
) (*api.CreateCertificateAuthorityResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalidArgs("CertificateAuthorityConfiguration is required")
	}

	if checkErr := checkEnum("CertificateAuthorityType", in.CertificateAuthorityType, authorityTypes); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkConfiguration(in.CertificateAuthorityConfiguration, scope.Region); checkErr != nil {
		return nil, checkErr
	}

	if checkErr := checkRevocation(in.RevocationConfiguration); checkErr != nil {
		return nil, checkErr
	}

	usage := common.OrElse(in.UsageMode, new(usageGeneral))
	if checkErr := checkEnum("UsageMode", usage, usageModes); checkErr != nil {
		return nil, checkErr
	}

	security := common.OrElse(in.KeyStorageSecurityStandard, new(securityDefault))
	if checkErr := checkEnum("KeyStorageSecurityStandard", security, securityStandards); checkErr != nil {
		return nil, checkErr
	}

	token := common.Value(in.IdempotencyToken)
	if in.IdempotencyToken != nil && (token == "" || len(token) > 36 || !tokenPattern.MatchString(token)) {
		return nil, invalidArgs("IdempotencyToken must be 1 to 36 characters")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if tagErr := tagging.CheckSet(tags); tagErr != nil {
		return nil, tagError(tagErr)
	}

	if bucket := s.missingBucket(ctx, in.RevocationConfiguration); bucket != "" {
		return nil, invalidArgs("The CRL S3 bucket %s does not exist", bucket)
	}

	now := time.Now().UTC()
	id := uuid.NewV4().String()
	rec := authorityRecord{
		Authority: &api.CertificateAuthority{
			Arn:                               new(authorityARN(scope, id)),
			OwnerAccount:                      new(scope.AccountID),
			CreatedAt:                         new(common.Unix(now)),
			LastStateChangeAt:                 new(common.Unix(now)),
			Type:                              in.CertificateAuthorityType,
			Status:                            new(statusPending),
			CertificateAuthorityConfiguration: common.Clone(in.CertificateAuthorityConfiguration),
			RevocationConfiguration:           common.Clone(in.RevocationConfiguration),
			KeyStorageSecurityStandard:        security,
			UsageMode:                         usage,
		},
		Tags: tags,
	}

	arn := *rec.Authority.Arn
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if purgeErr := purgeExpired(tx, scope, now); purgeErr != nil {
			return purgeErr
		}

		tokenKey := scope.Key(serviceName, tokenKind, base64.RawURLEncoding.EncodeToString([]byte(token)))
		if token != "" {
			seen, found, readErr := storage.Read[tokenRecord](tx, tokenKey)
			if readErr != nil {
				return readErr
			}

			if found && now.Before(seen.ExpiresAt) {
				if _, _, loadErr := load(tx, scope, new(authorityARN(scope, seen.ID)), now); loadErr == nil {
					arn = authorityARN(scope, seen.ID)
					return nil
				}
			}

			remembered := tokenRecord{ID: id, ExpiresAt: now.Add(idempotencyWindow)}
			if writeErr := storage.Write(tx, tokenKey, remembered); writeErr != nil {
				return writeErr
			}
		}

		return storage.Write(tx, scope.Key(serviceName, authorityKind, id), rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateCertificateAuthorityResponse{CertificateAuthorityArn: new(arn)}, nil
}

// purgeExpired removes the authorities whose restoration period has ended.
func purgeExpired(tx storage.WriteTx, scope request.Scope, now time.Time) error {
	for _, key := range tx.Keys(scope.Prefix(serviceName, authorityKind)) {
		rec, found, err := storage.Read[authorityRecord](tx, key)
		if err != nil {
			return err
		}

		if found && rec.expired(now) {
			tx.Delete(key)
		}
	}

	return nil
}

func (s *Service) DescribeCertificateAuthority(
	ctx context.Context,
	in *api.DescribeCertificateAuthorityRequest,
) (*api.DescribeCertificateAuthorityResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalidArn("CertificateAuthorityArn is required")
	}

	var rec *authorityRecord
	if viewErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		rec, _, loadErr = load(tx, scope, in.CertificateAuthorityArn, time.Now())
		return loadErr
	}); viewErr != nil {
		return nil, viewErr
	}

	return &api.DescribeCertificateAuthorityResponse{CertificateAuthority: common.Clone(rec.Authority)}, nil
}

func (s *Service) ListCertificateAuthorities(
	ctx context.Context,
	in *api.ListCertificateAuthoritiesRequest,
) (*api.ListCertificateAuthoritiesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		in = &api.ListCertificateAuthoritiesRequest{}
	}

	owner := common.OrElse(in.ResourceOwner, new(ownerSelf))
	if checkErr := checkEnum("ResourceOwner", owner, owners); checkErr != nil {
		return nil, checkErr
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	// Authorities are not shared between accounts, so no other account's authority is visible.
	if *owner == ownerOtherAccounts {
		return &api.ListCertificateAuthoritiesResponse{}, nil
	}

	now := time.Now()
	var list []api.CertificateAuthority
	if viewErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[authorityRecord](tx, scope.Prefix(serviceName, authorityKind))
		if readErr != nil {
			return readErr
		}

		for _, rec := range records {
			if !rec.expired(now) {
				list = append(list, *common.Clone(rec.Authority))
			}
		}

		return nil
	}); viewErr != nil {
		return nil, viewErr
	}

	page, next, err := common.Page(list, func(a api.CertificateAuthority) string {
		return *a.Arn
	}, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	return &api.ListCertificateAuthoritiesResponse{CertificateAuthorities: page, NextToken: next}, nil
}

// pageSize returns the page size a MaxResults asks for.
func pageSize(maxResults *int64) (int, error) {
	if maxResults == nil {
		return defaultPageSize, nil
	}

	if *maxResults < 1 || *maxResults > maxPageSize {
		return 0, invalidArgs("MaxResults must be from 1 to %d", maxPageSize)
	}

	return int(*maxResults), nil
}

// UpdateCertificateAuthority changes an authority's status and revocation configuration. Only an ACTIVE or DISABLED
// authority has a status to change, to the other of the two; a supplied CRL or OCSP block replaces the stored one.
func (s *Service) UpdateCertificateAuthority(
	ctx context.Context,
	in *api.UpdateCertificateAuthorityRequest,
) (*api.UpdateCertificateAuthorityOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalidArn("CertificateAuthorityArn is required")
	}

	if in.Status != nil && *in.Status != statusActive && *in.Status != statusDisabled {
		return nil, invalidArgs("Status can only be changed to %s or %s", statusActive, statusDisabled)
	}

	if checkErr := checkRevocation(in.RevocationConfiguration); checkErr != nil {
		return nil, checkErr
	}

	if bucket := s.missingBucket(ctx, in.RevocationConfiguration); bucket != "" {
		return nil, notFound("The CRL S3 bucket %s does not exist", bucket)
	}

	now := time.Now().UTC()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, loadErr := load(tx, scope, in.CertificateAuthorityArn, now)
		if loadErr != nil {
			return loadErr
		}

		status := common.Value(rec.Authority.Status)
		if status == statusDeleted {
			return invalidState("The certificate authority is deleted")
		}

		if in.Status != nil {
			if status != statusActive && status != statusDisabled {
				return invalidState("The status of a certificate authority in the %s state cannot be changed", status)
			}

			rec.Authority.Status = in.Status
		}

		if rev := in.RevocationConfiguration; rev != nil {
			current := common.OrElse(rec.Authority.RevocationConfiguration, &api.RevocationConfiguration{})
			current.CrlConfiguration = common.OrElse(common.Clone(rev.CrlConfiguration), current.CrlConfiguration)
			current.OcspConfiguration = common.OrElse(common.Clone(rev.OcspConfiguration), current.OcspConfiguration)
			rec.Authority.RevocationConfiguration = current
		}

		rec.Authority.LastStateChangeAt = new(common.Unix(now))
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateCertificateAuthorityOutput{}, nil
}

// DeleteCertificateAuthority keeps a deleted authority, in the DELETED state, until its restoration period ends.
// An ACTIVE authority must be disabled first.
func (s *Service) DeleteCertificateAuthority(
	ctx context.Context,
	in *api.DeleteCertificateAuthorityRequest,
) (*api.DeleteCertificateAuthorityOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalidArn("CertificateAuthorityArn is required")
	}

	days := common.Value(common.OrElse(in.PermanentDeletionTimeInDays, new(int64(defaultRestorableDays))))
	if days < 7 || days > 30 {
		return nil, invalidArgs("PermanentDeletionTimeInDays must be from 7 to 30")
	}

	now := time.Now().UTC()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, loadErr := load(tx, scope, in.CertificateAuthorityArn, now)
		if loadErr != nil {
			return loadErr
		}

		status := common.Value(rec.Authority.Status)
		if status == statusActive || status == statusDeleted {
			return invalidState("A certificate authority in the %s state cannot be deleted; disable it first", status)
		}

		rec.PreviousStatus = status
		rec.Authority.Status = new(statusDeleted)
		rec.Authority.RestorableUntil = new(common.Unix(now.Add(time.Duration(days) * 24 * time.Hour)))
		rec.Authority.LastStateChangeAt = new(common.Unix(now))
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteCertificateAuthorityOutput{}, nil
}

// RestoreCertificateAuthority returns a deleted authority, within its restoration period, to the status it had
// before it was deleted.
func (s *Service) RestoreCertificateAuthority(
	ctx context.Context,
	in *api.RestoreCertificateAuthorityRequest,
) (*api.RestoreCertificateAuthorityOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil {
		return nil, invalidArn("CertificateAuthorityArn is required")
	}

	now := time.Now().UTC()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, loadErr := load(tx, scope, in.CertificateAuthorityArn, now)
		if loadErr != nil {
			return loadErr
		}

		if status := common.Value(rec.Authority.Status); status != statusDeleted {
			return invalidState("A certificate authority in the %s state is not deleted", status)
		}

		rec.Authority.Status = new(rec.PreviousStatus)
		rec.Authority.RestorableUntil = nil
		rec.Authority.LastStateChangeAt = new(common.Unix(now))
		rec.PreviousStatus = ""
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.RestoreCertificateAuthorityOutput{}, nil
}
