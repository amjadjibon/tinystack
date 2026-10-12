// Package codepipeline stores pipeline declarations and their tags. Nothing runs: there are no executions, stage
// transitions, approvals, jobs, webhooks or triggers, so a pipeline's state only lists its stages and actions.
package codepipeline

import (
	"errors"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName  = "codepipeline"
	pipelineKind = "pipeline"
)

var _ api.API = (*Service)(nil)

type (
	Service struct{ store storage.Store }

	// record is a pipeline as stored. Only the current version of the declaration is kept.
	record struct {
		Tags     map[string]string
		Pipeline api.PipelineDeclaration
		Created  float64
		Updated  float64
	}
)

func New(store storage.Store) *Service { return &Service{store: store} }

func invalidStructure(format string, args ...any) error {
	return awserror.New("InvalidStructureException", fmt.Sprintf(format, args...))
}

func invalidStage(format string, args ...any) error {
	return awserror.New("InvalidStageDeclarationException", fmt.Sprintf(format, args...))
}

func invalidAction(format string, args ...any) error {
	return awserror.New("InvalidActionDeclarationException", fmt.Sprintf(format, args...))
}

func invalidTags(format string, args ...any) error {
	return awserror.New("InvalidTagsException", fmt.Sprintf(format, args...))
}

func pipelineNotFound(name string) error {
	return awserror.New(
		"PipelineNotFoundException", fmt.Sprintf("Account does not have a pipeline with name '%s'", name),
	)
}

func pipelineARN(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:codepipeline:%s:%s:%s", scope.Region, scope.AccountID, name)
}

// tagError reports the way a tag set breaks the shared limits.
func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TooManyTagsException", err.Error())
	}

	return invalidTags("%s", err.Error())
}

// tagMap returns the tags a caller sets, checked against the shared limits.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) {
		return t.Key, t.Value
	}, invalidTags("Each tag needs a key and a value"), func(key string) error {
		return invalidTags("Duplicate tag key %s", key)
	})
	if err != nil {
		return nil, err
	}

	if checkErr := tagging.CheckSet(out); checkErr != nil {
		return nil, tagError(checkErr)
	}

	return out, nil
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag {
		return api.Tag{Key: new(k), Value: new(v)}
	})
}
