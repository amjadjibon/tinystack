package amp

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type alertManagerRecord struct {
	CreatedAt   time.Time `json:"createdAt"`
	ModifiedAt  time.Time `json:"modifiedAt"`
	WorkspaceID string    `json:"workspaceId"`
	Data        []byte    `json:"data"`
}

func (s *Service) CreateAlertManagerDefinition(
	ctx context.Context,
	in *api.CreateAlertManagerDefinitionRequest,
) (*api.CreateAlertManagerDefinitionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	err = validateData(in.Data)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	record := alertManagerRecord{
		WorkspaceID: workspaceID,
		Data:        in.Data,
		CreatedAt:   now,
		ModifiedAt:  now,
	}

	recordKey := key(scope, kindAlertManagerDefinition, workspaceID)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		_, found, e := storage.Read[alertManagerRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if found {
			return conflict("AlertManagerDefinition already exists")
		}

		return storage.Write(tx, recordKey, record)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateAlertManagerDefinitionResponse{
		Status: &api.AlertManagerDefinitionStatus{StatusCode: new(statusActive)},
	}, nil
}

func (s *Service) DescribeAlertManagerDefinition(
	ctx context.Context,
	in *api.DescribeAlertManagerDefinitionRequest,
) (*api.DescribeAlertManagerDefinitionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	var record *alertManagerRecord
	recordKey := key(scope, kindAlertManagerDefinition, workspaceID)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		r, found, e := storage.Read[alertManagerRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("AlertManagerDefinition", workspaceID)
		}

		record = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeAlertManagerDefinitionResponse{
		AlertManagerDefinition: &api.AlertManagerDefinitionDescription{
			CreatedAt:  common.Seconds(record.CreatedAt),
			Data:       record.Data,
			ModifiedAt: common.Seconds(record.ModifiedAt),
			Status:     &api.AlertManagerDefinitionStatus{StatusCode: new(statusActive)},
		},
	}, nil
}

func (s *Service) PutAlertManagerDefinition(
	ctx context.Context,
	in *api.PutAlertManagerDefinitionRequest,
) (*api.PutAlertManagerDefinitionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	err = validateData(in.Data)
	if err != nil {
		return nil, err
	}

	recordKey := key(scope, kindAlertManagerDefinition, workspaceID)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		r, found, e := storage.Read[alertManagerRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("AlertManagerDefinition", workspaceID)
		}

		r.Data = in.Data
		r.ModifiedAt = time.Now().UTC()
		return storage.Write(tx, recordKey, *r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutAlertManagerDefinitionResponse{
		Status: &api.AlertManagerDefinitionStatus{StatusCode: new(statusActive)},
	}, nil
}

func (s *Service) DeleteAlertManagerDefinition(
	ctx context.Context,
	in *api.DeleteAlertManagerDefinitionRequest,
) (*api.DeleteAlertManagerDefinitionOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	recordKey := key(scope, kindAlertManagerDefinition, workspaceID)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		_, found, e := storage.Read[alertManagerRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("AlertManagerDefinition", workspaceID)
		}

		tx.Delete(recordKey)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAlertManagerDefinitionOutput{}, nil
}
