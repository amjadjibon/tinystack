package datasync

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func mergeOptions(base, update *api.Options) *api.Options {
	out := common.Clone(base)
	if out == nil {
		out = &api.Options{
			VerifyMode:                  new(verifyPointInTime),
			OverwriteMode:               new("ALWAYS"),
			PreserveDeletedFiles:        new(optionPreserve),
			TaskQueueing:                new("ENABLED"),
			LogLevel:                    new("OFF"),
			TransferMode:                new("CHANGED"),
			Atime:                       new("BEST_EFFORT"),
			Mtime:                       new(optionPreserve),
			Uid:                         new("INT_VALUE"),
			Gid:                         new("INT_VALUE"),
			PreserveDevices:             new(optionNone),
			PosixPermissions:            new(optionPreserve),
			BytesPerSecond:              new(int64(-1)),
			SecurityDescriptorCopyFlags: new("OWNER_DACL"),
			ObjectTags:                  new(optionPreserve),
			ObjectMetadata:              new(optionPreserve),
		}
	}

	if update == nil {
		return out
	}

	if update.VerifyMode != nil {
		out.VerifyMode = update.VerifyMode
	}

	if update.OverwriteMode != nil {
		out.OverwriteMode = update.OverwriteMode
	}

	if update.Atime != nil {
		out.Atime = update.Atime
	}

	if update.Mtime != nil {
		out.Mtime = update.Mtime
	}

	if update.Uid != nil {
		out.Uid = update.Uid
	}

	if update.Gid != nil {
		out.Gid = update.Gid
	}

	if update.PreserveDeletedFiles != nil {
		out.PreserveDeletedFiles = update.PreserveDeletedFiles
	}

	if update.PreserveDevices != nil {
		out.PreserveDevices = update.PreserveDevices
	}

	if update.PosixPermissions != nil {
		out.PosixPermissions = update.PosixPermissions
	}

	if update.BytesPerSecond != nil {
		out.BytesPerSecond = update.BytesPerSecond
	}

	if update.TaskQueueing != nil {
		out.TaskQueueing = update.TaskQueueing
	}

	if update.LogLevel != nil {
		out.LogLevel = update.LogLevel
	}

	if update.TransferMode != nil {
		out.TransferMode = update.TransferMode
	}

	if update.SecurityDescriptorCopyFlags != nil {
		out.SecurityDescriptorCopyFlags = update.SecurityDescriptorCopyFlags
	}

	if update.ObjectTags != nil {
		out.ObjectTags = update.ObjectTags
	}

	if update.ObjectMetadata != nil {
		out.ObjectMetadata = update.ObjectMetadata
	}

	return out
}

func validateTaskOptions(o *api.Options, includes, excludes []api.FilterRule) error {
	if o.BytesPerSecond != nil && *o.BytesPerSecond != -1 {
		return invalid("Bandwidth throttling is not supported locally; use BytesPerSecond -1")
	}

	if common.Value(o.TransferMode) == "ALL" {
		if common.Value(o.PreserveDeletedFiles) == "REMOVE" {
			return invalid("TransferMode ALL cannot be combined with PreserveDeletedFiles REMOVE")
		}

		if common.Value(o.VerifyMode) == verifyPointInTime {
			return invalid("TransferMode ALL cannot use POINT_IN_TIME_CONSISTENT verification")
		}
	}

	if _, err := compileRules(includes, true); err != nil {
		return err
	}

	if _, err := compileRules(excludes, false); err != nil {
		return err
	}

	return nil
}

func (s *Service) validateLogGroup(ctx context.Context, arn *string) error {
	if arn == nil || *arn == "" {
		return nil
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	prefix := "arn:aws:logs:" + scope.Region + ":" + scope.AccountID + ":log-group:"
	name, ok := strings.CutPrefix(*arn, prefix)
	if !ok {
		return invalid("Log group must be in this account and Region")
	}

	name = strings.TrimSuffix(name, ":*")
	var token any
	for {
		params := map[string]any{"LogGroupNamePrefix": name}
		if token != nil {
			params["NextToken"] = token
		}

		out, e := s.caller.Call(ctx, "logs", "DescribeLogGroups", params)
		if e != nil {
			return invalid("Cannot describe log group: %v", e)
		}

		groups, _ := out["LogGroups"].([]any)
		for _, item := range groups {
			group, _ := item.(map[string]any)
			if group["LogGroupName"] == name {
				return nil
			}
		}

		token = out["NextToken"]
		if token == nil {
			return invalid("CloudWatch log group %s does not exist", name)
		}
	}
}

func (s *Service) CreateTask(ctx context.Context, in *api.CreateTaskRequest) (*api.CreateTaskResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.TaskMode != nil && *in.TaskMode != modeBasic {
		return nil, invalid("Enhanced mode is not supported locally")
	}

	options := mergeOptions(nil, in.Options)
	if optionsErr := validateTaskOptions(options, in.Includes, in.Excludes); optionsErr != nil {
		return nil, optionsErr
	}

	if logErr := s.validateLogGroup(ctx, in.CloudWatchLogGroupArn); logErr != nil {
		return nil, logErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	arn := resourceARN(scope, kindTask)
	r := taskRecord{DescribeTaskResponse: api.DescribeTaskResponse{
		TaskArn: new(arn), Name: in.Name, Status: new("AVAILABLE"),
		SourceLocationArn: in.SourceLocationArn, DestinationLocationArn: in.DestinationLocationArn,
		CloudWatchLogGroupArn: in.CloudWatchLogGroupArn, Options: options, Includes: in.Includes, Excludes: in.Excludes,
		Schedule: in.Schedule, CreationTime: new(timestamp()), TaskMode: new(modeBasic),
	}, Tags: tags}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, ref := range []*string{in.SourceLocationArn, in.DestinationLocationArn} {
			if _, e := readRecord[locationRecord](tx, scope, kindLocation, common.Value(ref)); e != nil {
				return e
			}
		}

		if common.Value(in.SourceLocationArn) == common.Value(in.DestinationLocationArn) {
			return invalid("Source and destination locations must differ")
		}

		return writeRecord(tx, scope, kindTask, arn, r)
	})
	return &api.CreateTaskResponse{TaskArn: new(arn)}, err
}

func (s *Service) DescribeTask(ctx context.Context, in *api.DescribeTaskRequest) (*api.DescribeTaskResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r taskRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = readRecord[taskRecord](tx, scope, kindTask, common.Value(in.TaskArn))
		return e
	})
	return new(r.DescribeTaskResponse), err
}

func (s *Service) updateTask(
	ctx context.Context,
	in *api.UpdateTaskRequest,
	replace bool,
) (*api.UpdateTaskResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if logErr := s.validateLogGroup(ctx, in.CloudWatchLogGroupArn); logErr != nil {
		return nil, logErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord[taskRecord](tx, scope, kindTask, common.Value(in.TaskArn))
		if e != nil {
			return e
		}

		if common.Value(r.Status) == statusRunning {
			return invalid("Cannot update a running task")
		}

		if replace {
			r.Name = nil
			r.Schedule = nil
			r.CloudWatchLogGroupArn = nil
			r.Options = mergeOptions(nil, nil)
			r.Includes = nil
			r.Excludes = nil
		}

		r.Options = mergeOptions(r.Options, in.Options)
		if in.Name != nil {
			r.Name = in.Name
		}

		if in.Schedule != nil {
			r.Schedule = in.Schedule
		}

		if in.CloudWatchLogGroupArn != nil {
			r.CloudWatchLogGroupArn = in.CloudWatchLogGroupArn
		}

		if in.Includes != nil {
			r.Includes = in.Includes
		}

		if in.Excludes != nil {
			r.Excludes = in.Excludes
		}

		if optionsErr := validateTaskOptions(r.Options, r.Includes, r.Excludes); optionsErr != nil {
			return optionsErr
		}

		return writeRecord(tx, scope, kindTask, common.Value(r.TaskArn), r)
	})
	return &api.UpdateTaskResponse{}, err
}

func (s *Service) DeleteTask(ctx context.Context, in *api.DeleteTaskRequest) (*api.DeleteTaskResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	arn := common.Value(in.TaskArn)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := readRecord[taskRecord](tx, scope, kindTask, arn)
		if e != nil {
			return e
		}

		if common.Value(r.Status) == statusRunning {
			return invalid("Cannot delete a running task")
		}

		key, e := recordKey(scope, kindTask, arn)
		if e != nil {
			return e
		}

		for _, k := range tx.Keys(scope.Prefix(serviceName, kindExecution)) {
			x, _, readErr := storage.Read[executionRecord](tx, k)
			if readErr != nil {
				return readErr
			}

			if x.Task == arn {
				tx.Delete(queueKey(scope, common.Value(x.TaskExecutionArn), x.Sequence))
				tx.Delete(k)
			}
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteTaskResponse{}, err
}
