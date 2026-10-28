package codecommit

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	refCreated = "referenceCreated"
	refUpdated = "referenceUpdated"
	refDeleted = "referenceDeleted"
)

// emit publishes a "CodeCommit Repository State Change" event for a branch through the same in-process
// PutEvents path as CodeBuild's events. Tags are published by emitRef with their own type.
func (s *Service) emit(ctx context.Context, r *repository, event, name, commit, old string) {
	s.emitRef(ctx, r, event, "branch", headsPrefix+name, commit, old)
}

func (s *Service) emitRef(ctx context.Context, r *repository, event, kind, full, commit, old string) {
	if s.caller == nil {
		return
	}

	// The request's own metadata carries the caller; the scope is the repository's.
	m, _ := request.FromContext(ctx)
	m.Scope = r.scope
	ctx = request.WithMetadata(ctx, m)
	detail := map[string]any{
		"event":             event,
		"repositoryName":    r.name,
		"repositoryId":      r.id,
		"referenceType":     kind,
		"referenceName":     strings.TrimPrefix(strings.TrimPrefix(full, headsPrefix), tagsPrefix),
		"referenceFullName": full,
		"callerUserArn":     request.CallerARN(ctx),
	}
	if commit != "" {
		detail["commitId"] = commit
	}

	if old != "" {
		detail["oldCommitId"] = old
	}

	encoded, err := json.Marshal(detail)
	if err != nil {
		return
	}

	_, _ = s.caller.Call(ctx, "eventbridge", "PutEvents", map[string]any{"Entries": []any{
		map[string]any{
			"Source":     "aws.codecommit",
			"DetailType": "CodeCommit Repository State Change",
			"Detail":     string(encoded),
			"Resources":  []any{common.Value(r.meta.Arn)},
		},
	}})
}

// emitChanges publishes the events for the references that differ between two snapshots.
func (s *Service) emitChanges(ctx context.Context, r *repository, before, after map[string]string) {
	for full, commit := range after {
		kind := "branch"
		switch {
		case strings.HasPrefix(full, tagsPrefix):
			kind = "tag"
		case !strings.HasPrefix(full, headsPrefix):
			continue
		}

		old, existed := before[full]
		switch {
		case !existed:
			s.emitRef(ctx, r, refCreated, kind, full, commit, "")
		case old != commit:
			s.emitRef(ctx, r, refUpdated, kind, full, commit, old)
		}
	}

	for full, old := range before {
		if _, kept := after[full]; kept {
			continue
		}

		switch {
		case strings.HasPrefix(full, headsPrefix):
			s.emitRef(ctx, r, refDeleted, "branch", full, "", old)
		case strings.HasPrefix(full, tagsPrefix):
			s.emitRef(ctx, r, refDeleted, "tag", full, "", old)
		}
	}
}
