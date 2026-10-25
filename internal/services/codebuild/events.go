package codebuild

import (
	"context"
	"encoding/json"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// EventBridge uses the same in-process PutEvents path as S3 notifications.
func (s *Service) emit(ctx context.Context, scope request.Scope, b *api.Build, phase bool) {
	if s.caller == nil {
		return
	}

	detail := map[string]any{"build-id": common.Value(b.Arn), "project-name": common.Value(b.ProjectName)}
	kind := "CodeBuild Build State Change"
	if phase {
		kind = "CodeBuild Build Phase Change"
		if len(b.Phases) == 0 {
			return
		}

		p := b.Phases[len(b.Phases)-1]
		detail["completed-phase"] = common.Value(p.PhaseType)
		detail["completed-phase-status"] = common.Value(p.PhaseStatus)
		detail["completed-phase-duration-seconds"] = common.Value(p.DurationInSeconds)
	} else {
		detail["build-status"] = common.Value(b.BuildStatus)
	}

	encoded, err := json.Marshal(detail)
	if err != nil {
		return
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
	_, _ = s.caller.Call(ctx, "eventbridge", "PutEvents", map[string]any{"Entries": []any{
		map[string]any{
			"Source":     "aws.codebuild",
			"DetailType": kind,
			"Detail":     string(encoded),
			"Resources":  []any{common.Value(b.Arn)},
		},
	}})
}
