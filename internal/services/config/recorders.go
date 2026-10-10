package config

import (
	"context"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) PutConfigurationRecorder(
	ctx context.Context,
	in *api.PutConfigurationRecorderRequest,
) (*api.PutConfigurationRecorderOutput, error) {
	if in.ConfigurationRecorder == nil {
		return nil, invalid("ConfigurationRecorder is required")
	}
	recorder := common.Clone(in.ConfigurationRecorder)
	name := common.Value(common.OrElse(recorder.Name, new(defaultName)))
	if !validName(name, 256) || strings.HasPrefix(name, "AWSConfigurationRecorderFor") {
		return nil, failure("InvalidConfigurationRecorderNameException", "Invalid customer managed recorder name")
	}
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}
	rolePrefix := "arn:aws:iam::" + scope.AccountID + ":role/"
	role := common.Value(recorder.RoleARN)
	if !strings.HasPrefix(role, rolePrefix) || !roleName.MatchString(strings.TrimPrefix(role, rolePrefix)) ||
		len(role) > 2048 {
		return nil, failure("InvalidRoleException", "roleARN must be a nonempty IAM role ARN in the caller's account")
	}
	if groupErr := checkGroup(recorder.RecordingGroup); groupErr != nil {
		return nil, groupErr
	}
	if recorder.RecordingGroup == nil {
		recorder.RecordingGroup = &api.RecordingGroup{AllSupported: new(true), IncludeGlobalResourceTypes: new(false)}
	}
	tags, err := parseTags(in.Tags)
	if err != nil {
		return nil, err
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		keys := tx.Keys(scope.Prefix(serviceName, kindRecorder))
		if len(keys) > 0 && keys[0] != scope.Key(serviceName, kindRecorder, name) {
			return failure(
				"MaxNumberOfConfigurationRecordersExceededException",
				"Only one customer managed recorder per account and Region",
			)
		}
		old, found, e := storage.Read[record](tx, scope.Key(serviceName, kindRecorder, name))
		if e != nil {
			return e
		}
		r := &record{
			Name: name, Kind: kindRecorder, Recorder: recorder, Tags: tags,
			Status: &api.ConfigurationRecorderStatus{Name: new(name), Recording: new(false)},
		}
		if found {
			r.ARN = old.ARN
			r.Status = old.Status
			r.Tags = old.Tags
		} else {
			r.ARN = resourceARN(scope, "configuration-recorder/"+name+"/"+common.RandomID(7))
		}
		if recorder.Arn != nil && (!found || *recorder.Arn != r.ARN) {
			return invalid("Recorder ARN is read-only and must match the existing recorder")
		}
		recorder.Name = new(name)
		recorder.Arn = new(r.ARN)
		r.Status.Arn = new(r.ARN)
		return save(tx, scope, r)
	})
	return &api.PutConfigurationRecorderOutput{}, err
}

func (s *Service) DescribeConfigurationRecorders(
	ctx context.Context,
	in *api.DescribeConfigurationRecordersRequest,
) (*api.DescribeConfigurationRecordersResponse, error) {
	records, err := s.list(ctx, kindRecorder, in.ConfigurationRecorderNames)
	out := &api.DescribeConfigurationRecordersResponse{ConfigurationRecorders: []api.ConfigurationRecorder{}}
	for _, r := range records {
		out.ConfigurationRecorders = append(out.ConfigurationRecorders, *r.Recorder)
	}
	return out, err
}

func (s *Service) DescribeConfigurationRecorderStatus(
	ctx context.Context,
	in *api.DescribeConfigurationRecorderStatusRequest,
) (*api.DescribeConfigurationRecorderStatusResponse, error) {
	records, err := s.list(ctx, kindRecorder, in.ConfigurationRecorderNames)
	out := &api.DescribeConfigurationRecorderStatusResponse{
		ConfigurationRecordersStatus: []api.ConfigurationRecorderStatus{},
	}
	for _, r := range records {
		out.ConfigurationRecordersStatus = append(out.ConfigurationRecordersStatus, *r.Status)
	}
	return out, err
}

func (s *Service) setRecording(ctx context.Context, name string, recording bool) error {
	if name == "" {
		return invalid("ConfigurationRecorderName is required")
	}
	now := common.SecondsMilli(time.Now())
	return s.edit(ctx, kindRecorder, name, func(tx storage.WriteTx, scope request.Scope, r *record) error {
		if recording && len(tx.Keys(scope.Prefix(serviceName, kindChannel))) == 0 {
			return failure(
				"NoAvailableDeliveryChannelException",
				"A delivery channel is required before starting the recorder",
			)
		}
		// Recording is requested control-plane state only. No recording event has
		// occurred, so lastStatus and lastStatusChangeTime are intentionally absent.
		if common.Value(r.Status.Recording) != recording {
			if recording {
				r.Status.LastStartTime = now
			} else {
				r.Status.LastStopTime = now
			}
		}
		r.Status.Recording = new(recording)
		return nil
	})
}

func (s *Service) StartConfigurationRecorder(
	ctx context.Context,
	in *api.StartConfigurationRecorderRequest,
) (*api.StartConfigurationRecorderOutput, error) {
	return &api.StartConfigurationRecorderOutput{}, s.setRecording(
		ctx,
		common.Value(in.ConfigurationRecorderName),
		true,
	)
}

func (s *Service) StopConfigurationRecorder(
	ctx context.Context,
	in *api.StopConfigurationRecorderRequest,
) (*api.StopConfigurationRecorderOutput, error) {
	return &api.StopConfigurationRecorderOutput{}, s.setRecording(
		ctx,
		common.Value(in.ConfigurationRecorderName),
		false,
	)
}

func (s *Service) DeleteConfigurationRecorder(
	ctx context.Context,
	in *api.DeleteConfigurationRecorderRequest,
) (*api.DeleteConfigurationRecorderOutput, error) {
	return &api.DeleteConfigurationRecorderOutput{}, s.remove(
		ctx,
		kindRecorder,
		common.Value(in.ConfigurationRecorderName),
	)
}
