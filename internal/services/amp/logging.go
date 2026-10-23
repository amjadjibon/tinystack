package amp

import (
	"context"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var logGroupArnRegex = regexp.MustCompile(
	`^arn:aws[a-z0-9-]*:logs:[a-z0-9-]+:[0-9]{12}:log-group:[A-Za-z0-9\.\-\_\#/]{1,512}\:\*$`,
)

type loggingRecord struct {
	CreatedAt   time.Time `json:"createdAt"`
	ModifiedAt  time.Time `json:"modifiedAt"`
	WorkspaceID string    `json:"workspaceId"`
	LogGroupArn string    `json:"logGroupArn"`
}

func validateLogGroupArn(arn string) error {
	if arn == "" || !logGroupArnRegex.MatchString(arn) {
		return awserror.Validation("Invalid logGroupArn: %s", arn)
	}

	return nil
}

func (s *Service) CreateLoggingConfiguration(
	ctx context.Context,
	in *api.CreateLoggingConfigurationRequest,
) (*api.CreateLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	logGroupArn := common.Value(in.LogGroupArn)
	err = validateLogGroupArn(logGroupArn)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	record := loggingRecord{
		WorkspaceID: workspaceID,
		LogGroupArn: logGroupArn,
		CreatedAt:   now,
		ModifiedAt:  now,
	}

	recordKey := key(scope, kindLoggingConfiguration, workspaceID)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		_, found, e := storage.Read[loggingRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if found {
			return conflict("LoggingConfiguration already exists")
		}

		return storage.Write(tx, recordKey, record)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLoggingConfigurationResponse{
		Status: &api.LoggingConfigurationStatus{StatusCode: new(statusActive)},
	}, nil
}

func (s *Service) DescribeLoggingConfiguration(
	ctx context.Context,
	in *api.DescribeLoggingConfigurationRequest,
) (*api.DescribeLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	var record *loggingRecord
	recordKey := key(scope, kindLoggingConfiguration, workspaceID)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		r, found, e := storage.Read[loggingRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("LoggingConfiguration", workspaceID)
		}

		record = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeLoggingConfigurationResponse{
		LoggingConfiguration: &api.LoggingConfigurationMetadata{
			CreatedAt:   common.Seconds(record.CreatedAt),
			LogGroupArn: new(record.LogGroupArn),
			ModifiedAt:  common.Seconds(record.ModifiedAt),
			Status:      &api.LoggingConfigurationStatus{StatusCode: new(statusActive)},
			Workspace:   new(record.WorkspaceID),
		},
	}, nil
}

func (s *Service) UpdateLoggingConfiguration(
	ctx context.Context,
	in *api.UpdateLoggingConfigurationRequest,
) (*api.UpdateLoggingConfigurationResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	logGroupArn := common.Value(in.LogGroupArn)
	err = validateLogGroupArn(logGroupArn)
	if err != nil {
		return nil, err
	}

	recordKey := key(scope, kindLoggingConfiguration, workspaceID)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		r, found, e := storage.Read[loggingRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("LoggingConfiguration", workspaceID)
		}

		r.LogGroupArn = logGroupArn
		r.ModifiedAt = time.Now().UTC()
		return storage.Write(tx, recordKey, *r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateLoggingConfigurationResponse{
		Status: &api.LoggingConfigurationStatus{StatusCode: new(statusActive)},
	}, nil
}

func (s *Service) DeleteLoggingConfiguration(
	ctx context.Context,
	in *api.DeleteLoggingConfigurationRequest,
) (*api.DeleteLoggingConfigurationOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	recordKey := key(scope, kindLoggingConfiguration, workspaceID)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		_, found, e := storage.Read[loggingRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("LoggingConfiguration", workspaceID)
		}

		tx.Delete(recordKey)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteLoggingConfigurationOutput{}, nil
}
