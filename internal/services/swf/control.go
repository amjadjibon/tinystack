package swf

import (
	"context"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) RegisterDomain(ctx context.Context, in *api.RegisterDomainInput) (*api.RegisterDomainOutput, error) {
	retention := common.Value(in.WorkflowExecutionRetentionPeriodInDays)
	if retention != timeoutNone {
		n, err := strconv.Atoi(retention)
		if err != nil || n < 0 || n > 90 {
			return nil, invalid("Retention must be 0..90 days or NONE")
		}
	}

	tags, err := mergeTags(nil, in.Tags)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		key := domainKey(scope, common.Value(in.Name))
		if _, ok := tx.Get(key); ok {
			return fault("DomainAlreadyExistsFault", "Domain already exists")
		}

		return storage.Write(
			tx,
			key,
			domainRecord{
				Detail: api.DomainDetail{
					DomainInfo: &api.DomainInfo{
						Name:        in.Name,
						Status:      new(statusRegistered),
						Description: in.Description,
						Arn:         new(arn(scope, common.Value(in.Name))),
					},
					Configuration: &api.DomainConfiguration{
						WorkflowExecutionRetentionPeriodInDays: in.WorkflowExecutionRetentionPeriodInDays,
					},
				},
				Tags: tags,
			},
		)
	})
	return &api.RegisterDomainOutput{}, err
}

func (s *Service) DescribeDomain(ctx context.Context, in *api.DescribeDomainInput) (*api.DomainDetail, error) {
	var out *api.DomainDetail
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readDomain(tx, scope, common.Value(in.Name), false)
		if err != nil {
			return err
		}

		out = &r.Detail
		return nil
	})
	return out, err
}

func (s *Service) ListDomains(ctx context.Context, in *api.ListDomainsInput) (*api.DomainInfos, error) {
	out := &api.DomainInfos{DomainInfos: []api.DomainInfo{}}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		rs, err := storage.Records[domainRecord](tx, scope.Prefix(serviceName, "domain"))
		if err != nil {
			return err
		}

		for _, r := range rs {
			if common.Value(r.Detail.DomainInfo.Status) == common.Value(in.RegistrationStatus) {
				out.DomainInfos = append(out.DomainInfos, *r.Detail.DomainInfo)
			}
		}

		if common.Value(in.ReverseOrder) {
			slices.Reverse(out.DomainInfos)
		}

		out.DomainInfos, out.NextPageToken, err = common.Page(
			out.DomainInfos,
			func(r api.DomainInfo) string { return common.Value(r.Name) },
			in.NextPageToken,
			limit(in.MaximumPageSize),
			invalid("Invalid nextPageToken"),
		)
		return err
	})
	return out, err
}

func (s *Service) DeprecateDomain(
	ctx context.Context,
	in *api.DeprecateDomainInput,
) (*api.DeprecateDomainOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readDomain(tx, scope, common.Value(in.Name), false)
		if err != nil {
			return err
		}

		if common.Value(r.Detail.DomainInfo.Status) == statusDeprecated {
			return fault("DomainDeprecatedFault", "Resource is already deprecated")
		}

		r.Detail.DomainInfo.Status = new(statusDeprecated)
		return storage.Write(tx, domainKey(scope, common.Value(in.Name)), r)
	})
	return &api.DeprecateDomainOutput{}, err
}

func (s *Service) UndeprecateDomain(
	ctx context.Context,
	in *api.UndeprecateDomainInput,
) (*api.UndeprecateDomainOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := readDomain(tx, scope, common.Value(in.Name), false)
		if err != nil {
			return err
		}

		if common.Value(r.Detail.DomainInfo.Status) == statusRegistered {
			return fault("DomainAlreadyExistsFault", "Resource is already registered")
		}

		r.Detail.DomainInfo.Status = new(statusRegistered)
		return storage.Write(tx, domainKey(scope, common.Value(in.Name)), r)
	})
	return &api.UndeprecateDomainOutput{}, err
}

func (s *Service) RegisterWorkflowType(
	ctx context.Context,
	in *api.RegisterWorkflowTypeInput,
) (*api.RegisterWorkflowTypeOutput, error) {
	if err := timeout(in.DefaultTaskStartToCloseTimeout, true); err != nil {
		return nil, err
	}

	if err := timeout(in.DefaultExecutionStartToCloseTimeout, false); err != nil {
		return nil, err
	}

	if err := priority(in.DefaultTaskPriority); err != nil {
		return nil, err
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), true); err != nil {
			return err
		}

		key := typeKey(scope, common.Value(in.Domain), "workflow-type", common.Value(in.Name), common.Value(in.Version))
		if _, ok := tx.Get(key); ok {
			return fault("TypeAlreadyExistsFault", "Type already exists")
		}

		r := typeRecord{
			Workflow: &api.WorkflowTypeDetail{
				TypeInfo: &api.WorkflowTypeInfo{
					WorkflowType: &api.WorkflowType{Name: in.Name, Version: in.Version},
					Status:       new(statusRegistered),
					Description:  in.Description,
					CreationDate: new(now()),
				},
				Configuration: &api.WorkflowTypeConfiguration{
					DefaultTaskStartToCloseTimeout:      in.DefaultTaskStartToCloseTimeout,
					DefaultExecutionStartToCloseTimeout: in.DefaultExecutionStartToCloseTimeout,
					DefaultTaskList:                     in.DefaultTaskList,
					DefaultTaskPriority:                 in.DefaultTaskPriority,
					DefaultChildPolicy:                  in.DefaultChildPolicy,
					DefaultLambdaRole:                   in.DefaultLambdaRole,
				},
			},
		}

		return storage.Write(tx, key, r)
	})
	return &api.RegisterWorkflowTypeOutput{}, err
}

func (s *Service) DescribeWorkflowType(
	ctx context.Context,
	in *api.DescribeWorkflowTypeInput,
) (*api.WorkflowTypeDetail, error) {
	var out *api.WorkflowTypeDetail
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"workflow-type",
			common.Value(in.WorkflowType.Name),
			common.Value(in.WorkflowType.Version),
		)
		if err != nil {
			return err
		}

		out = r.Workflow
		return nil
	})
	return out, err
}

func (s *Service) ListWorkflowTypes(
	ctx context.Context,
	in *api.ListWorkflowTypesInput,
) (*api.WorkflowTypeInfos, error) {
	out := &api.WorkflowTypeInfos{TypeInfos: []api.WorkflowTypeInfo{}}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		rs, err := storage.Records[typeRecord](
			tx,
			scope.Prefix(serviceName, "workflow-type")+common.Value(in.Domain)+"/",
		)
		if err != nil {
			return err
		}

		for _, r := range rs {
			info := r.Workflow.TypeInfo
			if common.Value(info.Status) == common.Value(in.RegistrationStatus) &&
				(in.Name == nil || *in.Name == common.Value(info.WorkflowType.Name)) {
				out.TypeInfos = append(out.TypeInfos, *info)
			}
		}

		if common.Value(in.ReverseOrder) {
			slices.Reverse(out.TypeInfos)
		}

		out.TypeInfos, out.NextPageToken, err = common.Page(out.TypeInfos, func(r api.WorkflowTypeInfo) string {
			return common.Value(r.WorkflowType.Name) + "/" + common.Value(r.WorkflowType.Version)
		}, in.NextPageToken, limit(in.MaximumPageSize), invalid("Invalid nextPageToken"))
		return err
	})
	return out, err
}

func (s *Service) DeprecateWorkflowType(
	ctx context.Context,
	in *api.DeprecateWorkflowTypeInput,
) (*api.DeprecateWorkflowTypeOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"workflow-type",
			common.Value(in.WorkflowType.Name),
			common.Value(in.WorkflowType.Version),
		)
		if err != nil {
			return err
		}

		key := typeKey(
			scope,
			common.Value(in.Domain),
			"workflow-type",
			common.Value(in.WorkflowType.Name),
			common.Value(in.WorkflowType.Version),
		)
		if common.Value(r.Workflow.TypeInfo.Status) == statusDeprecated {
			return fault("TypeDeprecatedFault", "Resource is already deprecated")
		}

		r.Workflow.TypeInfo.Status = new(statusDeprecated)
		r.Workflow.TypeInfo.DeprecationDate = new(now())
		return storage.Write(tx, key, r)
	})
	return &api.DeprecateWorkflowTypeOutput{}, err
}

func (s *Service) UndeprecateWorkflowType(
	ctx context.Context,
	in *api.UndeprecateWorkflowTypeInput,
) (*api.UndeprecateWorkflowTypeOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"workflow-type",
			common.Value(in.WorkflowType.Name),
			common.Value(in.WorkflowType.Version),
		)
		if err != nil {
			return err
		}

		key := typeKey(
			scope,
			common.Value(in.Domain),
			"workflow-type",
			common.Value(in.WorkflowType.Name),
			common.Value(in.WorkflowType.Version),
		)
		if common.Value(r.Workflow.TypeInfo.Status) == statusRegistered {
			return fault("TypeAlreadyExistsFault", "Resource is already registered")
		}

		r.Workflow.TypeInfo.Status = new(statusRegistered)
		r.Workflow.TypeInfo.DeprecationDate = nil
		return storage.Write(tx, key, r)
	})
	return &api.UndeprecateWorkflowTypeOutput{}, err
}

func (s *Service) DeleteWorkflowType(
	ctx context.Context,
	in *api.DeleteWorkflowTypeInput,
) (*api.DeleteWorkflowTypeOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"workflow-type",
			common.Value(in.WorkflowType.Name),
			common.Value(in.WorkflowType.Version),
		)
		if err != nil {
			return err
		}

		key := typeKey(
			scope,
			common.Value(in.Domain),
			"workflow-type",
			common.Value(in.WorkflowType.Name),
			common.Value(in.WorkflowType.Version),
		)
		if common.Value(r.Workflow.TypeInfo.Status) != statusDeprecated {
			return fault("TypeNotDeprecatedFault", "Deprecate the type before deleting it")
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteWorkflowTypeOutput{}, err
}

func (s *Service) RegisterActivityType(
	ctx context.Context,
	in *api.RegisterActivityTypeInput,
) (*api.RegisterActivityTypeOutput, error) {
	if err := timeout(in.DefaultTaskStartToCloseTimeout, true); err != nil {
		return nil, err
	}

	if err := timeout(in.DefaultTaskHeartbeatTimeout, true); err != nil {
		return nil, err
	}

	if err := timeout(in.DefaultTaskScheduleToStartTimeout, true); err != nil {
		return nil, err
	}

	if err := timeout(in.DefaultTaskScheduleToCloseTimeout, true); err != nil {
		return nil, err
	}

	if err := priority(in.DefaultTaskPriority); err != nil {
		return nil, err
	}

	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), true); err != nil {
			return err
		}

		key := typeKey(scope, common.Value(in.Domain), "activity-type", common.Value(in.Name), common.Value(in.Version))
		if _, ok := tx.Get(key); ok {
			return fault("TypeAlreadyExistsFault", "Type already exists")
		}

		r := typeRecord{
			Activity: &api.ActivityTypeDetail{
				TypeInfo: &api.ActivityTypeInfo{
					ActivityType: &api.ActivityType{Name: in.Name, Version: in.Version},
					Status:       new(statusRegistered),
					Description:  in.Description,
					CreationDate: new(now()),
				},
				Configuration: &api.ActivityTypeConfiguration{
					DefaultTaskStartToCloseTimeout:    in.DefaultTaskStartToCloseTimeout,
					DefaultTaskHeartbeatTimeout:       in.DefaultTaskHeartbeatTimeout,
					DefaultTaskScheduleToStartTimeout: in.DefaultTaskScheduleToStartTimeout,
					DefaultTaskScheduleToCloseTimeout: in.DefaultTaskScheduleToCloseTimeout,
					DefaultTaskList:                   in.DefaultTaskList,
					DefaultTaskPriority:               in.DefaultTaskPriority,
				},
			},
		}

		return storage.Write(tx, key, r)
	})
	return &api.RegisterActivityTypeOutput{}, err
}

func (s *Service) DescribeActivityType(
	ctx context.Context,
	in *api.DescribeActivityTypeInput,
) (*api.ActivityTypeDetail, error) {
	var out *api.ActivityTypeDetail
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"activity-type",
			common.Value(in.ActivityType.Name),
			common.Value(in.ActivityType.Version),
		)
		if err != nil {
			return err
		}

		out = r.Activity
		return nil
	})
	return out, err
}

func (s *Service) ListActivityTypes(
	ctx context.Context,
	in *api.ListActivityTypesInput,
) (*api.ActivityTypeInfos, error) {
	out := &api.ActivityTypeInfos{TypeInfos: []api.ActivityTypeInfo{}}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		rs, err := storage.Records[typeRecord](
			tx,
			scope.Prefix(serviceName, "activity-type")+common.Value(in.Domain)+"/",
		)
		if err != nil {
			return err
		}

		for _, r := range rs {
			info := r.Activity.TypeInfo
			if common.Value(info.Status) == common.Value(in.RegistrationStatus) &&
				(in.Name == nil || *in.Name == common.Value(info.ActivityType.Name)) {
				out.TypeInfos = append(out.TypeInfos, *info)
			}
		}

		if common.Value(in.ReverseOrder) {
			slices.Reverse(out.TypeInfos)
		}

		out.TypeInfos, out.NextPageToken, err = common.Page(out.TypeInfos, func(r api.ActivityTypeInfo) string {
			return common.Value(r.ActivityType.Name) + "/" + common.Value(r.ActivityType.Version)
		}, in.NextPageToken, limit(in.MaximumPageSize), invalid("Invalid nextPageToken"))
		return err
	})
	return out, err
}

func (s *Service) DeprecateActivityType(
	ctx context.Context,
	in *api.DeprecateActivityTypeInput,
) (*api.DeprecateActivityTypeOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"activity-type",
			common.Value(in.ActivityType.Name),
			common.Value(in.ActivityType.Version),
		)
		if err != nil {
			return err
		}

		key := typeKey(
			scope,
			common.Value(in.Domain),
			"activity-type",
			common.Value(in.ActivityType.Name),
			common.Value(in.ActivityType.Version),
		)
		if common.Value(r.Activity.TypeInfo.Status) == statusDeprecated {
			return fault("TypeDeprecatedFault", "Resource is already deprecated")
		}

		r.Activity.TypeInfo.Status = new(statusDeprecated)
		r.Activity.TypeInfo.DeprecationDate = new(now())
		return storage.Write(tx, key, r)
	})
	return &api.DeprecateActivityTypeOutput{}, err
}

func (s *Service) UndeprecateActivityType(
	ctx context.Context,
	in *api.UndeprecateActivityTypeInput,
) (*api.UndeprecateActivityTypeOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"activity-type",
			common.Value(in.ActivityType.Name),
			common.Value(in.ActivityType.Version),
		)
		if err != nil {
			return err
		}

		key := typeKey(
			scope,
			common.Value(in.Domain),
			"activity-type",
			common.Value(in.ActivityType.Name),
			common.Value(in.ActivityType.Version),
		)
		if common.Value(r.Activity.TypeInfo.Status) == statusRegistered {
			return fault("TypeAlreadyExistsFault", "Resource is already registered")
		}

		r.Activity.TypeInfo.Status = new(statusRegistered)
		r.Activity.TypeInfo.DeprecationDate = nil
		return storage.Write(tx, key, r)
	})
	return &api.UndeprecateActivityTypeOutput{}, err
}

func (s *Service) DeleteActivityType(
	ctx context.Context,
	in *api.DeleteActivityTypeInput,
) (*api.DeleteActivityTypeOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := readDomain(tx, scope, common.Value(in.Domain), false); err != nil {
			return err
		}

		r, err := readType(
			tx,
			scope,
			common.Value(in.Domain),
			"activity-type",
			common.Value(in.ActivityType.Name),
			common.Value(in.ActivityType.Version),
		)
		if err != nil {
			return err
		}

		key := typeKey(
			scope,
			common.Value(in.Domain),
			"activity-type",
			common.Value(in.ActivityType.Name),
			common.Value(in.ActivityType.Version),
		)
		if common.Value(r.Activity.TypeInfo.Status) != statusDeprecated {
			return fault("TypeNotDeprecatedFault", "Deprecate the type before deleting it")
		}

		tx.Delete(key)
		return nil
	})
	return &api.DeleteActivityTypeOutput{}, err
}

func mergeTags(existing, incoming []api.ResourceTag) ([]api.ResourceTag, error) {
	current := map[string]string{}
	additions := map[string]string{}
	for _, t := range existing {
		current[common.Value(t.Key)] = common.Value(t.Value)
	}

	for _, t := range incoming {
		additions[common.Value(t.Key)] = common.Value(t.Value)
	}

	merged, err := tagging.Merge(current, additions)
	if err != nil {
		return nil, invalid(err.Error())
	}

	if len(merged) > 50 {
		return nil, fault("TooManyTagsFault", "A domain supports at most 50 tags")
	}

	out := slices.Clone(existing)
	for _, t := range incoming {
		found := false
		for i := range out {
			if common.Value(out[i].Key) == common.Value(t.Key) {
				out[i] = t
				found = true
				break
			}
		}

		if !found {
			out = append(out, t)
		}
	}

	return out, nil
}

func tagDomain(tx storage.ReadTx, scope request.Scope, value *string) (*domainRecord, error) {
	prefix := arn(scope, "")
	name, ok := strings.CutPrefix(common.Value(value), prefix)
	if !ok {
		return nil, missing()
	}

	return readDomain(tx, scope, name, false)
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := tagDomain(tx, scope, in.ResourceArn)
		if err != nil {
			return err
		}

		r.Tags, err = mergeTags(r.Tags, in.Tags)
		if err != nil {
			return err
		}

		return storage.Write(tx, domainKey(scope, common.Value(r.Detail.DomainInfo.Name)), r)
	})
	return &api.TagResourceOutput{}, err
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := tagDomain(tx, scope, in.ResourceArn)
		if err != nil {
			return err
		}

		r.Tags = slices.DeleteFunc(
			r.Tags,
			func(t api.ResourceTag) bool { return slices.Contains(in.TagKeys, common.Value(t.Key)) },
		)
		return storage.Write(tx, domainKey(scope, common.Value(r.Detail.DomainInfo.Name)), r)
	})
	return &api.UntagResourceOutput{}, err
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	out := &api.ListTagsForResourceOutput{}
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		r, err := tagDomain(tx, scope, in.ResourceArn)
		if err != nil {
			return err
		}

		out.Tags = r.Tags
		return nil
	})
	return out, err
}
