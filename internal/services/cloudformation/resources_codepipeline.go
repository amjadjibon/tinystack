package cloudformation

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

const (
	codePipelineType     = "AWS::CodePipeline::Pipeline"
	memberPipeline       = "Pipeline"
	propertyArtifactStrs = "ArtifactStores"
	propertyRestart      = "RestartExecutionOnUpdate"
)

// codePipelineDeclared are the properties that make up the pipeline declaration: a change to one of them updates
// the pipeline and so adds a version, but a change to tags alone does not.
var codePipelineDeclared = []string{
	memberRoleArn, "ArtifactStore", propertyArtifactStrs, "Stages", "PipelineType", "ExecutionMode", memberVariables,
}

// codePipelineUnsupported are the properties TinyStack accepts in a template and then refuses to apply, because they
// need triggers or stage transitions, which it does not store.
var codePipelineUnsupported = []string{"Triggers", "DisableInboundStageTransitions"}

func codePipelineARN(c *client, name string) string {
	return "arn:aws:codepipeline:" + c.scope.Region + ":" + c.scope.AccountID + ":" + name
}

func codePipelineTagger(c *client, name string) *tagger {
	return &tagger{
		service: "codepipeline",
		tag:     opTagResource,
		untag:   opUntagResource,
		target:  map[string]any{memberResourceArn: codePipelineARN(c, name)},
		key:     memberKey,
		value:   memberValue,
	}
}

// hasEntries reports whether a template sets a property to something other than an empty list.
func hasEntries(p props, k string) bool {
	v, ok := p[k]
	if l, isList := v.([]any); isList {
		return len(l) > 0
	}

	return ok
}

// codePipelineDeclaration is the pipeline declaration a template describes. The service rejects members it does not
// store, so an unsupported nested property fails; the properties that need triggers, stage transitions or
// executions fail here with a reason that names them.
func codePipelineDeclaration(name string, p props) (map[string]any, error) {
	for _, k := range codePipelineUnsupported {
		if hasEntries(p, k) {
			return nil, awserror.Validation("TinyStack does not support the %s property of %s", k, codePipelineType)
		}
	}

	if p.truth(propertyRestart) {
		return nil, awserror.Validation(
			"TinyStack does not run pipeline executions, so %s cannot be true", propertyRestart,
		)
	}

	declaration := pick(map[string]any{memberName: name}, p, codePipelineDeclared...)
	if !p.has(propertyArtifactStrs) {
		return declaration, nil
	}

	stores := map[string]any{}
	for _, entry := range p.list(propertyArtifactStrs) {
		item, _ := entry.(map[string]any)
		region, store := scalarText(item["Region"]), item["ArtifactStore"]
		if region == "" || store == nil {
			return nil, awserror.Validation("Each %s entry needs a Region and an ArtifactStore", propertyArtifactStrs)
		}

		stores[region] = store
	}

	declaration[propertyArtifactStrs] = stores

	return declaration, nil
}

func codePipelineVersion(res map[string]any) map[string]any {
	pipeline, _ := res[memberPipeline].(map[string]any)

	return map[string]any{memberVersion: scalarText(pipeline[memberVersion])}
}

func init() {
	register(codePipelineType, &resourceType{
		name: memberName,
		properties: append(
			slices.Clone(codePipelineDeclared),
			memberName, memberTags, propertyRestart, "Triggers", "DisableInboundStageTransitions",
		),
		replace:    []string{memberName},
		attributes: []string{memberVersion},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			name := c.nameOf(p, memberName, 100)
			declaration, err := codePipelineDeclaration(name, p)
			if err != nil {
				return "", nil, err
			}

			params := map[string]any{"pipeline": declaration}
			if p.has(memberTags) {
				params[memberTags] = p[memberTags]
			}

			res, err := c.call(ctx, "codepipeline", "CreatePipeline", params)
			if err != nil {
				return "", nil, err
			}

			return name, codePipelineVersion(res), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			declaration, err := codePipelineDeclaration(r.Physical, p)
			if err != nil {
				return nil, err
			}

			attributes := r.Attributes
			if slices.ContainsFunc(codePipelineDeclared, func(k string) bool { return p.changed(r.Properties, k) }) {
				res, callErr := c.call(ctx, "codepipeline", "UpdatePipeline", map[string]any{"pipeline": declaration})
				if callErr != nil {
					return nil, callErr
				}

				attributes = codePipelineVersion(res)
			}

			if tagErr := retag(ctx, c, r.Properties, p, codePipelineTagger(c, r.Physical)); tagErr != nil {
				return nil, tagErr
			}

			return attributes, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, err := c.call(ctx, "codepipeline", "DeletePipeline", map[string]any{memberName: r.Physical})

			return err
		},
	})
}
