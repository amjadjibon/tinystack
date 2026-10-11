package cloudformation

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
)

const (
	batchDisabled               = "DISABLED"
	batchEnabled                = "ENABLED"
	propertyBatchParameters     = "Parameters"
	propertyBatchPlatform       = "PlatformCapabilities"
	serviceBatch                = "batch"
	propertyBatchCEName         = "ComputeEnvironmentName"
	propertyBatchQueueName      = "JobQueueName"
	propertyBatchDefinitionName = "JobDefinitionName"
	propertyBatchResources      = "ComputeResources"
	propertyBatchOrder          = "ComputeEnvironmentOrder"
	propertyBatchContainer      = "ContainerProperties"
	propertyBatchRole           = "ServiceRole"
	propertyBatchState          = "State"
	propertyBatchPriority       = "Priority"
	memberBatchCE               = "ComputeEnvironment"
	memberBatchQueue            = "JobQueue"
	memberBatchDefinition       = "JobDefinition"
	memberBatchCEArn            = "ComputeEnvironmentArn"
	memberBatchQueueArn         = "JobQueueArn"
	memberBatchDefinitionArn    = "JobDefinitionArn"
	batchCEType                 = "AWS::Batch::ComputeEnvironment"
	batchQueueType              = "AWS::Batch::JobQueue"
	batchDefinitionType         = "AWS::Batch::JobDefinition"
)

func init() {
	register("AWS::Batch::ComputeEnvironment", &resourceType{
		// Batch names cannot coexist. Leave replacement create-first: an explicitly
		// reused name fails without deleting a referenced original environment.
		properties: []string{
			propertyBatchCEName,
			memberType,
			propertyBatchState,
			propertyBatchRole,
			propertyBatchResources,
			memberTags,
		},
		replace:    []string{propertyBatchCEName, memberType, propertyBatchRole, propertyBatchResources},
		attributes: []string{memberBatchCEArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if _, e := backupTags(p, memberTags); e != nil {
				return "", nil, e
			}
			if p.has(propertyBatchState) && p.str(propertyBatchState) != batchEnabled {
				return "", nil, validation("CloudFormation compute environments must be created ENABLED")
			}
			out, e := c.call(
				ctx,
				serviceBatch,
				"CreateComputeEnvironment",
				pick(
					map[string]any{propertyBatchCEName: c.nameOf(p, propertyBatchCEName, 128)},
					p,
					propertyBatchCEName,
					memberType,
					propertyBatchState,
					propertyBatchRole,
					propertyBatchResources,
					memberTags,
				),
			)
			return outText(out, memberBatchCEArn), out, e
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			before, e := batchRead(ctx, c, r.Type, r.Physical)
			if e != nil {
				return nil, e
			}
			params := pick(
				map[string]any{memberBatchCE: r.Physical, propertyBatchState: batchEnabled},
				p,
				propertyBatchState,
			)
			if _, e = c.call(ctx, serviceBatch, "UpdateComputeEnvironment", params); e != nil {
				return nil, e
			}
			if e = batchRetag(ctx, c, r, p); e != nil {
				_, restoreErr := c.call(
					context.WithoutCancel(ctx),
					serviceBatch,
					"UpdateComputeEnvironment",
					map[string]any{memberBatchCE: r.Physical, propertyBatchState: before[propertyBatchState]},
				)
				return nil, errors.Join(e, restoreErr)
			}
			return batchRead(ctx, c, r.Type, r.Physical)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			before, e := batchRead(ctx, c, r.Type, r.Physical)
			if e != nil {
				return e
			}
			if len(before) == 0 {
				return nil
			}
			if _, e = c.call(
				ctx,
				serviceBatch,
				"UpdateComputeEnvironment",
				map[string]any{memberBatchCE: r.Physical, propertyBatchState: batchDisabled},
			); e != nil {
				return e
			}
			_, e = c.call(ctx, serviceBatch, "DeleteComputeEnvironment", map[string]any{memberBatchCE: r.Physical})
			if e != nil {
				_, restoreErr := c.call(
					context.WithoutCancel(ctx),
					serviceBatch,
					"UpdateComputeEnvironment",
					map[string]any{memberBatchCE: r.Physical, propertyBatchState: before[propertyBatchState]},
				)
				return errors.Join(e, restoreErr)
			}
			return nil
		},
	})
	register("AWS::Batch::JobQueue", &resourceType{
		properties: []string{
			propertyBatchQueueName,
			propertyBatchState,
			propertyBatchPriority,
			propertyBatchOrder,
			memberTags,
		},
		replace:    []string{propertyBatchQueueName},
		attributes: []string{memberBatchQueueArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if !p.has(propertyBatchPriority) || !p.has(propertyBatchOrder) {
				return "", nil, validation("JobQueue Priority and ComputeEnvironmentOrder are required")
			}
			if _, e := backupTags(p, memberTags); e != nil {
				return "", nil, e
			}
			out, e := c.call(
				ctx,
				serviceBatch,
				"CreateJobQueue",
				pick(
					map[string]any{propertyBatchQueueName: c.nameOf(p, propertyBatchQueueName, 128)},
					p,
					propertyBatchQueueName,
					propertyBatchState,
					propertyBatchPriority,
					propertyBatchOrder,
					memberTags,
				),
			)
			return outText(out, memberBatchQueueArn), out, e
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if !p.has(propertyBatchPriority) || !p.has(propertyBatchOrder) {
				return nil, validation("JobQueue Priority and ComputeEnvironmentOrder are required")
			}
			before, e := batchRead(ctx, c, r.Type, r.Physical)
			if e != nil {
				return nil, e
			}
			params := pick(
				map[string]any{memberBatchQueue: r.Physical, propertyBatchState: batchEnabled},
				p,
				propertyBatchState,
				propertyBatchPriority,
				propertyBatchOrder,
			)
			if _, e = c.call(ctx, serviceBatch, "UpdateJobQueue", params); e != nil {
				return nil, e
			}
			if e = batchRetag(ctx, c, r, p); e != nil {
				restore := pick(
					map[string]any{memberBatchQueue: r.Physical},
					props(before),
					propertyBatchState,
					propertyBatchPriority,
					propertyBatchOrder,
				)
				_, restoreErr := c.call(context.WithoutCancel(ctx), serviceBatch, "UpdateJobQueue", restore)
				return nil, errors.Join(e, restoreErr)
			}
			return batchRead(ctx, c, r.Type, r.Physical)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			before, e := batchRead(ctx, c, r.Type, r.Physical)
			if e != nil {
				return e
			}
			if len(before) == 0 {
				return nil
			}
			if _, e = c.call(
				ctx,
				serviceBatch,
				"UpdateJobQueue",
				map[string]any{memberBatchQueue: r.Physical, propertyBatchState: batchDisabled},
			); e != nil {
				return e
			}
			_, e = c.call(ctx, serviceBatch, "DeleteJobQueue", map[string]any{memberBatchQueue: r.Physical})
			if e != nil {
				_, restoreErr := c.call(
					context.WithoutCancel(ctx),
					serviceBatch,
					"UpdateJobQueue",
					map[string]any{memberBatchQueue: r.Physical, propertyBatchState: before[propertyBatchState]},
				)
				return errors.Join(e, restoreErr)
			}
			return nil
		},
	})
	register("AWS::Batch::JobDefinition", &resourceType{
		properties: []string{
			propertyBatchDefinitionName,
			memberType,
			propertyBatchParameters,
			propertyBatchContainer,
			propertyBatchPlatform,
			memberTags,
		},
		replace: []string{
			propertyBatchDefinitionName,
			memberType,
			propertyBatchParameters,
			propertyBatchContainer,
			propertyBatchPlatform,
		},
		attributes: []string{memberBatchDefinitionArn},
		// Even an unchanged explicit name can create a new revision beside the old.
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			if _, e := backupTags(p, memberTags); e != nil {
				return "", nil, e
			}
			out, e := c.call(
				ctx,
				serviceBatch,
				"RegisterJobDefinition",
				pick(
					map[string]any{propertyBatchDefinitionName: c.nameOf(p, propertyBatchDefinitionName, 128)},
					p,
					propertyBatchDefinitionName,
					memberType,
					propertyBatchParameters,
					propertyBatchContainer,
					propertyBatchPlatform,
					memberTags,
				),
			)
			return outText(out, memberBatchDefinitionArn), out, e
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			return r.Attributes, batchRetag(ctx, c, r, p)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			before, e := batchRead(ctx, c, r.Type, r.Physical)
			if e != nil {
				return e
			}
			if len(before) == 0 {
				return nil
			}
			_, e = c.call(
				ctx,
				serviceBatch,
				"DeregisterJobDefinition",
				map[string]any{memberBatchDefinition: r.Physical},
			)
			return e
		},
	})
}

func batchRead(ctx context.Context, c *client, typ, id string) (map[string]any, error) {
	operation, list, filter := "DescribeComputeEnvironments", "ComputeEnvironments", "ComputeEnvironments"
	if typ == batchQueueType {
		operation, list, filter = "DescribeJobQueues", "JobQueues", "JobQueues"
	}
	if typ == batchDefinitionType {
		operation, list, filter = "DescribeJobDefinitions", "JobDefinitions", "JobDefinitions"
	}
	out, e := c.call(ctx, serviceBatch, operation, map[string]any{filter: []any{id}})
	if e != nil {
		return nil, e
	}
	items, _ := out[list].([]any)
	if len(items) == 0 {
		return map[string]any{}, nil
	}
	r, _ := items[0].(map[string]any)
	return r, nil
}

// Free removed owned keys before additions. Preserve live values for
// compensation: external edits to a template-owned key must also survive failure.
func batchRetag(ctx context.Context, c *client, r *resource, p props) error {
	desired, e := backupTags(p, memberTags)
	if e != nil {
		return e
	}
	old, e := backupTags(props(r.Properties), memberTags)
	if e != nil {
		return e
	}
	set, removed := tagChanges(old, desired)
	target := map[string]any{memberResourceArn: r.Physical}
	restore := map[string]any{}
	if len(set) > 0 && len(removed) > 0 {
		out, readErr := c.call(ctx, serviceBatch, "ListTagsForResource", target)
		if readErr != nil {
			return readErr
		}
		live, _ := out[memberTags].(map[string]any)
		for _, value := range removed {
			k, _ := value.(string)
			if v, ok := live[k]; ok {
				restore[k] = scalarText(v)
			}
		}
	}
	if len(removed) > 0 {
		params := maps.Clone(target)
		params["TagKeys"] = removed
		if _, e = c.call(ctx, serviceBatch, opUntagResource, params); e != nil {
			return e
		}
	}
	if len(set) > 0 {
		params := maps.Clone(target)
		params[memberTags] = tagMap(set)
		_, e = c.call(ctx, serviceBatch, opTagResource, params)
		if e != nil && len(restore) > 0 {
			params[memberTags] = restore
			_, restoreErr := c.call(context.WithoutCancel(ctx), serviceBatch, opTagResource, params)
			return errors.Join(e, restoreErr)
		}
	}
	return e
}

func isBatchResource(typ string) bool {
	return typ == batchCEType || typ == batchQueueType || typ == batchDefinitionType
}

func batchRestoreReplacement(st *stack, next *step) (id string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || !isBatchResource(next.typ) {
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

// Retired queues retain their original associations; remove them before retiring
// the old environments, even when replacements were appended in creation order.
func batchOrderRetired(st *stack) {
	for i := 0; i < len(st.Replaced); i++ {
		parent := st.Replaced[i].Resource
		if parent.Type != batchCEType {
			continue
		}
		moved := false
		for j := i + 1; j < len(st.Replaced) && !moved; j++ {
			child := st.Replaced[j].Resource
			if child.Type != batchQueueType {
				continue
			}
			for _, v := range props(child.Properties).list(propertyBatchOrder) {
				m, _ := v.(map[string]any)
				ref := scalarText(m[memberBatchCE])
				if ref == parent.Physical || strings.HasSuffix(parent.Physical, "/"+ref) {
					item := st.Replaced[j]
					copy(st.Replaced[i+1:j+1], st.Replaced[i:j])
					st.Replaced[i] = item
					moved = true
					break
				}
			}
		}
		if moved {
			i--
		}
	}
}
