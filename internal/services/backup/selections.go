package backup

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateBackupSelection(
	ctx context.Context,
	in *api.CreateBackupSelectionInput,
) (*api.CreateBackupSelectionOutput, error) {
	if err := validateSelection(in.BackupSelection); err != nil {
		return nil, err
	}
	if err := tokenOK(in.CreatorRequestId); err != nil {
		return nil, err
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var result selection
	var planID string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := activePlan(tx, scope, common.Value(in.BackupPlanId))
		if e != nil {
			// CreateBackupSelection does not model ResourceNotFoundException.
			var apiErr *awserror.Error
			if errors.As(e, &apiErr) && apiErr.Code == "ResourceNotFoundException" {
				return invalid("BackupPlanId must identify an active regional backup plan")
			}
			return e
		}
		planID = r.ID
		for _, old := range r.Selections {
			if in.CreatorRequestId != nil && old.Token != nil && *in.CreatorRequestId == *old.Token {
				result = old
				return nil
			}
		}
		if len(in.BackupSelection.ListOfTags) > 0 {
			count := 0
			for _, old := range r.Selections {
				if len(old.Value.ListOfTags) > 0 {
					count++
				}
			}
			if count >= 10 {
				return failure("LimitExceededException", "At most 10 tag-based selections per plan")
			}
		}
		if len(r.Selections) >= 100 {
			return failure("LimitExceededException", "At most 100 selections are supported per plan")
		}
		result = selection{ID: id(), Created: stamp(), Value: *in.BackupSelection, Token: in.CreatorRequestId}
		r.Selections[result.ID] = result
		return save(tx, scope, r)
	})
	if err != nil {
		return nil, err
	}
	return &api.CreateBackupSelectionOutput{
		SelectionId:  new(result.ID),
		BackupPlanId: new(planID),
		CreationDate: new(result.Created),
	}, nil
}

func readSelection(tx storage.ReadTx, scope request.Scope, planID, selectionID string) (*record, selection, error) {
	if selectionID == "" {
		return nil, selection{}, failure("MissingParameterValueException", "SelectionId is required")
	}
	r, err := activePlan(tx, scope, planID)
	if err != nil {
		return nil, selection{}, err
	}
	item, ok := r.Selections[selectionID]
	if !ok {
		return nil, selection{}, missing()
	}
	return r, item, nil
}

func (s *Service) GetBackupSelection(
	ctx context.Context,
	in *api.GetBackupSelectionInput,
) (*api.GetBackupSelectionOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	var r *record
	var item selection
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, item, e = readSelection(tx, scope, common.Value(in.BackupPlanId), common.Value(in.SelectionId))
		return e
	})
	if err != nil {
		return nil, err
	}
	return &api.GetBackupSelectionOutput{
		BackupSelection: &item.Value, SelectionId: new(item.ID), BackupPlanId: new(r.ID),
		CreationDate: new(item.Created), CreatorRequestId: item.Token,
	}, nil
}

func (s *Service) DeleteBackupSelection(
	ctx context.Context,
	in *api.DeleteBackupSelectionInput,
) (*api.DeleteBackupSelectionOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, item, e := readSelection(tx, scope, common.Value(in.BackupPlanId), common.Value(in.SelectionId))
		if e != nil {
			return e
		}
		delete(r.Selections, item.ID)
		return save(tx, scope, r)
	})
	return &api.DeleteBackupSelectionOutput{}, err
}

func (s *Service) ListBackupSelections(
	ctx context.Context,
	in *api.ListBackupSelectionsInput,
) (*api.ListBackupSelectionsOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	rows := []api.BackupSelectionsListMember{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := activePlan(tx, scope, common.Value(in.BackupPlanId))
		if e != nil {
			return e
		}
		for _, key := range slices.Sorted(maps.Keys(r.Selections)) {
			item := r.Selections[key]
			rows = append(rows, api.BackupSelectionsListMember{
				SelectionId:   new(item.ID),
				SelectionName: item.Value.SelectionName,
				BackupPlanId: new(
					r.ID,
				),
				CreationDate:     new(item.Created),
				CreatorRequestId: item.Token,
				IamRoleArn:       item.Value.IamRoleArn,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, next, err := page(
		rows,
		func(r api.BackupSelectionsListMember) string { return *r.SelectionId },
		in.NextToken,
		in.MaxResults,
		scope,
		"selections/"+common.Value(in.BackupPlanId),
	)
	return &api.ListBackupSelectionsOutput{BackupSelectionsList: rows, NextToken: next}, err
}
