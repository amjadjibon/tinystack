package cloudfront

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const kindKVS = "key-value-store"

// KVSRecord is owned by CloudFront. Both APIs access it only inside KVSTransaction;
// data-plane keys are separate records under KVSKeysPrefix in the same transaction.
type KVSRecord struct {
	Store         api.KeyValueStore
	Tags          map[string]string
	ETag          string
	DataETag      string
	Created       time.Time
	FailureReason string
	TargetStatus  string
}

// KVSMaxBytes is CloudFront's documented 5 MiB total key/value size limit.
// https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/cloudfront-limits.html#limits-keyvaluestores
const KVSMaxBytes = 5 * 1024 * 1024

// UseS3 connects local import sources before requests are served.
func (s *Service) UseS3(buckets *s3.Service) { s.buckets = buckets }

// KVSKeysPrefix returns the key-record namespace owned by a store.
func KVSKeysPrefix(account, id string) string { return key(account, "kvs-key", id+"/") }

// KVSKey is a persisted UTF-8 key/value pair, shared by import and the data plane.
type KVSKey struct {
	Key   string
	Value string
}

// CheckKVSLimits checks byte limits that the Smithy character constraints do not express.
func CheckKVSLimits(k, v string) error {
	if len(k) > 512 || len(v) > 1024 {
		return awserror.New("ValidationException", "Keys are limited to 512 bytes and values to 1024 bytes")
	}
	return nil
}

func readKVS(tx storage.WriteTx, account, name string) (*KVSRecord, error) {
	r, ok, err := storage.Read[KVSRecord](tx, key(account, kindKVS, name))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, awserror.New("EntityNotFound", "Key value store does not exist")
	}
	if common.Value(r.Store.Status) == "PROVISIONING" {
		r.Store.Status = new(r.TargetStatus)
		if err := storage.Write(tx, key(account, kindKVS, name), r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// KVSTransaction resolves an existing account-owned ARN and provides one atomic
// transaction for metadata and keys. No other service owns or duplicates the store.
func (s *Service) KVSTransaction(
	ctx context.Context,
	arn string,
	fn func(storage.WriteTx, string, *KVSRecord) error,
) error {
	m, err := request.Global(ctx)
	if err != nil {
		return err
	}
	prefix := fmt.Sprintf("arn:aws:cloudfront::%s:key-value-store/", m.AccountID)
	id, ok := strings.CutPrefix(arn, prefix)
	if !ok || id == "" {
		return awserror.New("ResourceNotFoundException", "Key value store does not exist")
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		b, found := tx.Get(key(m.AccountID, "kvs-id", id))
		if !found {
			return awserror.New("ResourceNotFoundException", "Key value store does not exist")
		}
		r, err := readKVS(tx, m.AccountID, string(b))
		if err != nil {
			return err
		}
		if err := fn(tx, KVSKeysPrefix(m.AccountID, id), r); err != nil {
			return err
		}
		return storage.Write(tx, key(m.AccountID, kindKVS, string(b)), r)
	})
}

func (s *Service) importKVS(ctx context.Context, source *api.ImportSource) ([]KVSKey, string, error) {
	if source == nil {
		return nil, "", nil
	}
	if common.Value(source.SourceType) != "S3" {
		return nil, "", invalidArgument("Only S3 import sources are supported")
	}
	path, ok := strings.CutPrefix(common.Value(source.SourceARN), "arn:aws:s3:::")
	bucket, object, found := strings.Cut(path, "/")
	if !ok || !found || bucket == "" || object == "" {
		return nil, "", invalidArgument("ImportSource must name an S3 object")
	}
	if s.buckets == nil {
		return nil, "", invalidArgument("S3 imports are unavailable")
	}
	body, err := s.buckets.Object(ctx, bucket, object, "")
	if err != nil {
		return nil, "", invalidArgument("Import source does not exist in TinyStack S3: " + err.Error())
	}
	if len(body) > KVSMaxBytes {
		return nil, "Import file exceeds 5 MiB", nil
	}
	var doc struct {
		Data *[]struct {
			Key   *string `json:"key"`
			Value *string `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Data == nil {
		return nil, "Import must be a JSON object containing a data array", nil
	}
	seen := map[string]bool{}
	var items []KVSKey
	size := 0
	for _, p := range *doc.Data {
		if p.Key == nil || p.Value == nil || *p.Key == "" {
			return nil, "Import entries require key and value strings", nil
		}
		if seen[*p.Key] {
			return nil, "Import contains duplicate keys", nil
		}
		if err := CheckKVSLimits(*p.Key, *p.Value); err != nil {
			return nil, err.Error(), nil
		}
		seen[*p.Key] = true
		size += len(*p.Key) + len(*p.Value)
		items = append(items, KVSKey{Key: *p.Key, Value: *p.Value})
	}
	if size > KVSMaxBytes {
		return nil, "Key value store exceeds 5 MiB", nil
	}
	return items, "", nil
}

func (s *Service) CreateKeyValueStore(
	ctx context.Context,
	in *api.CreateKeyValueStoreRequest,
) (*api.CreateKeyValueStoreResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := kvsTags(in.Tags)
	if err != nil {
		return nil, err
	}
	items, reason, err := s.importKVS(ctx, in.ImportSource)
	if err != nil {
		return nil, err
	}
	id := uuid.New().String()
	now := s.now().UTC()
	r := KVSRecord{
		Store: api.KeyValueStore{
			Name:             in.Name,
			Id:               new(id),
			ARN:              new(fmt.Sprintf("arn:aws:cloudfront::%s:key-value-store/%s", m.AccountID, id)),
			Comment:          common.OrElse(in.Comment, new("")),
			Status:           new("PROVISIONING"),
			LastModifiedTime: new(now),
		},
		Tags:          tags,
		ETag:          newID("E"),
		DataETag:      newID("KV"),
		Created:       now,
		TargetStatus:  "READY",
		FailureReason: reason,
	}
	if reason != "" {
		r.TargetStatus = "IMPORT_FAILED"
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(m.AccountID, kindKVS, *in.Name)
		if _, ok := tx.Get(k); ok {
			return awserror.New("EntityAlreadyExists", "Key value store name already exists")
		}
		if err := storage.Write(tx, k, r); err != nil {
			return err
		}
		tx.Put(key(m.AccountID, "kvs-id", id), []byte(*in.Name))
		for _, p := range items {
			if err := storage.Write(tx, KVSKeysPrefix(m.AccountID, id)+p.Key, p); err != nil {
				return err
			}
		}
		return nil
	})
	return &api.CreateKeyValueStoreResult{
		KeyValueStore: &r.Store,
		ETag:          new(r.ETag),
		Location:      new("https://cloudfront.amazonaws.com/2020-05-31/key-value-store/" + *r.Store.ARN),
	}, err
}

func (s *Service) DescribeKeyValueStore(
	ctx context.Context,
	in *api.DescribeKeyValueStoreRequest,
) (*api.DescribeKeyValueStoreResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}
	var r *KVSRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = readKVS(tx, m.AccountID, common.Value(in.Name))
		return e
	})
	if err != nil {
		return nil, err
	}
	return &api.DescribeKeyValueStoreResult{KeyValueStore: &r.Store, ETag: new(r.ETag)}, nil
}

func kvsMatch(match *string, etag string) error {
	if common.Value(match) == "" {
		return invalidIfMatchVersion()
	}
	if *match != etag {
		return &awserror.Error{
			Code:       "PreconditionFailed",
			Message:    "The If-Match version does not match",
			StatusCode: 412,
		}
	}
	return nil
}

func (s *Service) UpdateKeyValueStore(
	ctx context.Context,
	in *api.UpdateKeyValueStoreRequest,
) (*api.UpdateKeyValueStoreResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}
	var r *KVSRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = readKVS(tx, m.AccountID, common.Value(in.Name))
		if e != nil {
			return e
		}
		if e := kvsMatch(in.IfMatch, r.ETag); e != nil {
			return e
		}
		r.Store.Comment = in.Comment
		r.Store.LastModifiedTime = new(s.now().UTC())
		r.ETag = newID("E")
		return storage.Write(tx, key(m.AccountID, kindKVS, *in.Name), r)
	})
	if err != nil {
		return nil, err
	}
	return &api.UpdateKeyValueStoreResult{KeyValueStore: &r.Store, ETag: new(r.ETag)}, nil
}

func (s *Service) DeleteKeyValueStore(
	ctx context.Context,
	in *api.DeleteKeyValueStoreRequest,
) (*api.DeleteKeyValueStoreOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readKVS(tx, m.AccountID, common.Value(in.Name))
		if e != nil {
			return e
		}
		if e := kvsMatch(in.IfMatch, r.ETag); e != nil {
			return e
		}
		for _, k := range tx.Keys(KVSKeysPrefix(m.AccountID, *r.Store.Id)) {
			tx.Delete(k)
		}
		tx.Delete(key(m.AccountID, "kvs-id", *r.Store.Id))
		tx.Delete(key(m.AccountID, kindKVS, *in.Name))
		return nil
	})
	return &api.DeleteKeyValueStoreOutput{}, err
}

func (s *Service) ListKeyValueStores(
	ctx context.Context,
	in *api.ListKeyValueStoresRequest,
) (*api.ListKeyValueStoresResult, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}
	if in.MaxItems != nil && *in.MaxItems <= 0 {
		return nil, invalidArgument("MaxItems must be positive")
	}
	if in.Status != nil && *in.Status != "READY" && *in.Status != "PROVISIONING" && *in.Status != "IMPORT_FAILED" {
		return nil, invalidArgument("Invalid key value store status")
	}
	var items []api.KeyValueStore
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(key(m.AccountID, kindKVS, "")) {
			r, e := readKVS(tx, m.AccountID, strings.TrimPrefix(k, key(m.AccountID, kindKVS, "")))
			if e != nil {
				return e
			}
			if in.Status == nil || *in.Status == common.Value(r.Store.Status) {
				items = append(items, r.Store)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	limit := common.Value(common.OrElse(in.MaxItems, new(int64(100))))
	page, next, err := common.Page(
		items,
		func(v api.KeyValueStore) string { return common.Value(v.Name) },
		in.Marker,
		int(limit),
		invalidArgument("Invalid Marker"),
	)
	return &api.ListKeyValueStoresResult{
		KeyValueStoreList: &api.KeyValueStoreList{
			Items:      page,
			NextMarker: next,
			MaxItems:   new(limit),
			Quantity:   new(int64(len(page))),
		},
	}, err
}
