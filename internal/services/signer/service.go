// Package signer stores signing profiles, permissions and jobs. Artifacts contain a TinyStack
// envelope, not a cryptographic signature verifiable by AWS tooling.
package signer

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	acmapi "github.com/amjadjibon/tinystack/internal/aws/generated/acm"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/signer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "signer"
	active      = "Active"
	canceled    = "Canceled"
	revoked     = "Revoked"
)

var (
	namePattern         = regexp.MustCompile(`^[A-Za-z0-9_]{2,64}$`)
	_           api.API = (*Service)(nil)
)

// Objects accesses S3 outside storage transactions.
type Objects interface {
	Object(context.Context, string, string, string) ([]byte, error)
	PutObject(context.Context, *s3api.PutObjectRequest) (*s3api.PutObjectOutput, error)
}

// Certificates validates signing material in the caller's account and Region.
type Certificates interface {
	DescribeCertificate(
		context.Context,
		*acmapi.DescribeCertificateRequest,
	) (*acmapi.DescribeCertificateResponse, error)
}

type (
	// Service implements the Signer control plane.
	Service struct {
		store        storage.Store
		objects      Objects
		certificates Certificates
	}

	profile struct {
		Detail      api.GetSigningProfileResponse
		Versions    map[string]api.GetSigningProfileResponse
		Revision    string
		Permissions []api.Permission
	}
)

// New constructs a Signer service with S3 and ACM reference validation.
func New(store storage.Store, objects Objects, certificates Certificates) *Service {
	return &Service{store: store, objects: objects, certificates: certificates}
}

func invalid(message string) error { return awserror.New("ValidationException", message) }
func missing() error               { return awserror.New("ResourceNotFoundException", "Resource does not exist.") }

func conflict() error {
	return awserror.New("ConflictException", "Resource state or revision does not match.")
}

func read(tx storage.ReadTx, scope request.Scope, name *string) (*profile, error) {
	if !namePattern.MatchString(common.Value(name)) {
		return nil, invalid("Invalid profile name.")
	}

	r, ok, err := storage.Read[profile](tx, scope.Key(serviceName, "profile", *name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *profile) error {
	return storage.Write(tx, scope.Key(serviceName, "profile", *r.Detail.ProfileName), r)
}

func (s *Service) change(ctx context.Context, name *string, fn func(*profile) error) (*profile, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *profile
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, name)
		if e != nil {
			return e
		}

		if e := fn(r); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
	return r, err
}

func limit(n *int64) (int, error) {
	if n == nil {
		return 25, nil
	}

	if *n < 1 || *n > 25 {
		return 0, invalid("MaxResults must be between 1 and 25.")
	}

	return int(*n), nil
}

func owner(scope request.Scope, v *string) error {
	if v != nil && *v != scope.AccountID {
		return missing()
	}

	return nil
}

// PutSigningProfile creates a new immutable version and makes it current.
func (s *Service) PutSigningProfile(
	ctx context.Context,
	in *api.PutSigningProfileRequest,
) (*api.PutSigningProfileResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !namePattern.MatchString(common.Value(in.ProfileName)) {
		return nil, invalid("Invalid profile name.")
	}

	platform, err := platformByID(in.PlatformId)
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(in.Tags); err != nil {
		return nil, invalid(err.Error())
	}

	managed := common.Value(platform.RevocationSupported)
	if !managed && in.SigningMaterial == nil {
		return nil, invalid("This platform requires ACM signing material.")
	}

	if in.SigningMaterial != nil {
		if common.Value(in.SigningMaterial.CertificateArn) == "" {
			return nil, invalid("CertificateArn is required.")
		}

		if _, err = s.certificates.DescribeCertificate(ctx, &acmapi.DescribeCertificateRequest{
			CertificateArn: in.SigningMaterial.CertificateArn,
		}); err != nil {
			return nil, invalid("Signing certificate must exist in TinyStack ACM in this account and Region.")
		}
	}

	validity := common.Clone(in.SignatureValidityPeriod)
	if validity != nil && !managed {
		return nil, invalid("Signature validity is supported only for Lambda and Notation.")
	}

	if managed && validity == nil {
		validity = &api.SignatureValidityPeriod{Value: new(int64(135)), Type: new("MONTHS")}
	}

	if validity != nil {
		maxValue := map[string]int64{"DAYS": 4109, "MONTHS": 135, "YEARS": 11}[common.Value(validity.Type)]
		if common.Value(validity.Value) < 1 || common.Value(validity.Value) > maxValue {
			return nil, invalid("Invalid signature validity period.")
		}
	}

	version := common.RandomID(10)
	arn := fmt.Sprintf("arn:aws:signer:%s:%s:/signing-profiles/%s", scope.Region, scope.AccountID, *in.ProfileName)
	detail := api.GetSigningProfileResponse{
		ProfileName:             in.ProfileName,
		ProfileVersion:          new(version),
		ProfileVersionArn:       new(arn + "/" + version),
		Arn:                     new(arn),
		PlatformId:              in.PlatformId,
		PlatformDisplayName:     platform.DisplayName,
		SigningMaterial:         in.SigningMaterial,
		SignatureValidityPeriod: validity,
		Status:                  new(active),
		Tags:                    common.CloneTags(in.Tags),
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, ok, e := storage.Read[profile](tx, scope.Key(serviceName, "profile", *in.ProfileName))
		if e != nil {
			return e
		}

		if !ok {
			r = &profile{Versions: map[string]api.GetSigningProfileResponse{}, Revision: common.RandomID(16)}
		}

		if ok && common.Value(r.Detail.Status) == revoked {
			return conflict()
		}

		r.Detail = detail
		r.Versions[version] = detail
		return save(tx, scope, r)
	})
	return &api.PutSigningProfileResponse{
		Arn:               new(arn),
		ProfileVersion:    new(version),
		ProfileVersionArn: detail.ProfileVersionArn,
	}, err
}

// GetSigningProfile returns the current profile version. Permissions have their own list operation.
func (s *Service) GetSigningProfile(
	ctx context.Context,
	in *api.GetSigningProfileRequest,
) (*api.GetSigningProfileResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if ownerErr := owner(scope, in.ProfileOwner); ownerErr != nil {
		return nil, ownerErr
	}

	var r *profile
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, in.ProfileName)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &r.Detail, nil
}

// CancelSigningProfile prevents new jobs while retaining metadata and history.
func (s *Service) CancelSigningProfile(
	ctx context.Context,
	in *api.CancelSigningProfileRequest,
) (*api.CancelSigningProfileOutput, error) {
	_, err := s.change(ctx, in.ProfileName, func(r *profile) error {
		if common.Value(r.Detail.Status) == revoked {
			return conflict()
		}

		r.Detail.Status = new(canceled)
		r.Versions[*r.Detail.ProfileVersion] = r.Detail
		return nil
	})
	return &api.CancelSigningProfileOutput{}, err
}

// RevokeSigningProfile revokes a particular immutable profile version.
func (s *Service) RevokeSigningProfile(
	ctx context.Context,
	in *api.RevokeSigningProfileRequest,
) (*api.RevokeSigningProfileOutput, error) {
	if common.Value(in.Reason) == "" || in.EffectiveTime == nil {
		return nil, invalid("Reason and EffectiveTime are required.")
	}

	_, err := s.change(ctx, in.ProfileName, func(r *profile) error {
		d, ok := r.Versions[common.Value(in.ProfileVersion)]
		if !ok {
			return missing()
		}

		p, _ := platformByID(d.PlatformId)
		if !common.Value(p.RevocationSupported) {
			return invalid("Platform does not support revocation.")
		}

		d.Status = new(revoked)
		d.RevocationRecord = &api.SigningProfileRevocationRecord{
			RevocationEffectiveFrom: in.EffectiveTime,
			RevokedAt: common.Seconds(
				time.Now(),
			),
			RevokedBy: new(request.CallerARN(ctx)),
		}

		d.StatusReason = in.Reason
		r.Versions[*in.ProfileVersion] = d
		if *r.Detail.ProfileVersion == *in.ProfileVersion {
			r.Detail = d
		}

		return nil
	})
	return &api.RevokeSigningProfileOutput{}, err
}

// ListSigningProfiles lists current versions, with status and platform filters.
func (s *Service) ListSigningProfiles(
	ctx context.Context,
	in *api.ListSigningProfilesRequest,
) (*api.ListSigningProfilesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	n, err := limit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	for _, status := range in.Statuses {
		if !slices.Contains([]string{active, canceled, revoked}, status) {
			return nil, invalid("Invalid status.")
		}
	}

	rows := []api.SigningProfile{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[profile](tx, scope.Prefix(serviceName, "profile"))
		if e != nil {
			return e
		}

		for _, r := range records {
			d := r.Detail
			if in.PlatformId != nil && *in.PlatformId != *d.PlatformId {
				continue
			}

			if len(in.Statuses) > 0 && !slices.Contains(in.Statuses, *d.Status) {
				continue
			}

			if len(in.Statuses) == 0 && !common.Value(in.IncludeCanceled) && *d.Status == canceled {
				continue
			}

			rows = append(rows, api.SigningProfile{
				ProfileName:             d.ProfileName,
				ProfileVersion:          d.ProfileVersion,
				ProfileVersionArn:       d.ProfileVersionArn,
				Arn:                     d.Arn,
				PlatformId:              d.PlatformId,
				PlatformDisplayName:     d.PlatformDisplayName,
				SigningMaterial:         d.SigningMaterial,
				SignatureValidityPeriod: d.SignatureValidityPeriod,
				Status:                  d.Status,
				Tags:                    d.Tags,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(d api.SigningProfile) string { return *d.ProfileName },
		in.NextToken,
		n,
		invalid("Invalid NextToken."),
	)
	return &api.ListSigningProfilesResponse{Profiles: page, NextToken: next}, err
}
