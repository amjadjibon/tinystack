package dynamodb

import (
	"context"
	"maps"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	maxBatchGet      = 100
	maxBatchWrite    = 25
	maxBatchResponse = 16 << 20
)

func duplicateKeys() error {
	return awserror.Validation("Provided list of item keys contains duplicates")
}

// BatchGetItem reads up to 100 items from one or more tables in one snapshot.
// Keys past the 16 MB response limit come back in UnprocessedKeys.
func (s *Service) BatchGetItem(ctx context.Context, in *api.BatchGetItemInput) (*api.BatchGetItemOutput, error) {
	var used tally
	return observed(ctx, s, "BatchGetItem", &used, func() (*api.BatchGetItemOutput, error) {
		return s.batchGetItem(ctx, in, &used)
	})
}

func (s *Service) batchGetItem(
	ctx context.Context,
	in *api.BatchGetItemInput,
	used *tally,
) (*api.BatchGetItemOutput, error) {
	if err := validateCapacity(in.ReturnConsumedCapacity); err != nil {
		return nil, err
	}

	if len(in.RequestItems) == 0 {
		return nil, awserror.Validation("RequestItems must contain at least one table")
	}

	names := slices.Sorted(maps.Keys(in.RequestItems))
	keys := make(map[string]string, len(names))
	projections := make(map[string]projection, len(names))
	total := 0
	for _, name := range names {
		request := in.RequestItems[name]
		key, err := tableKey(ctx, &name)
		if err != nil {
			return nil, err
		}

		if len(request.Keys) == 0 {
			return nil, awserror.Validation("Keys for table %s must not be empty", name)
		}

		pr, err := readProjection(
			request.AttributesToGet,
			request.ExpressionAttributeNames,
			request.ProjectionExpression,
		)
		if err != nil {
			return nil, err
		}

		keys[name], projections[name] = key, pr
		total += len(request.Keys)
	}

	if total > maxBatchGet {
		return nil, awserror.Validation("Too many items requested for the BatchGetItem call")
	}

	out := &api.BatchGetItemOutput{
		Responses:       map[string][]map[string]api.AttributeValue{},
		UnprocessedKeys: map[string]api.KeysAndAttributes{},
	}

	err := s.readData(ctx, used, func(tx storage.ReadTx) error {
		var size int64
		for _, name := range names {
			request := in.RequestItems[name]
			t, err := load(tx, keys[name])
			if err != nil {
				return err
			}

			out.Responses[name] = []map[string]api.AttributeValue{}
			seen := map[string]bool{}
			for _, k := range request.Keys {
				id, keyErr := itemKey(t, k, true)
				if keyErr != nil {
					return keyErr
				}

				if seen[id] {
					return duplicateKeys()
				}

				seen[id] = true
				if size >= maxBatchResponse {
					rest := out.UnprocessedKeys[name]
					rest.Keys = append(rest.Keys, k)
					rest.ConsistentRead = request.ConsistentRead
					rest.AttributesToGet = request.AttributesToGet
					rest.ProjectionExpression = request.ProjectionExpression
					rest.ExpressionAttributeNames = request.ExpressionAttributeNames
					out.UnprocessedKeys[name] = rest
					continue
				}

				it, found, loadErr := loadItem(tx, itemRecord(keys[name], id))
				if loadErr != nil {
					return loadErr
				}

				if !found {
					used.charge(keys[name], nil, 0, common.Value(request.ConsistentRead))
					continue
				}

				bytes, sizeErr := itemSize(it)
				if sizeErr != nil {
					return sizeErr
				}

				size += bytes
				used.charge(keys[name], nil, bytes, common.Value(request.ConsistentRead))
				out.Responses[name] = append(out.Responses[name], projections[name].apply(it))
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out.ConsumedCapacity = used.capacity(in.ReturnConsumedCapacity)
	return out, nil
}

// BatchWriteItem puts and deletes up to 25 items across tables in one transaction.
func (s *Service) BatchWriteItem(
	ctx context.Context,
	in *api.BatchWriteItemInput,
) (*api.BatchWriteItemOutput, error) {
	puts := s.newOutbox(false)
	return observed(ctx, s, "BatchWriteItem", &puts.used, func() (*api.BatchWriteItemOutput, error) {
		return s.batchWriteItem(ctx, in, &puts)
	})
}

func (s *Service) batchWriteItem(
	ctx context.Context,
	in *api.BatchWriteItemInput,
	puts *outbox,
) (*api.BatchWriteItemOutput, error) {
	if err := validateCapacity(in.ReturnConsumedCapacity); err != nil {
		return nil, err
	}

	if err := puts.requestMetrics(in.ReturnItemCollectionMetrics); err != nil {
		return nil, err
	}

	if len(in.RequestItems) == 0 {
		return nil, awserror.Validation("RequestItems must contain at least one table")
	}

	names := slices.Sorted(maps.Keys(in.RequestItems))
	keys := make(map[string]string, len(names))
	total := 0
	for _, name := range names {
		key, err := tableKey(ctx, &name)
		if err != nil {
			return nil, err
		}

		if len(in.RequestItems[name]) == 0 {
			return nil, awserror.Validation("The write requests for table %s must not be empty", name)
		}

		for _, w := range in.RequestItems[name] {
			if (w.PutRequest == nil) == (w.DeleteRequest == nil) ||
				(w.PutRequest != nil && w.PutRequest.Item == nil) ||
				(w.DeleteRequest != nil && w.DeleteRequest.Key == nil) {
				return nil, awserror.Validation("Each WriteRequest must hold exactly one of an Item to put " +
					"or a Key to delete")
			}
		}

		keys[name] = key
		total += len(in.RequestItems[name])
	}

	if total > maxBatchWrite {
		return nil, awserror.Validation("Too many items requested for the BatchWriteItem call")
	}

	err := s.writeData(ctx, puts, func(tx storage.WriteTx) error {
		for _, name := range names {
			key := keys[name]
			t, err := load(tx, key)
			if err != nil {
				return err
			}

			seen := map[string]bool{}
			for _, w := range in.RequestItems[name] {
				k, next := item(nil), item(nil)
				if w.PutRequest != nil {
					k, next = w.PutRequest.Item, w.PutRequest.Item
					if guardErr := checkItem(
						t,
						next,
						"Item size has exceeded the maximum allowed size",
					); guardErr != nil {
						return guardErr
					}
				} else {
					k = w.DeleteRequest.Key
				}

				record, old, found, targetErr := target(tx, key, t, k, next == nil)
				if targetErr != nil {
					return targetErr
				}

				if seen[record] {
					return duplicateKeys()
				}

				seen[record] = true
				if next == nil && !found {
					puts.used.chargeWrite(capacityTarget{key: key}, 0)
					continue
				}

				if guardErr := writeItem(tx, key, t, record, old, next, puts); guardErr != nil {
					return guardErr
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	s.deliver(ctx, puts)
	return &api.BatchWriteItemOutput{
		UnprocessedItems:      map[string][]api.WriteRequest{},
		ConsumedCapacity:      puts.used.capacity(in.ReturnConsumedCapacity),
		ItemCollectionMetrics: puts.collectionMetrics,
	}, nil
}
