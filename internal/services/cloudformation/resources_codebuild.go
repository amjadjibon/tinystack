package cloudformation

import (
	"context"
	"maps"
	"reflect"
)

const (
	codebuildProjectType       = "AWS::CodeBuild::Project"
	serviceCodeBuild           = "codebuild"
	propertyBuildSource        = "Source"
	propertyBuildArtifacts     = "Artifacts"
	propertyBuildRole          = "ServiceRole"
	propertyBuildVersion       = "SourceVersion"
	propertyBuildTimeout       = "TimeoutInMinutes"
	propertyBuildQueuedTimeout = "QueuedTimeoutInMinutes"
)

func init() {
	register("AWS::CodeBuild::Project", &resourceType{
		properties: []string{
			memberName, memberDescription, propertyBuildSource, propertyBuildArtifacts, propertyEnvironment,
			propertyBuildRole, propertyBuildVersion, propertyBuildTimeout, propertyBuildQueuedTimeout, memberTags,
		},
		replace: []string{memberName}, attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params, e := codebuildParams(p)
			if e != nil {
				return "", nil, e
			}
			params[memberName] = c.nameOf(p, memberName, 150)
			out, e := c.call(ctx, serviceCodeBuild, "CreateProject", params)
			if e != nil {
				return "", nil, e
			}
			project, _ := out["Project"].(map[string]any)
			return outText(project, memberName), project, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			params, e := codebuildParams(p)
			if e != nil {
				return nil, e
			}
			before, e := codebuildRead(ctx, c, r.Physical)
			if e != nil {
				return nil, e
			}
			if len(before) == 0 {
				return nil, validation("CodeBuild project is missing")
			}
			oldTags, e := codebuildCFTags(props(r.Properties))
			if e != nil {
				return nil, e
			}
			desired, e := codebuildCFTags(p)
			if e != nil {
				return nil, e
			}
			// Replace the complete set in the same atomic project write as configuration.
			// Removing owned keys frees capacity before the new set is validated, while
			// live external keys remain owned by their caller.
			live := codebuildLiveTags(before)
			for k := range oldTags {
				if _, ok := desired[k]; !ok {
					delete(live, k)
				}
			}
			maps.Copy(live, desired)
			params[memberTags] = tagList(live, memberKey, memberValue)
			params[memberName] = r.Physical
			out, e := c.call(ctx, serviceCodeBuild, "UpdateProject", params)
			if e != nil {
				return nil, e
			}
			project, _ := out["Project"].(map[string]any)
			project["CodeBuildPreviousProject"] = before
			project["CodeBuildPreviousProperties"] = r.Properties
			return project, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.call(ctx, serviceCodeBuild, "DeleteProject", map[string]any{memberName: r.Physical})
			return e
		},
	})
}

func codebuildCFTags(p props) (map[string]string, error) {
	out := map[string]string{}
	if !p.has(memberTags) {
		return out, nil
	}
	items, ok := p[memberTags].([]any)
	if !ok {
		return nil, validation("CodeBuild Tags requires a list of Key/Value objects")
	}
	for _, v := range items {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, validation("Invalid CodeBuild Tag")
		}
		for k := range m {
			if k != memberKey && k != memberValue {
				return nil, validation("Unsupported CodeBuild Tag property %s", k)
			}
		}
		k, kok := m[memberKey].(string)
		val, vok := m[memberValue].(string)
		if !kok || !vok {
			return nil, validation("CodeBuild tag Key and Value must be strings")
		}
		if _, dup := out[k]; dup {
			return nil, validation("Duplicate CodeBuild tag key")
		}
		out[k] = val
	}
	return out, nil
}

func codebuildParams(p props) (map[string]any, error) {
	role, ok := p[propertyBuildRole].(string)
	if !ok || role == "" {
		return nil, validation("CodeBuild ServiceRole is required and must be a nonempty string")
	}
	tags, e := codebuildCFTags(p)
	if e != nil {
		return nil, e
	}
	params := pick(map[string]any{
		memberDescription: "", propertyBuildVersion: "", propertyBuildTimeout: float64(60),
		propertyBuildQueuedTimeout: float64(60), memberTags: tagList(tags, memberKey, memberValue),
	}, p,
		memberDescription, propertyBuildVersion, propertyBuildTimeout, propertyBuildQueuedTimeout, propertyBuildRole)
	for _, prop := range []string{propertyBuildSource, propertyBuildArtifacts, propertyEnvironment} {
		m, ok := p[prop].(map[string]any)
		if !ok {
			return nil, validation("CodeBuild %s is required and must be an object", prop)
		}
		converted := maps.Clone(m)
		// CloudFormation's BuildSpec spelling is distinct from the SDK's Buildspec.
		if prop == propertyBuildSource {
			if _, bad := m["Buildspec"]; bad {
				return nil, validation("Use CloudFormation BuildSpec")
			}
			if v, ok := converted["BuildSpec"]; ok {
				converted["Buildspec"] = v
				delete(converted, "BuildSpec")
			}
		}
		params[prop] = converted
	}
	return params, nil
}

func codebuildLiveTags(project map[string]any) map[string]string {
	out := map[string]string{}
	items, _ := project[memberTags].([]any)
	for _, v := range items {
		m, _ := v.(map[string]any)
		out[scalarText(m[memberKey])] = scalarText(m[memberValue])
	}
	return out
}

func codebuildRead(ctx context.Context, c *client, name string) (map[string]any, error) {
	out, e := c.call(ctx, serviceCodeBuild, "BatchGetProjects", map[string]any{"Names": []any{name}})
	if e != nil {
		return nil, e
	}
	items, _ := out["Projects"].([]any)
	if len(items) == 0 {
		return map[string]any{}, nil
	}
	project, _ := items[0].(map[string]any)
	return project, nil
}

func codebuildRestoreReplacement(st *stack, next *step) (physical string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || next.typ != codebuildProjectType {
		return "", nil, false
	}
	for _, old := range st.Replaced {
		if old.Logical == next.logical && old.Resource.Type == next.typ &&
			reflect.DeepEqual(props(old.Resource.Properties), next.props) {
			return old.Resource.Physical, old.Resource.Attributes, true
		}
	}
	return "", nil, false
}

// Restore live configuration captured before this update when a later resource
// fails. A compensating UpdateProject is itself a successful modification, so
// LastModified advances while the original ARN and Created remain stable.
func codebuildRestoreUpdate(
	ctx context.Context,
	c *client,
	r *resource,
	p props,
) (attributes map[string]any, found bool, err error) {
	before, ok := r.Attributes["CodeBuildPreviousProject"].(map[string]any)
	previous, has := r.Attributes["CodeBuildPreviousProperties"].(map[string]any)
	if !ok || !has || !reflect.DeepEqual(props(previous), p) {
		return nil, false, nil
	}
	params := pick(
		map[string]any{memberName: r.Physical, memberDescription: "", propertyBuildVersion: ""},
		props(before),
		memberDescription,
		propertyBuildSource,
		propertyBuildArtifacts,
		propertyEnvironment,
		propertyBuildRole,
		propertyBuildVersion,
		propertyBuildTimeout,
		propertyBuildQueuedTimeout,
		memberTags,
	)
	out, e := c.call(ctx, serviceCodeBuild, "UpdateProject", params)
	if e != nil {
		return nil, true, e
	}
	project, _ := out["Project"].(map[string]any)
	return project, true, nil
}
