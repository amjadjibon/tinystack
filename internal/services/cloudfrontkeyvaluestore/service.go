// Package cloudfrontkeyvaluestore implements local key/value access to stores
// owned by the CloudFront control plane, with atomic ETag-conditional writes.
package cloudfrontkeyvaluestore

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfrontkeyvaluestore"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/cloudfront"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Service accesses CloudFront's records through its transactional owner API.
type Service struct{ owner *cloudfront.Service }

// New constructs the data plane for an existing CloudFront control plane.
func New(owner *cloudfront.Service) *Service { return &Service{owner: owner} }

func stats(tx storage.ReadTx, prefix string) (count, size int64, err error) {
	records, err := storage.Records[cloudfront.KVSKey](tx, prefix)
	if err != nil {
		return 0, 0, err
	}

	for _, p := range records {
		size += int64(len(p.Key) + len(p.Value))
	}

	return int64(len(records)), size, nil
}

func ready(r *cloudfront.KVSRecord) error {
	if common.Value(r.Store.Status) != "READY" {
		return awserror.New("ConflictException", "Key value store is not READY")
	}

	return nil
}

func (s *Service) DescribeKeyValueStore(
	ctx context.Context,
	in *api.DescribeKeyValueStoreRequest,
) (*api.DescribeKeyValueStoreResponse, error) {
	out := &api.DescribeKeyValueStoreResponse{}
	err := s.owner.KVSTransaction(
		ctx,
		common.Value(in.KvsARN),
		func(tx storage.WriteTx, prefix string, r *cloudfront.KVSRecord) error {
			count, size, e := stats(tx, prefix)
			if e != nil {
				return e
			}

			status := common.Value(r.Store.Status)
			// The data-plane model calls the control plane's IMPORT_FAILED state IMPORT_FAILURE.
			if status == "IMPORT_FAILED" {
				status = "IMPORT_FAILURE"
			}

			out.KvsARN = in.KvsARN
			out.ETag = new(r.DataETag)
			out.Status = new(status)
			out.ItemCount = new(count)
			out.TotalSizeInBytes = new(size)
			out.Created = new(float64(r.Created.UnixMilli()) / 1000)
			out.LastModified = new(float64(r.DataModified.UnixMilli()) / 1000)
			if r.FailureReason != "" {
				out.FailureReason = new(r.FailureReason)
			}

			return nil
		},
	)
	return out, err
}

func (s *Service) GetKey(ctx context.Context, in *api.GetKeyRequest) (*api.GetKeyResponse, error) {
	if err := cloudfront.CheckKVSLimits(common.Value(in.Key), ""); err != nil {
		return nil, err
	}

	out := &api.GetKeyResponse{}
	err := s.owner.KVSTransaction(
		ctx,
		common.Value(in.KvsARN),
		func(tx storage.WriteTx, prefix string, r *cloudfront.KVSRecord) error {
			if err := ready(r); err != nil {
				return err
			}

			pair, ok, e := storage.Read[cloudfront.KVSKey](tx, prefix+common.Value(in.Key))
			if e != nil {
				return e
			}

			if !ok {
				return awserror.New("ResourceNotFoundException", "Key does not exist")
			}

			count, size, e := stats(tx, prefix)
			if e != nil {
				return e
			}

			out.Key = new(pair.Key)
			out.Value = new(pair.Value)
			out.ItemCount = new(count)
			out.TotalSizeInBytes = new(size)
			return nil
		},
	)
	return out, err
}

func (s *Service) ListKeys(ctx context.Context, in *api.ListKeysRequest) (*api.ListKeysResponse, error) {
	out := &api.ListKeysResponse{}
	err := s.owner.KVSTransaction(
		ctx,
		common.Value(in.KvsARN),
		func(tx storage.WriteTx, prefix string, r *cloudfront.KVSRecord) error {
			if err := ready(r); err != nil {
				return err
			}

			records, e := storage.Records[cloudfront.KVSKey](tx, prefix)
			if e != nil {
				return e
			}

			page, next, e := common.Page(
				records,
				func(p *cloudfront.KVSKey) string { return p.Key },
				in.NextToken,
				int(common.Value(common.OrElse(in.MaxResults, new(int64(50))))),
				awserror.New("ValidationException", "Invalid NextToken"),
			)
			if e != nil {
				return e
			}

			for _, p := range page {
				out.Items = append(out.Items, api.ListKeysResponseListItem{Key: new(p.Key), Value: new(p.Value)})
			}

			out.NextToken = next
			return nil
		},
	)
	return out, err
}

func (s *Service) mutate(
	ctx context.Context,
	arn, match *string,
	puts []api.PutKeyRequestListItem,
	deletes []api.DeleteKeyRequestListItem,
) (*api.UpdateKeysResponse, error) {
	// Quotas: at most 50 keys, 512-byte keys, 1 KiB values, 5 MiB per store.
	// https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/cloudfront-limits.html#limits-keyvaluestores
	if len(puts)+len(deletes) > 50 {
		return nil, awserror.New("ServiceQuotaExceededException", "At most 50 keys can be updated per request")
	}

	for _, p := range puts {
		if err := cloudfront.CheckKVSLimits(common.Value(p.Key), common.Value(p.Value)); err != nil {
			return nil, err
		}
	}

	for _, p := range deletes {
		if err := cloudfront.CheckKVSLimits(common.Value(p.Key), ""); err != nil {
			return nil, err
		}
	}

	out := &api.UpdateKeysResponse{}
	err := s.owner.KVSTransaction(
		ctx,
		common.Value(arn),
		func(tx storage.WriteTx, prefix string, r *cloudfront.KVSRecord) error {
			if err := ready(r); err != nil {
				return err
			}

			if common.Value(match) != r.DataETag {
				return awserror.New("ConflictException", "The If-Match version does not match")
			}

			for _, p := range deletes {
				tx.Delete(prefix + common.Value(p.Key))
			}

			for _, p := range puts {
				pair := cloudfront.KVSKey{Key: common.Value(p.Key), Value: common.Value(p.Value)}
				if err := storage.Write(tx, prefix+pair.Key, pair); err != nil {
					return err
				}
			}

			count, size, e := stats(tx, prefix)
			if e != nil {
				return e
			}

			if size > cloudfront.KVSMaxBytes {
				return awserror.New("ServiceQuotaExceededException", "Key value store exceeds 5 MiB")
			}

			r.DataModified = time.Now().UTC()
			r.DataETag = "KV" + common.RandomID(16, common.AlphabetUpperHex)
			out.ETag = new(r.DataETag)
			out.ItemCount = new(count)
			out.TotalSizeInBytes = new(size)
			return nil
		},
	)
	return out, err
}

func (s *Service) PutKey(ctx context.Context, in *api.PutKeyRequest) (*api.PutKeyResponse, error) {
	out, err := s.mutate(ctx, in.KvsARN, in.IfMatch, []api.PutKeyRequestListItem{{Key: in.Key, Value: in.Value}}, nil)
	if err != nil {
		return nil, err
	}

	return &api.PutKeyResponse{ETag: out.ETag, ItemCount: out.ItemCount, TotalSizeInBytes: out.TotalSizeInBytes}, nil
}

func (s *Service) DeleteKey(ctx context.Context, in *api.DeleteKeyRequest) (*api.DeleteKeyResponse, error) {
	out, err := s.mutate(ctx, in.KvsARN, in.IfMatch, nil, []api.DeleteKeyRequestListItem{{Key: in.Key}})
	if err != nil {
		return nil, err
	}

	return &api.DeleteKeyResponse{ETag: out.ETag, ItemCount: out.ItemCount, TotalSizeInBytes: out.TotalSizeInBytes}, nil
}

func (s *Service) UpdateKeys(ctx context.Context, in *api.UpdateKeysRequest) (*api.UpdateKeysResponse, error) {
	return s.mutate(ctx, in.KvsARN, in.IfMatch, in.Puts, in.Deletes)
}

var _ api.API = (*Service)(nil)
