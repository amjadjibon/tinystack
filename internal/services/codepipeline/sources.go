package codepipeline

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// S3SourceChanged consumes the same committed object writes as S3 bucket notifications.
// Each write starts at most one execution per pipeline, even with multiple matching actions.
func (s *Service) S3SourceChanged(ctx context.Context, scope request.Scope, bucket, key, version string) {
	var pipelines []*record
	err := s.store.View(ctx, func(tx storage.ReadTx) (e error) {
		pipelines, e = storage.Records[record](tx, scope.Prefix(serviceName, pipelineKind))
		return e
	})
	if err != nil {
		return
	}

	ctx = request.WithMetadata(ctx, request.Metadata{Scope: scope})
	for _, p := range pipelines {
		values := map[string]string{}
		for _, v := range p.Pipeline.Variables {
			values["variables."+*v.Name] = common.Value(v.DefaultValue)
		}

		input := &api.StartPipelineExecutionInput{Name: p.Pipeline.Name}
		matched := false
		for _, action := range p.Pipeline.Stages[0].Actions {
			if *action.ActionTypeId.Provider != "S3" || *action.ActionTypeId.Owner != ownerAWS {
				continue
			}

			config := action.Configuration
			sourceBucket, bucketErr := resolve(config["S3Bucket"], values)
			sourceKey, keyErr := resolve(config["S3ObjectKey"], values)
			poll, pollErr := resolve(config["PollForSourceChanges"], values)
			if bucketErr != nil || keyErr != nil || pollErr != nil ||
				sourceBucket != bucket || sourceKey != key || (poll != "" && poll != "true") {
				continue
			}

			matched = true
			if version != "" && version != "null" {
				input.SourceRevisions = append(input.SourceRevisions, api.SourceRevisionOverride{
					ActionName: action.Name, RevisionType: new("S3_OBJECT_VERSION_ID"), RevisionValue: new(version),
				})
			}
		}

		if matched {
			_, _ = s.startExecution(ctx, input, "PollForSourceChanges", scope, p)
		}
	}
}
