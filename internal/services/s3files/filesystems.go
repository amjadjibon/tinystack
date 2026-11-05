package s3files

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3files"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	bucketARNPrefix = "arn:aws:s3:::"
	roleARNPrefix   = "arn:aws:iam::"
)

func fsResponse(scope request.Scope, r *fileSystem) *api.GetFileSystemResponse {
	out := &api.GetFileSystemResponse{
		CreationTime:  common.SecondsMilli(r.Created),
		FileSystemArn: new(fsARN(scope, r.ID)),
		FileSystemId:  new(r.ID),
		Bucket:        new(r.Bucket),
		Prefix:        new(r.Prefix),
		ClientToken:   new(r.ClientToken),
		Status:        new(r.State),
		RoleArn:       new(r.RoleARN),
		OwnerId:       new(scope.AccountID),
		Tags:          tagList(r.Tags),
		Name:          nameTag(r.Tags),
	}

	if r.KMSKeyID != "" {
		out.KmsKeyId = new(r.KMSKeyID)
	}

	return out
}

// bucketName returns the name in an S3 bucket ARN, which must name a bucket rather than an object.
func bucketName(arn string) (string, error) {
	name, ok := strings.CutPrefix(arn, bucketARNPrefix)
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", invalid("bucket must be the ARN of an S3 general purpose bucket in the aws partition")
	}

	return name, nil
}

// checkBucket requires the bucket to exist in the caller's account and Region. A bucket that does not keep object
// versions cannot synchronize changes, even if bucket warnings are acknowledged.
func (s *Service) checkBucket(
	ctx context.Context, scope request.Scope, name, prefix string, accept bool,
) error {
	if _, err := s.buckets.HeadBucket(
		ctx,
		&s3api.HeadBucketRequest{Bucket: new(name), ExpectedBucketOwner: new(scope.AccountID)},
	); err != nil {
		e, ok := errors.AsType[*awserror.Error](err)
		if ok && (e.Code == "NoSuchBucket" || e.Code == "AccessDenied" || e.Code == "PermanentRedirect") {
			return notFound("Bucket %s does not exist in this account and Region", name)
		}

		return err
	}

	versioning, err := s.buckets.GetBucketVersioning(ctx, &s3api.GetBucketVersioningRequest{Bucket: new(name)})
	if err != nil {
		return err
	}

	if common.Value(versioning.Status) != "Enabled" {
		return invalid("Bucket %s does not have S3 Versioning enabled, which S3 Files needs to synchronize changes; "+
			"enable versioning before creating the file system", name)
	}

	enc, err := s.buckets.GetBucketEncryption(ctx, &s3api.GetBucketEncryptionRequest{Bucket: new(name)})
	if err != nil {
		return err
	}

	for _, rule := range enc.ServerSideEncryptionConfiguration.Rules {
		if rule.ApplyServerSideEncryptionByDefault != nil {
			algorithm := common.Value(rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm)
			if algorithm != "AES256" && algorithm != "aws:kms" {
				return invalid("Bucket encryption must be SSE-S3 or SSE-KMS")
			}
		}
	}

	// AWS documents approximately 12 million objects as the prefix rename warning threshold:
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/s3-files-synchronization.html
	// Counting live objects performs no synchronization or mutation of the bucket.
	if prefix != "" && !accept {
		var token *string
		count := 0
		for {
			page, err := s.buckets.ListObjectsV2(ctx, &s3api.ListObjectsV2Request{
				Bucket: new(name), Prefix: new(prefix), ContinuationToken: token,
			})
			if err != nil {
				return err
			}

			count += len(page.Contents)
			if count >= 12_000_000 {
				return invalid(
					"Prefix contains at least 12 million objects; acknowledge rename costs with acceptBucketWarning",
				)
			}

			token = page.NextContinuationToken
			if token == nil {
				break
			}
		}
	}

	return nil
}

// checkRole requires an IAM role in the caller's account.
func (s *Service) checkRole(ctx context.Context, scope request.Scope, arn string) error {
	if !strings.HasPrefix(arn, roleARNPrefix+scope.AccountID+":") {
		return invalid("roleArn must name an IAM role in account %s", scope.AccountID)
	}

	_, found, err := s.roles.RoleByARN(ctx, arn)
	if err != nil {
		return err
	}

	if !found {
		return notFound("IAM role %s does not exist", arn)
	}

	return nil
}

// checkKey requires kmsKeyId to name a usable customer managed key in the caller's account and Region.
func (s *Service) checkKey(ctx context.Context, scope request.Scope, ref string) error {
	err := s.keys.Check(ctx, scope, ref)
	if e, ok := errors.AsType[*awserror.Error](err); ok {
		if e.Code == "NotFoundException" {
			return notFound("KMS key %s does not exist", ref)
		}

		return invalid("KMS key %s cannot be used: %s", ref, e.Message)
	}

	return err
}

func (s *Service) CreateFileSystem(
	ctx context.Context,
	in *api.CreateFileSystemRequest,
) (*api.CreateFileSystemResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	bucket, err := bucketName(common.Value(in.Bucket))
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	if bucketErr := s.checkBucket(
		ctx,
		scope,
		bucket,
		common.Value(in.Prefix),
		common.Value(in.AcceptBucketWarning),
	); bucketErr != nil {
		return nil, bucketErr
	}

	if roleErr := s.checkRole(ctx, scope, common.Value(in.RoleArn)); roleErr != nil {
		return nil, roleErr
	}

	if in.KmsKeyId != nil {
		if keyErr := s.checkKey(ctx, scope, *in.KmsKeyId); keyErr != nil {
			return nil, keyErr
		}
	}

	token := newToken(in.ClientToken)
	input := struct {
		Bucket, Prefix, KMSKeyID, RoleARN *string
		Tags                              map[string]string
	}{in.Bucket, in.Prefix, in.KmsKeyId, in.RoleArn, tags}

	var out *api.GetFileSystemResponse
	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := idempotency.Key(scope, serviceName, "CreateFileSystem", new(token))
		previous, found, replayErr := idempotency.Replay[*api.GetFileSystemResponse](
			tx,
			key,
			input,
			conflict("Client token %s was already used with different parameters", token),
		)
		if replayErr != nil {
			return replayErr
		}

		if found {
			out = previous
			return nil
		}

		r := &fileSystem{
			ID:          "fs-" + common.RandomID(17, "0123456789abcdef"),
			Bucket:      common.Value(in.Bucket),
			Prefix:      common.Value(in.Prefix),
			ClientToken: token,
			KMSKeyID:    common.Value(in.KmsKeyId),
			RoleARN:     common.Value(in.RoleArn),
			Tags:        tags,
			State:       creating,
			Created:     time.Now().UTC(),
			Due:         time.Now().UTC().Add(transitionDelay),
		}

		out = fsResponse(scope, r)

		if writeErr := queue(tx, scope, kindFS, r.ID, r); writeErr != nil {
			return writeErr
		}

		return idempotency.Remember(tx, key, input, out)
	})
	if err != nil {
		return nil, err
	}

	return new(api.CreateFileSystemResponse(*out)), nil
}

func (s *Service) GetFileSystem(ctx context.Context, in *api.GetFileSystemRequest) (*api.GetFileSystemResponse, error) {
	var out *api.GetFileSystemResponse
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		r, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		out = fsResponse(scope, r)
		return nil
	})

	return out, err
}

func (s *Service) ListFileSystems(
	ctx context.Context,
	in *api.ListFileSystemsRequest,
) (*api.ListFileSystemsResponse, error) {
	out := &api.ListFileSystemsResponse{FileSystems: []api.ListFileSystemsDescription{}}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		all, e := storage.Records[fileSystem](tx, scope.Prefix(serviceName, kindFS))
		if e != nil {
			return e
		}

		if in.Bucket != nil {
			all = slices.DeleteFunc(all, func(r *fileSystem) bool { return r.Bucket != *in.Bucket })
		}

		all, out.NextToken, e = common.Page(all, func(r *fileSystem) string { return r.ID }, in.NextToken,
			pageLimit(in.MaxResults, listPageSize), errBadToken)
		if e != nil {
			return e
		}

		for _, r := range all {
			out.FileSystems = append(out.FileSystems, api.ListFileSystemsDescription{
				CreationTime:  common.SecondsMilli(r.Created),
				FileSystemArn: new(fsARN(scope, r.ID)),
				FileSystemId:  new(r.ID),
				Name:          nameTag(r.Tags),
				Bucket:        new(r.Bucket),
				Status:        new(r.State),
				RoleArn:       new(r.RoleARN),
				OwnerId:       new(scope.AccountID),
			})
		}

		return nil
	})

	return out, err
}

// DeleteFileSystem refuses while mount targets or access points remain. forceDelete only overrides pending export
// data, and a local file system never holds any.
func (s *Service) DeleteFileSystem(
	ctx context.Context,
	in *api.DeleteFileSystemRequest,
) (*api.DeleteFileSystemOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, e := readFS(tx, scope, common.Value(in.FileSystemId))
		if e != nil {
			return e
		}

		if r.State == deleting {
			return nil
		}

		mounts, e := storage.Records[mountTarget](tx, scope.Prefix(serviceName, kindMT))
		if e != nil {
			return e
		}

		points, e := storage.Records[accessPoint](tx, scope.Prefix(serviceName, kindAP))
		if e != nil {
			return e
		}

		if slices.ContainsFunc(mounts, func(m *mountTarget) bool { return m.FileSystemID == r.ID }) {
			return conflict("File system %s still has mount targets; delete them first", r.ID)
		}

		if slices.ContainsFunc(points, func(p *accessPoint) bool { return p.FileSystemID == r.ID }) {
			return conflict("File system %s still has access points; delete them first", r.ID)
		}

		r.State = deleting
		r.Due = time.Now().UTC().Add(transitionDelay)

		return queue(tx, scope, kindFS, r.ID, r)
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteFileSystemOutput{}, nil
}
