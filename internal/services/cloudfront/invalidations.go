package cloudfront

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindInvalidation = "invalidation"
	kindInvRef       = "invalidation-ref"
)

func noSuchInvalidation(id string) error {
	msg := "The specified invalidation does not exist."
	if id != "" {
		msg = "The specified invalidation does not exist: " + id
	}
	return &awserror.Error{
		Code:       "NoSuchInvalidation",
		Message:    msg,
		StatusCode: 404,
	}
}

func invKey(account, distID, id string) string {
	return account + "/" + service + "/" + kindDistribution + "/" + distID + "/" + kindInvalidation + "/" + id
}

func invRefKey(account, distID, ref string) string {
	return account + "/" + service + "/" + kindDistribution + "/" + distID + "/" + kindInvRef + "/" + ref
}

func invLocation(distID, id string) string {
	return "https://cloudfront.amazonaws.com/2020-05-31/distribution/" + distID + "/invalidation/" + id
}

func (s *Service) CreateInvalidation(
	ctx context.Context, in *api.CreateInvalidationRequest,
) (*api.CreateInvalidationResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	distID := common.Value(in.DistributionId)
	if distID == "" {
		return nil, noSuchDistribution("")
	}
	if in.InvalidationBatch == nil || in.InvalidationBatch.Paths == nil {
		return nil, invalidArgument("InvalidationBatch is required")
	}

	ref := common.Value(in.InvalidationBatch.CallerReference)
	if ref == "" {
		return nil, invalidArgument("CallerReference is required")
	}

	var resultInv *api.Invalidation
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		// Verify distribution exists
		_, found, readErr := storage.Read[distributionRecord](tx, key(m.AccountID, kindDistribution, distID))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchDistribution(distID)
		}

		// Check idempotency
		rKey := invRefKey(m.AccountID, distID, ref)
		if existingID, exists := tx.Get(rKey); exists {
			existingInv, invFound, readErr := storage.Read[api.Invalidation](
				tx, invKey(m.AccountID, distID, string(existingID)),
			)
			if readErr != nil {
				return readErr
			}
			if invFound {
				resultInv = existingInv
				return nil
			}
		}

		invID := newID("I")
		now := s.now().UTC()
		inv := &api.Invalidation{
			Id:                &invID,
			Status:            new("Completed"),
			CreateTime:        &now,
			InvalidationBatch: in.InvalidationBatch,
		}

		if writeErr := storage.Write(tx, invKey(m.AccountID, distID, invID), inv); writeErr != nil {
			return writeErr
		}
		tx.Put(rKey, []byte(invID))

		resultInv = inv
		return nil
	})
	if err != nil {
		return nil, err
	}

	loc := invLocation(distID, common.Value(resultInv.Id))
	return &api.CreateInvalidationResult{
		Invalidation: resultInv,
		Location:     &loc,
	}, nil
}

func (s *Service) GetInvalidation(
	ctx context.Context, in *api.GetInvalidationRequest,
) (*api.GetInvalidationResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	distID := common.Value(in.DistributionId)
	if distID == "" {
		return nil, noSuchDistribution("")
	}
	id := common.Value(in.Id)
	if id == "" {
		return nil, noSuchInvalidation("")
	}

	var inv *api.Invalidation
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[distributionRecord](tx, key(m.AccountID, kindDistribution, distID))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchDistribution(distID)
		}

		i, invFound, readErr := storage.Read[api.Invalidation](tx, invKey(m.AccountID, distID, id))
		if readErr != nil {
			return readErr
		}
		if !invFound {
			return noSuchInvalidation(id)
		}
		inv = i
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetInvalidationResult{
		Invalidation: inv,
	}, nil
}

func (s *Service) ListInvalidations(
	ctx context.Context, in *api.ListInvalidationsRequest,
) (*api.ListInvalidationsResult, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	distID := common.Value(in.DistributionId)
	if distID == "" {
		return nil, noSuchDistribution("")
	}

	var items []api.InvalidationSummary
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		_, found, readErr := storage.Read[distributionRecord](tx, key(m.AccountID, kindDistribution, distID))
		if readErr != nil {
			return readErr
		}
		if !found {
			return noSuchDistribution(distID)
		}

		prefix := m.AccountID + "/" + service + "/" + kindDistribution + "/" + distID + "/" + kindInvalidation + "/"
		keys := tx.Keys(prefix)
		slices.Sort(keys)

		for _, k := range keys {
			inv, invFound, readErr := storage.Read[api.Invalidation](tx, k)
			if readErr != nil {
				return readErr
			}
			if invFound && inv != nil {
				items = append(items, api.InvalidationSummary{
					Id:         inv.Id,
					CreateTime: inv.CreateTime,
					Status:     inv.Status,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	maxItems := int64(100)
	if in != nil && in.MaxItems != nil && *in.MaxItems > 0 {
		maxItems = *in.MaxItems
	}

	marker := ""
	if in != nil && in.Marker != nil {
		marker = *in.Marker
	}

	start := 0
	if marker != "" {
		for i := range items {
			if common.Value(items[i].Id) > marker {
				start = i
				break
			}
			if i == len(items)-1 {
				start = len(items)
			}
		}
	}

	page := items[start:]
	truncated := false
	var nextMarker *string
	if int64(len(page)) > maxItems {
		truncated = true
		page = page[:maxItems]
		nextMarker = page[len(page)-1].Id
	}

	quantity := int64(len(page))
	return &api.ListInvalidationsResult{
		InvalidationList: &api.InvalidationList{
			IsTruncated: &truncated,
			Items:       page,
			Marker:      &marker,
			MaxItems:    &maxItems,
			NextMarker:  nextMarker,
			Quantity:    &quantity,
		},
	}, nil
}
