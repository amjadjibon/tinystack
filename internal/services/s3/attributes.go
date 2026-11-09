package s3

import (
	"context"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) GetObjectAttributes(ctx context.Context,
	in *api.GetObjectAttributesRequest,
) (*api.GetObjectAttributesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if guardErr := validKey(in.Key); guardErr != nil {
		return nil, guardErr
	}

	if len(in.ObjectAttributes) == 0 {
		return nil, invalidArgument("ObjectAttributes is required")
	}

	for _, attribute := range in.ObjectAttributes {
		switch attribute {
		case "ETag", "Checksum", "ObjectParts", "StorageClass", "ObjectSize":
		default:
			return nil, invalidArgument("Invalid object attribute")
		}
	}

	limit := int64(1000)
	if in.MaxParts != nil {
		limit = *in.MaxParts
	}

	marker := int64(0)
	if in.PartNumberMarker != nil {
		marker, err = strconv.ParseInt(*in.PartNumberMarker, 10, 64)
		if err != nil {
			return nil, invalidArgument("Invalid part-number-marker")
		}
	}
	if limit < 0 || limit > 1000 || marker < 0 || marker > 10000 {
		return nil, invalidArgument("Invalid max-parts or part-number-marker")
	}

	out := &api.GetObjectAttributesOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		bucket, transactionErr := resolve(tx, m, in.Bucket, in.ExpectedBucketOwner)
		if transactionErr != nil {
			return transactionErr
		}

		obj, _, transactionErr := locate(tx, bucket, *in.Key, common.Value(in.VersionId))
		if transactionErr != nil {
			return transactionErr
		}

		status, transactionErr := versioning(tx, bucket)
		if transactionErr != nil {
			return transactionErr
		}

		out.VersionId = reported(status, &obj)
		out.LastModified = new(time.UnixMilli(obj.LastModified).UTC())
		for _, attribute := range in.ObjectAttributes {
			switch attribute {
			case "ETag":
				out.ETag = new(strings.Trim(obj.ETag, `"`))
			case "Checksum":
				if obj.Checksum != nil {
					out.Checksum = &api.Checksum{}
					obj.Checksum.apply(out.Checksum)
				}
			case "StorageClass":
				out.StorageClass = new(storageStandard)
			case "ObjectSize":
				out.ObjectSize = new(obj.Size)
			case "ObjectParts":
				if len(obj.Parts) == 0 {
					continue
				}

				parts := &api.GetObjectAttributesParts{
					TotalPartsCount: new(int64(len(obj.Parts))),
					MaxParts: new(
						limit,
					),
					PartNumberMarker: new(strconv.FormatInt(marker, 10)),
					IsTruncated:      new(false),
				}
				for _, part := range obj.Parts {
					if common.Value(part.PartNumber) <= marker {
						continue
					}

					if int64(len(parts.Parts)) == limit {
						parts.IsTruncated = new(true)
						break
					}

					parts.Parts = append(parts.Parts, part)
					parts.NextPartNumberMarker = new(strconv.FormatInt(*part.PartNumber, 10))
				}

				out.ObjectParts = parts
			}
		}

		return nil
	})
	return out, err
}
