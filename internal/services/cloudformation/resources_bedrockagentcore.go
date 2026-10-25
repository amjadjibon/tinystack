package cloudformation

import (
	"context"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	agentCoreService  = "bedrockagentcorecontrol"
	agentCoreNotFound = "ResourceNotFoundException"
	agentCoreRuntime  = "AWS::BedrockAgentCore::Runtime"
	agentCoreEndpoint = "AWS::BedrockAgentCore::RuntimeEndpoint"
	agentCoreMemory   = "AWS::BedrockAgentCore::Memory"

	agentCoreRuntimeID   = "AgentRuntimeId"
	agentCoreRuntimeARN  = "AgentRuntimeArn"
	agentCoreEndpointID  = "AgentRuntimeEndpointArn"
	agentCoreMemoryARN   = "MemoryArn"
	agentCoreMemoryID    = "MemoryId"
	agentCoreProtocol    = "ProtocolConfiguration"
	agentCoreEndpoints   = "EndpointName"
	agentCoreEnvironment = "EnvironmentVariables"
	agentCoreEncryption  = "EncryptionKeyArn"
	agentCoreVersion     = "AgentRuntimeVersion"
	agentCoreFailure     = "FailureReason"
	agentCoreCreated     = "CreatedAt"
	agentCoreUpdated     = "LastUpdatedAt"
)

// agentCoreSettle polls op until done reports the resource settled, as CloudFormation waits for AgentCore.
func agentCoreSettle(
	ctx context.Context,
	c *client,
	op string,
	params map[string]any,
	done func(out map[string]any, err error) (bool, error),
) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, err := c.call(ctx, agentCoreService, op, params)
		if finished, doneErr := done(out, err); finished || doneErr != nil {
			return out, doneErr
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// agentCoreReady waits for the resource's status to leave the transitional ones.
func agentCoreReady(ctx context.Context, c *client, op string, params map[string]any) (map[string]any, error) {
	return agentCoreSettle(ctx, c, op, params, func(out map[string]any, err error) (bool, error) {
		switch outText(out, memberStatus) {
		case "CREATING", "UPDATING":
			return false, err
		}

		return err == nil, err
	})
}

// agentCoreDelete deletes a resource and waits until it has gone, so the resources it depends on can follow.
func agentCoreDelete(ctx context.Context, c *client, del, get string, params map[string]any) error {
	if _, err := c.call(ctx, agentCoreService, del, params); err != nil {
		if gone(err, agentCoreNotFound) {
			return nil
		}

		return err
	}

	_, err := agentCoreSettle(ctx, c, get, params, func(_ map[string]any, err error) (bool, error) {
		if gone(err, agentCoreNotFound) {
			return true, nil
		}

		return false, err
	})

	return err
}

func agentCoreTagger(arn any) *tagger {
	return &tagger{
		service: agentCoreService, tag: opTagResource, untag: opUntagResource,
		target: map[string]any{memberResourceArn: arn},
	}
}

// agentCoreTime renders an API timestamp, seconds since the epoch or already text, as CloudFormation does.
func agentCoreTime(v any) any {
	if stamp, ok := v.(float64); ok {
		return time.UnixMilli(int64(stamp * 1000)).UTC().Format(time.RFC3339Nano)
	}

	return v
}

// agentRuntimeParams maps the template's properties to the members Create and UpdateAgentRuntime take. The
// template names the protocol with a string, the API with a structure.
func agentRuntimeParams(p props) map[string]any {
	params := pick(map[string]any{}, p, "AgentRuntimeArtifact", "AuthorizerConfiguration", memberDescription,
		agentCoreEnvironment, "LifecycleConfiguration", "NetworkConfiguration", "RequestHeaderConfiguration",
		memberRoleArn)
	if p.has(agentCoreProtocol) {
		params[agentCoreProtocol] = map[string]any{"ServerProtocol": p.str(agentCoreProtocol)}
	}

	return params
}

func agentRuntimeAttributes(ctx context.Context, c *client, id string) (map[string]any, error) {
	out, err := agentCoreReady(ctx, c, "GetAgentRuntime", map[string]any{agentCoreRuntimeID: id})
	if err != nil {
		return nil, err
	}

	return map[string]any{
		agentCoreRuntimeARN: out[agentCoreRuntimeARN], agentCoreRuntimeID: out[agentCoreRuntimeID],
		agentCoreVersion: out[agentCoreVersion], agentCoreCreated: agentCoreTime(out[agentCoreCreated]),
		agentCoreFailure: outText(out, agentCoreFailure), agentCoreUpdated: agentCoreTime(out[agentCoreUpdated]),
		memberStatus: out[memberStatus],
	}, nil
}

func agentEndpointAttributes(ctx context.Context, c *client, runtimeID, name string) (map[string]any, error) {
	out, err := agentCoreReady(ctx, c, "GetAgentRuntimeEndpoint",
		map[string]any{agentCoreRuntimeID: runtimeID, agentCoreEndpoints: name})
	if err != nil {
		return nil, err
	}

	return map[string]any{
		agentCoreRuntimeARN: out[agentCoreRuntimeARN], agentCoreEndpointID: out[agentCoreEndpointID],
		agentCoreCreated: agentCoreTime(out[agentCoreCreated]), agentCoreFailure: outText(out, agentCoreFailure),
		"Id": out["Id"], agentCoreUpdated: agentCoreTime(out[agentCoreUpdated]), "LiveVersion": out["LiveVersion"],
		memberStatus: out[memberStatus], "TargetVersion": out["TargetVersion"],
	}, nil
}

func agentMemoryAttributes(ctx context.Context, c *client, id string) (map[string]any, error) {
	out, err := agentCoreReady(ctx, c, "GetMemory", map[string]any{agentCoreMemoryID: id})
	if err != nil {
		return nil, err
	}

	memory, _ := out["Memory"].(map[string]any)

	return map[string]any{
		agentCoreMemoryARN: memory["Arn"],
		agentCoreMemoryID:  memory["Id"],
		agentCoreCreated:   agentCoreTime(memory[agentCoreCreated]),
		agentCoreFailure:   outText(memory, agentCoreFailure),
		"UpdatedAt":        agentCoreTime(memory["UpdatedAt"]),
		memberStatus:       memory[memberStatus],
	}, nil
}

// agentRuntimeIdentity returns the ID of the runtime an endpoint belongs to from its AgentRuntimeId property: the
// ID, or the ARN a template passes with Ref.
func agentRuntimeIdentity(ref string) string { return ref[strings.LastIndex(ref, "/")+1:] }

func init() {
	register(agentCoreRuntime, &resourceType{
		properties: []string{
			"AgentRuntimeArtifact",
			"AgentRuntimeName",
			"AuthorizerConfiguration",
			memberDescription,
			agentCoreEnvironment,
			"LifecycleConfiguration",
			"NetworkConfiguration",
			agentCoreProtocol,
			"RequestHeaderConfiguration",
			memberRoleArn,
			memberTags,
		},
		attributes: []string{
			agentCoreRuntimeARN,
			agentCoreRuntimeID,
			agentCoreVersion,
			agentCoreCreated,
			agentCoreFailure,
			agentCoreUpdated,
			memberStatus,
		},
		replace: []string{"AgentRuntimeName"},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := agentRuntimeParams(p)
			params["AgentRuntimeName"] = p.str("AgentRuntimeName")
			params[memberTags] = tagMap(p.tags())
			params["ClientToken"] = common.RandomID(48)
			out, err := c.call(ctx, agentCoreService, "CreateAgentRuntime", params)
			if err != nil {
				return "", nil, err
			}

			attrs, err := agentRuntimeAttributes(ctx, c, outText(out, agentCoreRuntimeID))

			return outText(out, agentCoreRuntimeARN), attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			id := outText(r.Attributes, agentCoreRuntimeID)
			params := agentRuntimeParams(p)
			params[agentCoreRuntimeID] = id
			params["ClientToken"] = common.RandomID(48)
			if _, err := c.call(ctx, agentCoreService, "UpdateAgentRuntime", params); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, agentCoreTagger(r.Physical)); err != nil {
				return nil, err
			}

			return agentRuntimeAttributes(ctx, c, id)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return agentCoreDelete(ctx, c, "DeleteAgentRuntime", "GetAgentRuntime",
				map[string]any{agentCoreRuntimeID: outText(r.Attributes, agentCoreRuntimeID)})
		},
	})
	register(agentCoreEndpoint, &resourceType{
		name:       memberName,
		properties: []string{"AgentRuntimeId", agentCoreVersion, memberDescription, memberName, memberTags},
		attributes: []string{
			agentCoreRuntimeARN,
			agentCoreEndpointID,
			agentCoreCreated,
			agentCoreFailure,
			"Id",
			agentCoreUpdated,
			"LiveVersion",
			memberStatus,
			"TargetVersion",
		},
		replace: []string{"AgentRuntimeId", memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			runtimeID := agentRuntimeIdentity(p.str("AgentRuntimeId"))
			params := pick(map[string]any{agentCoreRuntimeID: runtimeID}, p, agentCoreVersion, memberDescription)
			params[memberName] = p.str(memberName)
			params[memberTags] = tagMap(p.tags())
			params["ClientToken"] = common.RandomID(48)
			out, err := c.call(ctx, agentCoreService, "CreateAgentRuntimeEndpoint", params)
			if err != nil {
				return "", nil, err
			}

			attrs, err := agentEndpointAttributes(ctx, c, runtimeID, p.str(memberName))

			return outText(out, agentCoreEndpointID), attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			runtimeID := agentRuntimeIdentity(p.str("AgentRuntimeId"))
			params := pick(map[string]any{agentCoreRuntimeID: runtimeID, agentCoreEndpoints: p.str(memberName)}, p,
				agentCoreVersion, memberDescription)
			params["ClientToken"] = common.RandomID(48)
			if _, err := c.call(ctx, agentCoreService, "UpdateAgentRuntimeEndpoint", params); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, agentCoreTagger(r.Physical)); err != nil {
				return nil, err
			}

			return agentEndpointAttributes(ctx, c, runtimeID, p.str(memberName))
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			declared := props(r.Properties)

			return agentCoreDelete(ctx, c, "DeleteAgentRuntimeEndpoint", "GetAgentRuntimeEndpoint", map[string]any{
				agentCoreRuntimeID: agentRuntimeIdentity(declared.str("AgentRuntimeId")),
				agentCoreEndpoints: declared.str(memberName),
			})
		},
	})
	register(agentCoreMemory, &resourceType{
		name: memberName,
		properties: []string{
			memberDescription,
			agentCoreEncryption,
			"EventExpiryDuration",
			"MemoryExecutionRoleArn",
			memberName,
			memberTags,
		},
		attributes: []string{
			agentCoreMemoryARN,
			agentCoreMemoryID,
			agentCoreCreated,
			agentCoreFailure,
			memberStatus,
			"UpdatedAt",
		},
		replace: []string{agentCoreEncryption, memberName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			params := pick(map[string]any{}, p, memberDescription, agentCoreEncryption, "EventExpiryDuration",
				"MemoryExecutionRoleArn", memberName)
			params[memberTags] = tagMap(p.tags())
			params["ClientToken"] = common.RandomID(48)
			out, err := c.call(ctx, agentCoreService, "CreateMemory", params)
			if err != nil {
				return "", nil, err
			}

			memory, _ := out["Memory"].(map[string]any)
			attrs, err := agentMemoryAttributes(ctx, c, outText(memory, "Id"))

			return outText(memory, "Arn"), attrs, err
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			id := outText(r.Attributes, agentCoreMemoryID)
			params := pick(map[string]any{agentCoreMemoryID: id}, p, memberDescription, "EventExpiryDuration",
				"MemoryExecutionRoleArn")
			params["ClientToken"] = common.RandomID(48)
			if _, err := c.call(ctx, agentCoreService, "UpdateMemory", params); err != nil {
				return nil, err
			}

			if err := retag(ctx, c, r.Properties, p, agentCoreTagger(r.Physical)); err != nil {
				return nil, err
			}

			return agentMemoryAttributes(ctx, c, id)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			return agentCoreDelete(ctx, c, "DeleteMemory", "GetMemory",
				map[string]any{agentCoreMemoryID: outText(r.Attributes, agentCoreMemoryID)})
		},
	})
}
