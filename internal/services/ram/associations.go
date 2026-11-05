package ram

import (
	"cmp"
	"context"
	"slices"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/ram"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AssociateResourceShare(
	ctx context.Context,
	in *api.AssociateResourceShareRequest,
) (*api.AssociateResourceShareResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceShareArn == nil || *in.ResourceShareArn == "" {
		return nil, invalidParameter("ResourceShareArn is required")
	}

	if len(in.ResourceArns) == 0 && len(in.Principals) == 0 {
		return nil, invalidParameter("At least one resource ARN or principal must be specified.")
	}

	id, parseErr := parseShareARN(*in.ResourceShareArn)
	if parseErr != nil {
		return nil, parseErr
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
	}

	shareKey := scope.Key(serviceName, shareKind, id)

	var associations []api.ResourceShareAssociation
	now := common.Seconds(time.Now().UTC())

	tokenKey := idempotency.Key(scope, serviceName, "AssociateResourceShare", in.ClientToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, replayed, replayErr := idempotency.Replay[[]api.ResourceShareAssociation](
			tx,
			tokenKey,
			in,
			idempotentMismatch(),
		)
		if replayErr != nil {
			return replayErr
		}

		if replayed {
			associations = previous
			return nil
		}

		rec, found, readErr := storage.Read[resourceShareRecord](tx, shareKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil || rec.Share == nil || (rec.Share.Status != nil && *rec.Share.Status == statusDeleted) {
			return unknownResource(*in.ResourceShareArn)
		}

		allowExternal := rec.Share.AllowExternalPrincipals != nil && *rec.Share.AllowExternalPrincipals
		for _, p := range in.Principals {
			if !allowExternal && isExternalPrincipal(p, scope.AccountID) {
				return operationNotPermitted(
					"External principals are not permitted when allowExternalPrincipals is false.",
				)
			}
		}

		shareName := common.Value(rec.Share.Name)

		for _, resArn := range in.ResourceArns {
			assocKey := associationKey(scope, id, "RESOURCE", resArn)
			aRec, aFound, aErr := storage.Read[associationRecord](tx, assocKey)
			if aErr != nil {
				return aErr
			}

			var assoc api.ResourceShareAssociation
			if aFound && aRec != nil && aRec.Association != nil {
				assoc = *aRec.Association
				assoc.Status = new(statusAssociated)
				assoc.LastUpdatedTime = now
			} else {
				assoc = api.ResourceShareAssociation{
					AssociatedEntity:  new(resArn),
					AssociationType:   new("RESOURCE"),
					CreationTime:      now,
					LastUpdatedTime:   now,
					ResourceShareArn:  new(*in.ResourceShareArn),
					ResourceShareName: new(shareName),
					Status:            new(statusAssociated),
				}
			}

			if wErr := storage.Write(tx, assocKey, &associationRecord{Association: &assoc}); wErr != nil {
				return wErr
			}

			associations = append(associations, assoc)
		}

		for _, p := range in.Principals {
			external := isExternalPrincipal(p, scope.AccountID)
			assocKey := associationKey(scope, id, "PRINCIPAL", p)
			aRec, aFound, aErr := storage.Read[associationRecord](tx, assocKey)
			if aErr != nil {
				return aErr
			}

			var assoc api.ResourceShareAssociation
			if aFound && aRec != nil && aRec.Association != nil {
				assoc = *aRec.Association
				assoc.Status = new(statusAssociated)
				assoc.LastUpdatedTime = now
			} else {
				assoc = api.ResourceShareAssociation{
					AssociatedEntity:  new(p),
					AssociationType:   new("PRINCIPAL"),
					CreationTime:      now,
					External:          new(external),
					LastUpdatedTime:   now,
					ResourceShareArn:  new(*in.ResourceShareArn),
					ResourceShareName: new(shareName),
					Status:            new(statusAssociated),
				}
			}

			if wErr := storage.Write(tx, assocKey, &associationRecord{Association: &assoc}); wErr != nil {
				return wErr
			}

			associations = append(associations, assoc)
		}

		return idempotency.Remember(tx, tokenKey, in, associations)
	})
	if err != nil {
		return nil, err
	}

	return &api.AssociateResourceShareResponse{
		ClientToken:               in.ClientToken,
		ResourceShareAssociations: associations,
	}, nil
}

func (s *Service) DisassociateResourceShare(
	ctx context.Context,
	in *api.DisassociateResourceShareRequest,
) (*api.DisassociateResourceShareResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.ResourceShareArn == nil || *in.ResourceShareArn == "" {
		return nil, invalidParameter("ResourceShareArn is required")
	}

	if len(in.ResourceArns) == 0 && len(in.Principals) == 0 {
		return nil, invalidParameter("At least one resource ARN or principal must be specified.")
	}

	id, parseErr := parseShareARN(*in.ResourceShareArn)
	if parseErr != nil {
		return nil, parseErr
	}

	shareKey := scope.Key(serviceName, shareKind, id)

	var associations []api.ResourceShareAssociation
	now := common.Seconds(time.Now().UTC())

	tokenKey := idempotency.Key(scope, serviceName, "DisassociateResourceShare", in.ClientToken)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		previous, replayed, replayErr := idempotency.Replay[[]api.ResourceShareAssociation](
			tx,
			tokenKey,
			in,
			idempotentMismatch(),
		)
		if replayErr != nil {
			return replayErr
		}

		if replayed {
			associations = previous
			return nil
		}

		rec, found, readErr := storage.Read[resourceShareRecord](tx, shareKey)
		if readErr != nil {
			return readErr
		}

		if !found || rec == nil || rec.Share == nil {
			return unknownResource(*in.ResourceShareArn)
		}

		shareName := common.Value(rec.Share.Name)

		for _, resArn := range in.ResourceArns {
			assocKey := associationKey(scope, id, "RESOURCE", resArn)
			aRec, aFound, aErr := storage.Read[associationRecord](tx, assocKey)
			if aErr != nil {
				return aErr
			}

			var assoc api.ResourceShareAssociation
			if aFound && aRec != nil && aRec.Association != nil {
				assoc = *aRec.Association
				assoc.Status = new(statusDisassociated)
				assoc.LastUpdatedTime = now
			} else {
				assoc = api.ResourceShareAssociation{
					AssociatedEntity:  new(resArn),
					AssociationType:   new("RESOURCE"),
					CreationTime:      now,
					LastUpdatedTime:   now,
					ResourceShareArn:  new(*in.ResourceShareArn),
					ResourceShareName: new(shareName),
					Status:            new(statusDisassociated),
				}
			}

			if wErr := storage.Write(tx, assocKey, &associationRecord{Association: &assoc}); wErr != nil {
				return wErr
			}

			associations = append(associations, assoc)
		}

		for _, p := range in.Principals {
			external := isExternalPrincipal(p, scope.AccountID)
			assocKey := associationKey(scope, id, "PRINCIPAL", p)
			aRec, aFound, aErr := storage.Read[associationRecord](tx, assocKey)
			if aErr != nil {
				return aErr
			}

			var assoc api.ResourceShareAssociation
			if aFound && aRec != nil && aRec.Association != nil {
				assoc = *aRec.Association
				assoc.Status = new(statusDisassociated)
				assoc.LastUpdatedTime = now
			} else {
				assoc = api.ResourceShareAssociation{
					AssociatedEntity:  new(p),
					AssociationType:   new("PRINCIPAL"),
					CreationTime:      now,
					External:          new(external),
					LastUpdatedTime:   now,
					ResourceShareArn:  new(*in.ResourceShareArn),
					ResourceShareName: new(shareName),
					Status:            new(statusDisassociated),
				}
			}

			if wErr := storage.Write(tx, assocKey, &associationRecord{Association: &assoc}); wErr != nil {
				return wErr
			}

			associations = append(associations, assoc)
		}

		return idempotency.Remember(tx, tokenKey, in, associations)
	})
	if err != nil {
		return nil, err
	}

	return &api.DisassociateResourceShareResponse{
		ClientToken:               in.ClientToken,
		ResourceShareAssociations: associations,
	}, nil
}

func (s *Service) GetResourceShareAssociations(
	ctx context.Context,
	in *api.GetResourceShareAssociationsRequest,
) (*api.GetResourceShareAssociationsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.AssociationType == nil || *in.AssociationType == "" {
		return nil, invalidParameter("AssociationType is required")
	}

	aType := *in.AssociationType
	if aType != "RESOURCE" && aType != "PRINCIPAL" {
		return nil, invalidParameter("AssociationType must be RESOURCE or PRINCIPAL")
	}

	var associations []api.ResourceShareAssociation
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := scope.Prefix(serviceName, associationKind)
		for _, key := range tx.Keys(prefix) {
			rec, found, readErr := storage.Read[associationRecord](tx, key)
			if readErr != nil {
				return readErr
			}

			if !found || rec == nil || rec.Association == nil {
				continue
			}

			assoc := rec.Association
			if assoc.AssociationType == nil || *assoc.AssociationType != aType {
				continue
			}

			if len(in.ResourceShareArns) > 0 {
				matched := false
				for _, rArn := range in.ResourceShareArns {
					if assoc.ResourceShareArn != nil && *assoc.ResourceShareArn == rArn {
						matched = true
						break
					}
				}

				if !matched {
					continue
				}
			}

			if in.ResourceArn != nil && *in.ResourceArn != "" {
				if assoc.AssociatedEntity == nil || *assoc.AssociatedEntity != *in.ResourceArn {
					continue
				}
			}

			if in.Principal != nil && *in.Principal != "" {
				if assoc.AssociatedEntity == nil || *assoc.AssociatedEntity != *in.Principal {
					continue
				}
			}

			if in.AssociationStatus != nil && *in.AssociationStatus != "" {
				if assoc.Status == nil || *assoc.Status != *in.AssociationStatus {
					continue
				}
			}

			associations = append(associations, *assoc)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(associations, func(a, b api.ResourceShareAssociation) int {
		if c := cmp.Compare(common.Value(a.ResourceShareArn), common.Value(b.ResourceShareArn)); c != 0 {
			return c
		}

		return cmp.Compare(common.Value(a.AssociatedEntity), common.Value(b.AssociatedEntity))
	})

	limit, limitErr := pageLimit(in.MaxResults)
	if limitErr != nil {
		return nil, limitErr
	}

	page, next, pageErr := common.Page(associations, func(a api.ResourceShareAssociation) string {
		return common.Value(a.ResourceShareArn) + "/" + common.Value(a.AssociatedEntity)
	}, in.NextToken, limit, invalidNextToken("Invalid nextToken"))
	if pageErr != nil {
		return nil, pageErr
	}

	return &api.GetResourceShareAssociationsResponse{
		NextToken:                 next,
		ResourceShareAssociations: page,
	}, nil
}
