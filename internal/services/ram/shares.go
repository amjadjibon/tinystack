package ram

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ram"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateResourceShare(
	ctx context.Context,
	in *api.CreateResourceShareRequest,
) (*api.CreateResourceShareResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return nil, invalidParameter("The name must be specified and cannot be empty.")
	}

	if len(*in.Name) > 255 {
		return nil, invalidParameter("The name cannot exceed 255 characters.")
	}

	allowExternal := true
	if in.AllowExternalPrincipals != nil {
		allowExternal = *in.AllowExternalPrincipals
	}

	for _, resArn := range in.ResourceArns {
		if vErr := validateResourceARN(resArn); vErr != nil {
			return nil, vErr
		}
	}

	for _, p := range in.Principals {
		if !isValidPrincipal(p) {
			return nil, invalidParameter("The principal %s is not valid.", p)
		}

		if !allowExternal && isExternalPrincipal(p, scope.AccountID) {
			return nil, operationNotPermitted(
				"External principals are not permitted when allowExternalPrincipals is false.",
			)
		}
	}

	tags, tagErr := tagMap(in.Tags)
	if tagErr != nil {
		return nil, tagErr
	}

	if checkErr := tagging.CheckSet(tags); checkErr != nil {
		return nil, checkTagLimit(checkErr)
	}

	id := newShareID()
	arn := shareARN(scope, id)
	now := common.Seconds(time.Now().UTC())

	share := &api.ResourceShare{
		AllowExternalPrincipals: new(allowExternal),
		CreationTime:            now,
		FeatureSet:              new(featureSet),
		LastUpdatedTime:         now,
		Name:                    new(*in.Name),
		OwningAccountId:         new(scope.AccountID),
		ResourceShareArn:        new(arn),
		Status:                  new(statusActive),
		Tags:                    tagList(tags),
	}

	rec := &resourceShareRecord{
		Share: share,
		Tags:  tags,
	}

	shareKey := scope.Key(serviceName, shareKind, id)
	tokenKey := idempotency.Key(scope, serviceName, "CreateResourceShare", in.ClientToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, found, replayErr := idempotency.Replay[*api.ResourceShare](tx, tokenKey, in, idempotentMismatch())
		if replayErr != nil {
			return replayErr
		}

		if found {
			share = previous
			return nil
		}

		if wErr := storage.Write(tx, shareKey, rec); wErr != nil {
			return wErr
		}

		for _, resArn := range in.ResourceArns {
			assoc := api.ResourceShareAssociation{
				AssociatedEntity:  new(resArn),
				AssociationType:   new("RESOURCE"),
				CreationTime:      now,
				LastUpdatedTime:   now,
				ResourceShareArn:  new(arn),
				ResourceShareName: new(*in.Name),
				Status:            new(statusAssociated),
			}

			assocKey := associationKey(scope, id, "RESOURCE", resArn)
			if wErr := storage.Write(tx, assocKey, &associationRecord{Association: &assoc}); wErr != nil {
				return wErr
			}
		}

		for _, p := range in.Principals {
			external := isExternalPrincipal(p, scope.AccountID)
			assoc := api.ResourceShareAssociation{
				AssociatedEntity:  new(p),
				AssociationType:   new("PRINCIPAL"),
				CreationTime:      now,
				External:          new(external),
				LastUpdatedTime:   now,
				ResourceShareArn:  new(arn),
				ResourceShareName: new(*in.Name),
				Status:            new(statusAssociated),
			}

			assocKey := associationKey(scope, id, "PRINCIPAL", p)
			if wErr := storage.Write(tx, assocKey, &associationRecord{Association: &assoc}); wErr != nil {
				return wErr
			}
		}

		return idempotency.Remember(tx, tokenKey, in, share)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateResourceShareResponse{
		ClientToken:   in.ClientToken,
		ResourceShare: share,
	}, nil
}

func (s *Service) GetResourceShares(
	ctx context.Context,
	in *api.GetResourceSharesRequest,
) (*api.GetResourceSharesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceOwner == nil || *in.ResourceOwner == "" {
		return nil, invalidParameter("ResourceOwner is required")
	}

	owner := *in.ResourceOwner
	if owner != "SELF" && owner != "OTHER-ACCOUNTS" {
		return nil, invalidParameter("ResourceOwner must be SELF or OTHER-ACCOUNTS")
	}

	if owner == "OTHER-ACCOUNTS" {
		return &api.GetResourceSharesResponse{
			ResourceShares: []api.ResourceShare{},
		}, nil
	}

	for _, a := range in.ResourceShareArns {
		if _, pErr := parseShareARN(a); pErr != nil {
			return nil, pErr
		}
	}

	var shares []api.ResourceShare
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := scope.Prefix(serviceName, shareKind)
		for _, key := range tx.Keys(prefix) {
			rec, found, readErr := storage.Read[resourceShareRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if !found || rec == nil || rec.Share == nil {
				continue
			}

			sh := rec.Share

			if len(in.ResourceShareArns) > 0 {
				matched := false
				for _, reqArn := range in.ResourceShareArns {
					if sh.ResourceShareArn != nil && *sh.ResourceShareArn == reqArn {
						matched = true
						break
					}
				}

				if !matched {
					continue
				}
			}

			if in.Name != nil && *in.Name != "" {
				if sh.Name == nil || *sh.Name != *in.Name {
					continue
				}
			}

			if in.ResourceShareStatus != nil && *in.ResourceShareStatus != "" {
				if sh.Status == nil || *sh.Status != *in.ResourceShareStatus {
					continue
				}
			} else if len(in.ResourceShareArns) == 0 {
				if sh.Status == nil || *sh.Status != statusActive {
					continue
				}
			}

			shCopy := *sh
			shCopy.Tags = tagList(rec.Tags)
			shares = append(shares, shCopy)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(shares, func(a, b api.ResourceShare) int {
		return cmp.Compare(common.Value(a.ResourceShareArn), common.Value(b.ResourceShareArn))
	})

	limit, limitErr := pageLimit(in.MaxResults)
	if limitErr != nil {
		return nil, limitErr
	}

	page, next, pageErr := common.Page(shares, func(s api.ResourceShare) string {
		return common.Value(s.ResourceShareArn)
	}, in.NextToken, limit, invalidNextToken("Invalid nextToken"))
	if pageErr != nil {
		return nil, pageErr
	}

	return &api.GetResourceSharesResponse{
		NextToken:      next,
		ResourceShares: page,
	}, nil
}

func (s *Service) UpdateResourceShare(
	ctx context.Context,
	in *api.UpdateResourceShareRequest,
) (*api.UpdateResourceShareResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceShareArn == nil || *in.ResourceShareArn == "" {
		return nil, invalidParameter("ResourceShareArn is required")
	}

	id, parseErr := parseShareARN(*in.ResourceShareArn)
	if parseErr != nil {
		return nil, parseErr
	}

	shareKey := scope.Key(serviceName, shareKind, id)

	var updatedShare *api.ResourceShare
	tokenKey := idempotency.Key(scope, serviceName, "UpdateResourceShare", in.ClientToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, replayed, replayErr := idempotency.Replay[*api.ResourceShare](tx, tokenKey, in, idempotentMismatch())
		if replayErr != nil {
			return replayErr
		}

		if replayed {
			updatedShare = previous
			return nil
		}

		rec, found, readErr := storage.Read[resourceShareRecord](tx, shareKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil || rec.Share == nil || (rec.Share.Status != nil && *rec.Share.Status == statusDeleted) {
			return unknownResource(*in.ResourceShareArn)
		}

		candidate := *rec.Share
		if in.Name != nil {
			if strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 255 {
				return invalidParameter("The name must contain 1-255 characters.")
			}

			candidate.Name = new(*in.Name)
		}

		if in.AllowExternalPrincipals != nil {
			if !*in.AllowExternalPrincipals &&
				rec.Share.AllowExternalPrincipals != nil &&
				*rec.Share.AllowExternalPrincipals {
				assocPrefix := associationTypePrefix(scope, id, "PRINCIPAL")
				for _, assocKey := range tx.Keys(assocPrefix) {
					aRec, aFound, aErr := storage.Read[associationRecord](tx, assocKey)
					if aErr != nil {
						return aErr
					}

					if aFound && aRec != nil && aRec.Association != nil &&
						aRec.Association.Status != nil && *aRec.Association.Status == statusAssociated &&
						aRec.Association.External != nil && *aRec.Association.External {
						return operationNotPermitted(
							"Cannot set allowExternalPrincipals to false when external principals are associated.",
						)
					}
				}
			}

			candidate.AllowExternalPrincipals = new(*in.AllowExternalPrincipals)
		}

		candidate.LastUpdatedTime = common.Seconds(time.Now().UTC())
		rec.Share = &candidate

		if writeErr := storage.Write(tx, shareKey, rec); writeErr != nil {
			return writeErr
		}

		if in.Name != nil && *in.Name != "" {
			assocPrefix := associationSharePrefix(scope, id)
			for _, assocKey := range tx.Keys(assocPrefix) {
				aRec, aFound, aErr := storage.Read[associationRecord](tx, assocKey)
				if aErr != nil {
					return aErr
				}

				if aFound && aRec != nil && aRec.Association != nil {
					aRec.Association.ResourceShareName = new(*in.Name)
					if wErr := storage.Write(tx, assocKey, aRec); wErr != nil {
						return wErr
					}
				}
			}
		}

		updatedShare = &candidate
		updatedShare.Tags = tagList(rec.Tags)
		return idempotency.Remember(tx, tokenKey, in, updatedShare)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateResourceShareResponse{
		ClientToken:   in.ClientToken,
		ResourceShare: updatedShare,
	}, nil
}

func (s *Service) DeleteResourceShare(
	ctx context.Context,
	in *api.DeleteResourceShareRequest,
) (*api.DeleteResourceShareResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceShareArn == nil || *in.ResourceShareArn == "" {
		return nil, invalidParameter("ResourceShareArn is required")
	}

	id, parseErr := parseShareARN(*in.ResourceShareArn)
	if parseErr != nil {
		return nil, parseErr
	}

	shareKey := scope.Key(serviceName, shareKind, id)

	tokenKey := idempotency.Key(scope, serviceName, "DeleteResourceShare", in.ClientToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, replayed, replayErr := idempotency.Replay[struct{}](tx, tokenKey, in, idempotentMismatch())
		if replayErr != nil {
			return replayErr
		}

		if replayed {
			return nil
		}

		rec, found, readErr := storage.Read[resourceShareRecord](tx, shareKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil || rec.Share == nil {
			return unknownResource(*in.ResourceShareArn)
		}

		now := common.Seconds(time.Now().UTC())
		rec.Share.Status = new(statusDeleted)
		rec.Share.LastUpdatedTime = now

		if writeErr := storage.Write(tx, shareKey, rec); writeErr != nil {
			return writeErr
		}

		assocPrefix := associationSharePrefix(scope, id)
		for _, assocKey := range tx.Keys(assocPrefix) {
			aRec, aFound, aErr := storage.Read[associationRecord](tx, assocKey)
			if aErr != nil {
				return aErr
			}

			if aFound && aRec != nil && aRec.Association != nil {
				aRec.Association.Status = new(statusDisassociated)
				aRec.Association.LastUpdatedTime = now
				if wErr := storage.Write(tx, assocKey, aRec); wErr != nil {
					return wErr
				}
			}
		}

		return idempotency.Remember(tx, tokenKey, in, struct{}{})
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteResourceShareResponse{
		ClientToken: in.ClientToken,
		ReturnValue: new(true),
	}, nil
}
