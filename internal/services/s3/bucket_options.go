package s3

import (
	"cmp"
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) GetBucketAccelerateConfiguration(
	ctx context.Context,
	in *api.GetBucketAccelerateConfigurationRequest,
) (*api.GetBucketAccelerateConfigurationOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	out := &api.GetBucketAccelerateConfigurationOutput{}
	if meta.Acceleration != "" {
		out.Status = new(meta.Acceleration)
	}

	return out, nil
}

func (s *Service) PutBucketAccelerateConfiguration(
	ctx context.Context,
	in *api.PutBucketAccelerateConfigurationRequest,
) (*api.PutBucketAccelerateConfigurationOutput, error) {
	if in.AccelerateConfiguration == nil {
		return nil, malformedXML()
	}

	status := common.Value(in.AccelerateConfiguration.Status)
	if status != configurationEnabled && status != versioningSuspended {
		return nil, malformedXML()
	}

	if status == "Enabled" && strings.Contains(common.Value(in.Bucket), ".") {
		return nil, invalidArgument("Buckets with periods in their names cannot enable transfer acceleration")
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Acceleration = status
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketAccelerateConfigurationOutput{}, nil
}

func (s *Service) GetBucketRequestPayment(
	ctx context.Context,
	in *api.GetBucketRequestPaymentRequest,
) (*api.GetBucketRequestPaymentOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	return &api.GetBucketRequestPaymentOutput{Payer: new(cmp.Or(meta.Payer, "BucketOwner"))}, nil
}

func (s *Service) PutBucketRequestPayment(
	ctx context.Context,
	in *api.PutBucketRequestPaymentRequest,
) (*api.PutBucketRequestPaymentOutput, error) {
	if in.RequestPaymentConfiguration == nil {
		return nil, malformedXML()
	}

	payer := common.Value(in.RequestPaymentConfiguration.Payer)
	if payer != "Requester" && payer != "BucketOwner" {
		return nil, malformedXML()
	}

	err := s.updateBucket(ctx, in.Bucket, in.ExpectedBucketOwner, func(meta *bucketMeta) {
		meta.Payer = payer
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketRequestPaymentOutput{}, nil
}
