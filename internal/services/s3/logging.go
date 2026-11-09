package s3

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// PutBucketLogging stores configuration only; no access logs are delivered.
func (s *Service) PutBucketLogging(ctx context.Context,
	in *api.PutBucketLoggingRequest,
) (*api.PutBucketLoggingOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if in.BucketLoggingStatus == nil {
		return nil, malformedXML()
	}

	cfg := in.BucketLoggingStatus.LoggingEnabled
	if cfg != nil {
		if common.Value(cfg.TargetBucket) == "" || cfg.TargetPrefix == nil {
			return nil, malformedXML()
		}

		if format := cfg.TargetObjectKeyFormat; format != nil {
			if (format.SimplePrefix == nil) == (format.PartitionedPrefix == nil) {
				return nil, malformedXML()
			}

			if format.PartitionedPrefix != nil {
				switch common.Value(format.PartitionedPrefix.PartitionDateSource) {
				case "EventTime", "DeliveryTime":
				default:
					return nil, malformedXML()
				}
			}
		}

		for _, grant := range cfg.TargetGrants {
			if grant.Grantee == nil {
				return nil, malformedXML()
			}

			switch common.Value(grant.Permission) {
			case "FULL_CONTROL", "READ", "WRITE":
			default:
				return nil, malformedXML()
			}
		}
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		bucket, transactionErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if transactionErr != nil {
			return transactionErr
		}

		if cfg != nil {
			target, found, targetErr := storage.Read[bucketIndex](tx, indexPrefix+*cfg.TargetBucket)
			if targetErr != nil {
				return targetErr
			}

			owner, _, _ := strings.Cut(bucket, "/")

			if !found || target.Account != owner || target.Region != m.Region {
				return fail(
					400,
					"InvalidTargetBucketForLogging",
					"The target bucket must exist in the same account and region",
				)
			}
		}

		meta, _, transactionErr := storage.Read[bucketMeta](tx, bucket)
		if transactionErr != nil {
			return transactionErr
		}

		meta.Logging = cfg
		return storage.Write(tx, bucket, meta)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutBucketLoggingOutput{}, nil
}

func (s *Service) GetBucketLogging(ctx context.Context,
	in *api.GetBucketLoggingRequest,
) (*api.GetBucketLoggingOutput, error) {
	meta, err := s.bucket(ctx, in.Bucket, in.ExpectedBucketOwner)
	if err != nil {
		return nil, err
	}

	return &api.GetBucketLoggingOutput{LoggingEnabled: meta.Logging}, nil
}
