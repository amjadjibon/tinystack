package s3

import (
	"context"
	"net/http"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
)

const sseS3 = "AES256"

// defaultEncryption is the configuration of a bucket that sets none: S3
// encrypts every object with S3 managed keys.
func defaultEncryption() *api.ServerSideEncryptionConfiguration {
	return &api.ServerSideEncryptionConfiguration{Rules: []api.ServerSideEncryptionRule{{
		ApplyServerSideEncryptionByDefault: &api.ServerSideEncryptionByDefault{SSEAlgorithm: new(sseS3)},
		BucketKeyEnabled:                   new(false),
	}}}
}

// checkEncryption validates a default encryption configuration: one rule
// naming an algorithm, with a KMS key only for KMS encryption.
func checkEncryption(cfg *api.ServerSideEncryptionConfiguration) error {
	malformed := fail(http.StatusBadRequest, "MalformedXML",
		"The XML you provided was not well-formed or did not validate against our published schema")
	if cfg == nil || len(cfg.Rules) != 1 || cfg.Rules[0].ApplyServerSideEncryptionByDefault == nil {
		return malformed
	}

	d := cfg.Rules[0].ApplyServerSideEncryptionByDefault
	switch value(d.SSEAlgorithm) {
	case sseS3:
		if d.KMSMasterKeyID != nil {
			return fail(http.StatusBadRequest, "InvalidArgument",
				"a KMSMasterKeyID is not applicable if the default sse algorithm is not aws:kms or aws:kms:dsse")
		}
	case "aws:kms", "aws:kms:dsse":
	default:
		return malformed
	}

	return nil
}

func (s *Service) PutBucketEncryption(
	ctx context.Context,
	in *api.PutBucketEncryptionRequest,
) (*api.PutBucketEncryptionOutput, error) {
	if err := checkEncryption(in.ServerSideEncryptionConfiguration); err != nil {
		return nil, err
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Encryption = in.ServerSideEncryptionConfiguration
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketEncryptionOutput{}, nil
}

// GetBucketEncryption returns the bucket's default encryption, which is SSE-S3
// unless the bucket sets another.
func (s *Service) GetBucketEncryption(
	ctx context.Context,
	in *api.GetBucketEncryptionRequest,
) (*api.GetBucketEncryptionOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	cfg := meta.Encryption
	if cfg == nil {
		cfg = defaultEncryption()
	}

	return &api.GetBucketEncryptionOutput{ServerSideEncryptionConfiguration: cfg}, nil
}

// DeleteBucketEncryption returns the bucket to SSE-S3.
func (s *Service) DeleteBucketEncryption(
	ctx context.Context,
	in *api.DeleteBucketEncryptionRequest,
) (*api.DeleteBucketEncryptionOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Encryption = nil })
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketEncryptionOutput{}, nil
}
