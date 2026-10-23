package datasync

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
)

// UpdateTask changes only the supplied API members.
func (s *Service) UpdateTask(ctx context.Context, in *api.UpdateTaskRequest) (*api.UpdateTaskResponse, error) {
	return s.updateTask(ctx, in, false)
}

// CloudFormationUpdateTask updates a complete template configuration in place,
// resetting removed optional properties instead of keeping API patch defaults.
func (s *Service) CloudFormationUpdateTask(
	ctx context.Context,
	in *api.UpdateTaskRequest,
) (*api.UpdateTaskResponse, error) {
	if err := in.Validate(""); err != nil {
		return nil, invalid("%v", err)
	}

	return s.updateTask(ctx, in, true)
}

// UpdateLocationEfs changes only the supplied API members.
func (s *Service) UpdateLocationEfs(
	ctx context.Context,
	in *api.UpdateLocationEfsRequest,
) (*api.UpdateLocationEfsResponse, error) {
	return s.updateLocationEfs(ctx, in, false)
}

// CloudFormationUpdateLocationEfs resets removed optional template properties
// while updating the location through the same reference and state checks.
func (s *Service) CloudFormationUpdateLocationEfs(
	ctx context.Context,
	in *api.UpdateLocationEfsRequest,
) (*api.UpdateLocationEfsResponse, error) {
	if err := in.Validate(""); err != nil {
		return nil, invalid("%v", err)
	}

	return s.updateLocationEfs(ctx, in, true)
}
