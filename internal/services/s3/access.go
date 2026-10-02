package s3

import (
	"context"
	"net/http"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
)

const ownerEnforced = "BucketOwnerEnforced"

var ownerships = map[string]bool{ownerEnforced: true, "BucketOwnerPreferred": true, "ObjectWriter": true}

// newBucketAccess is the access configuration S3 gives a new bucket: ACLs
// disabled and every public access setting on.
func newBucketAccess(meta *bucketMeta) {
	meta.Ownership = ownerEnforced
	meta.PublicAccessBlock = &api.PublicAccessBlockConfiguration{
		BlockPublicAcls: new(true), IgnorePublicAcls: new(true), BlockPublicPolicy: new(true),
		RestrictPublicBuckets: new(true),
	}
}

func (s *Service) PutBucketOwnershipControls(
	ctx context.Context,
	in *api.PutBucketOwnershipControlsRequest,
) (*api.PutBucketOwnershipControlsOutput, error) {
	c := in.OwnershipControls
	if c == nil || len(c.Rules) != 1 || !ownerships[value(c.Rules[0].ObjectOwnership)] {
		return nil, fail(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema")
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Ownership = *c.Rules[0].ObjectOwnership
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketOwnershipControlsOutput{}, nil
}

func (s *Service) GetBucketOwnershipControls(
	ctx context.Context,
	in *api.GetBucketOwnershipControlsRequest,
) (*api.GetBucketOwnershipControlsOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if meta.Ownership == "" {
		return nil, fail(http.StatusNotFound, "OwnershipControlsNotFoundError",
			"The bucket ownership controls were not found")
	}

	return &api.GetBucketOwnershipControlsOutput{OwnershipControls: &api.OwnershipControls{
		Rules: []api.OwnershipControlsRule{{ObjectOwnership: new(meta.Ownership)}},
	}}, nil
}

func (s *Service) DeleteBucketOwnershipControls(
	ctx context.Context,
	in *api.DeleteBucketOwnershipControlsRequest,
) (*api.DeleteBucketOwnershipControlsOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) { meta.Ownership = "" })
	if err != nil {
		return nil, err
	}

	return &api.DeleteBucketOwnershipControlsOutput{}, nil
}

// PutPublicAccessBlock stores the bucket's public access settings; a setting
// left out is off. BlockPublicPolicy rejects public bucket policies, the only
// public access TinyStack has.
func (s *Service) PutPublicAccessBlock(
	ctx context.Context,
	in *api.PutPublicAccessBlockRequest,
) (*api.PutPublicAccessBlockOutput, error) {
	c := in.PublicAccessBlockConfiguration
	if c == nil {
		return nil, fail(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema")
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.PublicAccessBlock = &api.PublicAccessBlockConfiguration{
			BlockPublicAcls: new(value(c.BlockPublicAcls)), IgnorePublicAcls: new(value(c.IgnorePublicAcls)),
			BlockPublicPolicy:     new(value(c.BlockPublicPolicy)),
			RestrictPublicBuckets: new(value(c.RestrictPublicBuckets)),
		}
	})
	if err != nil {
		return nil, err
	}

	return &api.PutPublicAccessBlockOutput{}, nil
}

func (s *Service) GetPublicAccessBlock(
	ctx context.Context,
	in *api.GetPublicAccessBlockRequest,
) (*api.GetPublicAccessBlockOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	if meta.PublicAccessBlock == nil {
		return nil, fail(http.StatusNotFound, "NoSuchPublicAccessBlockConfiguration",
			"The public access block configuration was not found")
	}

	return &api.GetPublicAccessBlockOutput{PublicAccessBlockConfiguration: meta.PublicAccessBlock}, nil
}

func (s *Service) DeletePublicAccessBlock(
	ctx context.Context,
	in *api.DeletePublicAccessBlockRequest,
) (*api.DeletePublicAccessBlockOutput, error) {
	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.PublicAccessBlock = nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeletePublicAccessBlockOutput{}, nil
}
