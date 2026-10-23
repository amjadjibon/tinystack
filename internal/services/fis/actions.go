package fis

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// GetAction describes the executable local wait action.
func (s *Service) GetAction(ctx context.Context, in *api.GetActionRequest) (*api.GetActionResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if common.Value(in.Id) != waitAction {
		return nil, missing()
	}

	return &api.GetActionResponse{Action: &api.Action{
		Id: new(waitAction), Arn: new(fmt.Sprintf("arn:aws:fis:%s::action/%s", scope.Region, waitAction)),
		Description: new("Wait for the specified ISO 8601 duration"),
		Parameters: map[string]api.ActionParameter{"duration": {
			Description: new("ISO 8601 duration between one minute and 12 hours"), Required: new(true),
		}},
	}}, nil
}

// ListActions lists only actions that TinyStack can execute.
func (s *Service) ListActions(ctx context.Context, in *api.ListActionsRequest) (*api.ListActionsResponse, error) {
	action, err := s.GetAction(ctx, &api.GetActionRequest{Id: new(waitAction)})
	if err != nil {
		return nil, err
	}

	a := action.Action
	items, next, err := common.Page([]api.ActionSummary{{Id: a.Id, Arn: a.Arn, Description: a.Description}},
		func(a api.ActionSummary) string { return common.Value(a.Id) }, in.NextToken,
		int(common.Value(common.OrElse(in.MaxResults, new(int64(100))))), awserror.Validation("Invalid nextToken"))
	return &api.ListActionsResponse{Actions: items, NextToken: next}, err
}

// ListTargetResourceTypes returns no types because the wait action has no targets.
func (s *Service) ListTargetResourceTypes(
	_ context.Context, in *api.ListTargetResourceTypesRequest,
) (*api.ListTargetResourceTypesResponse, error) {
	if in.NextToken != nil {
		return nil, awserror.Validation("Invalid nextToken")
	}

	return &api.ListTargetResourceTypesResponse{TargetResourceTypes: []api.TargetResourceTypeSummary{}}, nil
}

// GetTargetResourceType reports that no target resource types are executable.
func (s *Service) GetTargetResourceType(
	_ context.Context, _ *api.GetTargetResourceTypeRequest,
) (*api.GetTargetResourceTypeResponse, error) {
	return nil, missing()
}
