package signer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/signer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type (
	job struct {
		Detail  api.DescribeSigningJobResponse
		Request api.StartSigningJobRequest
	}

	jobToken struct {
		ID    string
		Input string
	}
)

func readJob(tx storage.ReadTx, scope request.Scope, id *string) (*job, error) {
	r, ok, err := storage.Read[job](tx, scope.Key(serviceName, "job", common.Value(id)))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func expires(v *api.SignatureValidityPeriod, now time.Time) *float64 {
	if v == nil {
		return nil
	}

	n := int(common.Value(v.Value))
	switch common.Value(v.Type) {
	case "DAYS":
		now = now.AddDate(0, 0, n)
	case "MONTHS":
		now = now.AddDate(0, n, 0)
	case "YEARS":
		now = now.AddDate(n, 0, 0)
	}

	return common.Seconds(now)
}

// StartSigningJob validates an S3 source and writes an explicitly non-cryptographic artifact.
// The reserved token/job record precedes S3 I/O, so retries never duplicate side effects.
func (s *Service) StartSigningJob(
	ctx context.Context,
	in *api.StartSigningJobRequest,
) (*api.StartSigningJobResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if ownerErr := owner(scope, in.ProfileOwner); ownerErr != nil {
		return nil, ownerErr
	}

	if in.Source == nil || in.Source.S3 == nil || in.Destination == nil || in.Destination.S3 == nil {
		return nil, invalid("S3 Source and Destination are required.")
	}

	source, dest := in.Source.S3, in.Destination.S3
	if common.Value(source.BucketName) == "" || common.Value(source.Key) == "" || common.Value(source.Version) == "" ||
		common.Value(dest.BucketName) == "" || common.Value(in.ClientRequestToken) == "" {
		return nil, invalid("Source bucket, key, version, destination bucket and ClientRequestToken are required.")
	}

	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	tokenKey := scope.Key(serviceName, "token", *in.ClientRequestToken)
	id := common.RandomID(16)
	var r *job
	replay := false
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		token, ok, e := storage.Read[jobToken](tx, tokenKey)
		if e != nil {
			return e
		}

		if ok {
			if token.Input != string(body) {
				return conflict()
			}

			id = token.ID
			replay = true
			return nil
		}

		p, e := read(tx, scope, in.ProfileName)
		if e != nil {
			return e
		}

		d := p.Detail
		if common.Value(d.Status) != active {
			return conflict()
		}

		if common.Value(d.PlatformId) == "Notation-OCI-SHA384-ECDSA" {
			return invalid("Notation requires SignPayload and registry integration, which are unsupported.")
		}

		now := time.Now()
		r = &job{Request: *common.Clone(in), Detail: api.DescribeSigningJobResponse{
			JobId: new(id), Source: in.Source, SigningMaterial: d.SigningMaterial, PlatformId: d.PlatformId,
			PlatformDisplayName: d.PlatformDisplayName, ProfileName: d.ProfileName, ProfileVersion: d.ProfileVersion,
			CreatedAt: common.Seconds(now), SignatureExpiresAt: expires(d.SignatureValidityPeriod, now),
			RequestedBy: new("arn:aws:iam::" + scope.AccountID + ":root"), Status: new("InProgress"),
			JobOwner: new(scope.AccountID), JobInvoker: new(scope.AccountID),
		}}
		if e := storage.Write(tx, scope.Key(serviceName, "job", id), r); e != nil {
			return e
		}

		return storage.Write(tx, tokenKey, jobToken{ID: id, Input: string(body)})
	})
	if err != nil {
		return nil, err
	}

	out := &api.StartSigningJobResponse{JobId: new(id), JobOwner: new(scope.AccountID)}
	if replay {
		return out, nil
	}

	data, ioErr := s.objects.Object(ctx, *source.BucketName, *source.Key, *source.Version)
	if ioErr == nil {
		p, _ := platformByID(r.Detail.PlatformId)
		if int64(len(data)) > common.Value(p.MaxSizeInMB)*1024*1024 {
			ioErr = invalid("Source exceeds platform maximum size.")
		}
	}

	key := common.Value(dest.Prefix) + id
	if ioErr == nil {
		envelope := fmt.Sprintf("\nTINYSTACK SIGNATURE ENVELOPE (NOT CRYPTOGRAPHIC)\njob=%s\nprofile=%s/%s\n",
			id, *r.Detail.ProfileName, *r.Detail.ProfileVersion)
		_, ioErr = s.objects.PutObject(ctx, &s3api.PutObjectRequest{
			Bucket: dest.BucketName, Key: new(key),
			Body: append(data, []byte(envelope)...), ContentType: new("application/octet-stream"),
		})
	}

	r.Detail.CompletedAt = common.Seconds(time.Now())
	if ioErr != nil {
		r.Detail.Status = new("Failed")
		r.Detail.StatusReason = new("S3 source validation or output failed: " + ioErr.Error())
	} else {
		r.Detail.Status = new("Succeeded")
		r.Detail.StatusReason = new("TinyStack envelope written; not cryptographically verifiable by AWS tooling.")
		r.Detail.SignedObject = &api.SignedObject{S3: &api.S3SignedObject{BucketName: dest.BucketName, Key: new(key)}}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, scope.Key(serviceName, "job", id), r)
	})
	return out, err
}

// DescribeSigningJob returns job metadata, output location and revocation state.
func (s *Service) DescribeSigningJob(
	ctx context.Context,
	in *api.DescribeSigningJobRequest,
) (*api.DescribeSigningJobResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *job
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readJob(tx, scope, in.JobId)
		return e
	})
	if err != nil {
		return nil, err
	}

	return &r.Detail, nil
}

// ListSigningJobs lists scoped job records with all modeled filters.
func (s *Service) ListSigningJobs(
	ctx context.Context,
	in *api.ListSigningJobsRequest,
) (*api.ListSigningJobsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	n, err := limit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if in.Status != nil {
		switch *in.Status {
		case "InProgress", "Succeeded", "Failed":
		default:
			return nil, invalid("Invalid signing job status.")
		}
	}

	rows := []api.SigningJob{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[job](tx, scope.Prefix(serviceName, "job"))
		if e != nil {
			return e
		}

		for _, r := range records {
			d := r.Detail
			if in.Status != nil && *in.Status != common.Value(d.Status) {
				continue
			}

			if in.PlatformId != nil && *in.PlatformId != common.Value(d.PlatformId) {
				continue
			}

			if in.RequestedBy != nil && *in.RequestedBy != common.Value(d.RequestedBy) {
				continue
			}

			if in.JobInvoker != nil && *in.JobInvoker != common.Value(d.JobInvoker) {
				continue
			}

			isRevoked := d.RevocationRecord != nil
			if in.IsRevoked != nil && *in.IsRevoked != isRevoked {
				continue
			}

			if in.SignatureExpiresBefore != nil &&
				(d.SignatureExpiresAt == nil || *d.SignatureExpiresAt >= *in.SignatureExpiresBefore) {
				continue
			}

			if in.SignatureExpiresAfter != nil &&
				(d.SignatureExpiresAt == nil || *d.SignatureExpiresAt <= *in.SignatureExpiresAfter) {
				continue
			}

			rows = append(rows, api.SigningJob{
				JobId: d.JobId, Source: d.Source, SignedObject: d.SignedObject,
				SigningMaterial: d.SigningMaterial, CreatedAt: d.CreatedAt, Status: d.Status, IsRevoked: new(isRevoked),
				ProfileName: d.ProfileName, ProfileVersion: d.ProfileVersion, PlatformId: d.PlatformId,
				PlatformDisplayName: d.PlatformDisplayName, SignatureExpiresAt: d.SignatureExpiresAt,
				JobOwner: d.JobOwner, JobInvoker: d.JobInvoker,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows,
		func(d api.SigningJob) string { return *d.JobId },
		in.NextToken,
		n,
		invalid("Invalid NextToken."),
	)
	return &api.ListSigningJobsResponse{Jobs: page, NextToken: next}, err
}

// RevokeSignature records revocation without modifying the stored S3 artifact.
func (s *Service) RevokeSignature(
	ctx context.Context,
	in *api.RevokeSignatureRequest,
) (*api.RevokeSignatureOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if ownerErr := owner(scope, in.JobOwner); ownerErr != nil {
		return nil, ownerErr
	}

	if common.Value(in.Reason) == "" {
		return nil, invalid("Reason is required.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readJob(tx, scope, in.JobId)
		if e != nil {
			return e
		}

		p, _ := platformByID(r.Detail.PlatformId)
		if !common.Value(p.RevocationSupported) {
			return invalid("Platform does not support revocation.")
		}

		if common.Value(r.Detail.Status) != "Succeeded" {
			return conflict()
		}

		r.Detail.RevocationRecord = &api.SigningJobRevocationRecord{
			Reason:    in.Reason,
			RevokedAt: common.Seconds(time.Now()), RevokedBy: new("arn:aws:iam::" + scope.AccountID + ":root"),
		}

		return storage.Write(tx, scope.Key(serviceName, "job", *in.JobId), r)
	})
	return &api.RevokeSignatureOutput{}, err
}
