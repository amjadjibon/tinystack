package eventbridge

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// targetService returns the service of a target ARN in the caller's account
// and region. TinyStack delivers only to SQS queues, SNS topics, and Lambda
// functions there.
func targetService(scope request.Scope, arn string) (string, error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] == "" || parts[5] == "" || !accountID.MatchString(parts[4]) {
		return "", invalid("Parameter %s is not valid. Reason: Provided Arn is not in correct format.", arn)
	}

	switch parts[2] {
	case serviceSQS, serviceSNS, serviceLambda:
	default:
		return "", invalid("TinyStack does not support %s targets.", parts[2])
	}

	if parts[3] != scope.Region || parts[4] != scope.AccountID {
		return "", invalid("TinyStack delivers only to targets in the rule's account and region: %s.", arn)
	}

	return parts[2], nil
}

func newTarget(scope request.Scope, in *api.Target) (target, error) {
	t := target{
		ID:        common.Value(in.Id),
		ARN:       common.Value(in.Arn),
		RoleARN:   common.Value(in.RoleArn),
		Input:     common.Value(in.Input),
		InputPath: common.Value(in.InputPath),
	}
	if !targetID.MatchString(t.ID) {
		return t, invalid("Target Id must be 1 to 64 characters from [.-_A-Za-z0-9].")
	}

	service, err := targetService(scope, t.ARN)
	if err != nil {
		return t, err
	}

	if (in.Input != nil && in.InputPath != nil) || (in.InputTransformer != nil && (in.Input != nil ||
		in.InputPath != nil)) {
		return t, invalid("Only one of Input, InputPath, or InputTransformer can be specified for target %s.", t.ID)
	}

	if in.InputTransformer != nil {
		if t.Transformer, err = newTransformer(t.ID, in.InputTransformer); err != nil {
			return t, err
		}
	}

	if in.Input != nil && (len(t.Input) > 8192 || !json.Valid([]byte(t.Input))) {
		return t, invalid("Input for target %s must be valid JSON of at most 8192 characters.", t.ID)
	}

	if in.InputPath != nil && (len(t.InputPath) > maxInputPath || !inputPath.MatchString(t.InputPath)) {
		return t, invalid("TinyStack supports only $ followed by dotted fields and [index] steps in InputPaths " +
			"such as $.detail.items[0].id.")
	}

	if in.RetryPolicy != nil || in.DeadLetterConfig != nil {
		var retries, age *int64
		if in.RetryPolicy != nil {
			retries, age = in.RetryPolicy.MaximumRetryAttempts, in.RetryPolicy.MaximumEventAgeInSeconds
		}

		var arn *string
		if in.DeadLetterConfig != nil {
			arn = in.DeadLetterConfig.Arn
		}

		if policyErr := t.retryPolicy(scope, retries, age, arn); policyErr != nil {
			return t, policyErr
		}
	}

	if in.SqsParameters != nil {
		if service != serviceSQS {
			return t, invalid("SqsParameters apply only to SQS targets.")
		}

		t.MessageGroupID = common.Value(in.SqsParameters.MessageGroupId)
		if len(t.MessageGroupID) > 100 {
			return t, invalid("MessageGroupId must be at most 100 characters.")
		}
	}

	return t, nil
}

func (s *Service) PutTargets(ctx context.Context, in *api.PutTargetsRequest) (*api.PutTargetsResponse, error) {
	ref, err := resolveRule(ctx, in.Rule, in.EventBusName)
	if err != nil {
		return nil, err
	}

	if len(in.Targets) == 0 || len(in.Targets) > pageSize {
		return nil, invalid("Targets must contain between 1 and %d targets.", pageSize)
	}

	targets := make([]target, 0, len(in.Targets))
	for i := range in.Targets {
		t, targetErr := newTarget(ref.scope, &in.Targets[i])
		if targetErr != nil {
			return nil, targetErr
		}

		if slices.ContainsFunc(targets, func(o target) bool { return o.ID == t.ID }) {
			return nil, invalid("Target ids must be unique within the request: %s.", t.ID)
		}

		targets = append(targets, t)
	}

	err = s.update(ctx, &ref, func(r *rule) error {
		for j := range targets {
			t := &targets[j]
			i := slices.IndexFunc(r.Targets, func(o target) bool { return o.ID == t.ID })
			if i >= 0 {
				r.Targets[i] = *t
			} else {
				r.Targets = append(r.Targets, *t)
			}
		}

		if len(r.Targets) > maxTargets {
			return awserror.New(
				"LimitExceededException",
				"The requested resource exceeds the maximum number allowed.",
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutTargetsResponse{FailedEntryCount: new(int64(0))}, nil
}

// RemoveTargets succeeds for IDs the rule does not have, as EventBridge does.
func (s *Service) RemoveTargets(ctx context.Context, in *api.RemoveTargetsRequest) (*api.RemoveTargetsResponse, error) {
	ref, err := resolveRule(ctx, in.Rule, in.EventBusName)
	if err != nil {
		return nil, err
	}

	if len(in.Ids) == 0 || len(in.Ids) > pageSize {
		return nil, invalid("Ids must contain between 1 and %d target ids.", pageSize)
	}

	for _, id := range in.Ids {
		if !targetID.MatchString(id) {
			return nil, invalid("Target Id must be 1 to 64 characters from [.-_A-Za-z0-9].")
		}
	}

	err = s.update(ctx, &ref, func(r *rule) error {
		r.Targets = slices.DeleteFunc(r.Targets, func(t target) bool { return slices.Contains(in.Ids, t.ID) })
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveTargetsResponse{FailedEntryCount: new(int64(0))}, nil
}

func (t *target) describe() api.Target {
	out := api.Target{Id: new(t.ID), Arn: new(t.ARN)}
	if t.RoleARN != "" {
		out.RoleArn = new(t.RoleARN)
	}

	if t.Input != "" {
		out.Input = new(t.Input)
	}

	if t.InputPath != "" {
		out.InputPath = new(t.InputPath)
	}

	if t.MaxRetries != nil || t.MaxAge != nil {
		out.RetryPolicy = &api.RetryPolicy{MaximumRetryAttempts: t.MaxRetries, MaximumEventAgeInSeconds: t.MaxAge}
	}

	if t.DeadLetterARN != "" {
		out.DeadLetterConfig = &api.DeadLetterConfig{Arn: new(t.DeadLetterARN)}
	}

	if t.MessageGroupID != "" {
		out.SqsParameters = &api.SqsParameters{MessageGroupId: new(t.MessageGroupID)}
	}

	if t.Transformer != nil {
		out.InputTransformer = &api.InputTransformer{
			InputPathsMap: t.Transformer.Paths,
			InputTemplate: new(t.Transformer.Template),
		}
	}

	return out
}

func (s *Service) ListTargetsByRule(
	ctx context.Context,
	in *api.ListTargetsByRuleRequest,
) (*api.ListTargetsByRuleResponse, error) {
	ref, err := resolveRule(ctx, in.Rule, in.EventBusName)
	if err != nil {
		return nil, err
	}

	after, size, err := page(in.NextToken, in.Limit)
	if err != nil {
		return nil, err
	}

	var targets []target
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, loadErr := loadRule(tx, &ref)
		if loadErr == nil {
			targets = r.Targets
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	targets = slices.DeleteFunc(targets, func(t target) bool { return t.ID <= after })
	out := &api.ListTargetsByRuleResponse{}
	if len(targets) > size {
		targets = targets[:size]
		out.NextToken = nextToken(targets[size-1].ID)
	}

	for i := range targets {
		out.Targets = append(out.Targets, targets[i].describe())
	}

	return out, nil
}
