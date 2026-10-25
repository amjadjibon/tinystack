package bedrockagentruntime

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	controlapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentruntime"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/eventstream"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type conversation struct {
	Started          time.Time         `json:"started"`
	Updated          time.Time         `json:"updated"`
	Fixture          *Fixture          `json:"fixture,omitempty"`
	Attributes       map[string]string `json:"attributes,omitempty"`
	PromptAttributes map[string]string `json:"promptAttributes,omitempty"`
	Input            string            `json:"input"`
	InvocationID     string            `json:"invocationId,omitempty"`
	Position         int               `json:"position"`
	Busy             bool              `json:"busy"`
	Ended            bool              `json:"ended"`
}

// InvokeAgent executes fixture-selected steps against a prepared alias.
func (s *Service) InvokeAgent(ctx context.Context, in *api.InvokeAgentRequest) (*api.InvokeAgentResponse, error) {
	agent, err := s.agents.ResolveInvocation(ctx, common.Value(in.AgentId), common.Value(in.AgentAliasId))
	if err != nil {
		return nil, err
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	key := scope.Key(
		serviceName,
		"conversation",
		common.Value(in.AgentId)+"/"+common.Value(in.AgentAliasId)+"/"+common.Value(in.SessionId),
	)
	var state *conversation
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, ok, e := storage.Read[conversation](tx, key)
		if e != nil {
			return e
		}

		now := time.Now().UTC()
		if !ok || now.Sub(r.Updated) > time.Duration(common.Value(agent.IdleSessionTTLInSeconds))*time.Second {
			r = &conversation{Started: now, Attributes: map[string]string{}, PromptAttributes: map[string]string{}}
		}

		if r.Busy {
			return conflict("A turn is already executing for this session.")
		}

		if r.Ended {
			return conflict("Agent session has ended.")
		}

		if in.SessionState != nil {
			if r.Attributes == nil {
				r.Attributes = map[string]string{}
			}

			maps.Copy(r.Attributes, in.SessionState.SessionAttributes)
			r.PromptAttributes = common.CloneTags(in.SessionState.PromptSessionAttributes)
		}

		if r.InvocationID != "" {
			if in.SessionState == nil || common.Value(in.SessionState.InvocationId) != r.InvocationID ||
				len(in.SessionState.ReturnControlInvocationResults) != 1 {
				return invalid("Return-control results must match the pending invocationId.")
			}

			result := in.SessionState.ReturnControlInvocationResults[0]
			step := r.Fixture.Steps[r.Position-1]
			if step.Function != "" {
				if result.FunctionResult == nil ||
					common.Value(result.FunctionResult.ActionGroup) != step.ActionGroup ||
					common.Value(result.FunctionResult.Function) != step.Function {
					return invalid("Function result does not match the pending action.")
				}
			} else if result.ApiResult == nil || common.Value(result.ApiResult.ActionGroup) != step.ActionGroup ||
				common.Value(result.ApiResult.ApiPath) != step.APIPath ||
				common.Value(result.ApiResult.HttpMethod) != step.HTTPMethod {
				return invalid("API result does not match the pending action.")
			}

			r.InvocationID = ""
		} else {
			if in.SessionState != nil &&
				(in.SessionState.InvocationId != nil || len(in.SessionState.ReturnControlInvocationResults) > 0) {
				return invalid("No return-control invocation is pending.")
			}

			r.Fixture = nil
			r.Position = 0
			r.Input = common.Value(in.InputText)
			fixtures, e := storage.Records[Fixture](
				tx,
				(request.Scope{AccountID: scope.AccountID}).Prefix("bedrock-agent", "fixture"),
			)
			if e != nil {
				return e
			}

			for _, f := range fixtures {
				if f.matches(common.Value(in.AgentId), common.Value(in.AgentAliasId), r.Input) {
					r.Fixture = f
					break
				}
			}

			if r.Fixture == nil {
				return invalid("No agent fixture matches; register one at /_tinystack/bedrock-agent/fixtures.")
			}
		}

		r.Updated = now
		r.Busy = true
		state = r
		return storage.Write(tx, key, r)
	})
	if err != nil {
		return nil, err
	}

	// Release the claim on errors as well as success. Calls to Lambda occur outside storage transactions.
	defer func() {
		_ = s.store.Update(context.WithoutCancel(ctx), func(tx storage.WriteTx) error {
			state.Busy = false
			return storage.Write(tx, key, state)
		})
	}()

	stream := eventstream.Stream{}

	for state.Position < len(state.Fixture.Steps) {
		step := new(state.Fixture.Steps[state.Position])
		group, e := s.agents.ActionGroup(ctx, common.Value(in.AgentId), common.Value(agent.Version), step.ActionGroup)
		if e != nil {
			return nil, e
		}

		parameters, e := stepParameters(step, group)
		if e != nil {
			return nil, e
		}

		if group.ActionGroupExecutor == nil {
			return nil, invalid("Built-in action groups cannot execute fixture steps.")
		}

		if common.Value(group.ActionGroupExecutor.CustomControl) == "RETURN_CONTROL" {
			state.InvocationID = uuid()
			state.Position++
			input := map[string]any{"actionGroup": step.ActionGroup, "parameters": parameters}

			kind := "apiInvocationInput"
			if step.Function != "" {
				kind = "functionInvocationInput"
				input["function"] = step.Function
			} else {
				input["apiPath"] = step.APIPath
				input["httpMethod"] = step.HTTPMethod
			}

			payload, marshalErr := json.Marshal(
				map[string]any{
					"invocationId":     state.InvocationID,
					"invocationInputs": []any{map[string]any{kind: input}},
				},
			)
			if marshalErr != nil {
				return nil, marshalErr
			}

			stream = append(stream, eventstream.Event("event", "returnControl", payload))
			return &api.InvokeAgentResponse{
				Completion:  new(stream),
				ContentType: new("application/json"),
				SessionId:   in.SessionId,
				MemoryId:    in.MemoryId,
			}, nil
		}

		if s.caller == nil {
			return nil, invalid("Lambda execution is unavailable.")
		}

		event := map[string]any{
			"messageVersion": "1.0",
			"agent": map[string]any{
				"id":      common.Value(in.AgentId),
				"name":    common.Value(agent.AgentName),
				"alias":   common.Value(in.AgentAliasId),
				"version": common.Value(agent.Version),
			},
			"actionGroup":             step.ActionGroup,
			"inputText":               state.Input,
			"sessionId":               common.Value(in.SessionId),
			"parameters":              parameters,
			"sessionAttributes":       state.Attributes,
			"promptSessionAttributes": state.PromptAttributes,
		}

		if step.Function != "" {
			event["function"] = step.Function
		} else {
			event["apiPath"] = step.APIPath
			event["httpMethod"] = step.HTTPMethod
			event["requestBody"] = step.RequestBody
		}

		payload, e := json.Marshal(event)
		if e != nil {
			return nil, e
		}

		out, e := s.caller.Call(
			ctx,
			"lambda",
			"Invoke",
			map[string]any{
				"FunctionName":   common.Value(group.ActionGroupExecutor.Lambda),
				"InvocationType": "RequestResponse",
				"Payload":        string(payload),
			},
		)
		if e != nil {
			return nil, e
		}

		if out["FunctionError"] != nil {
			return nil, invalid("Action group Lambda failed: %v", out["FunctionError"])
		}

		raw, _ := out["Payload"].(string)
		if responseErr := applyLambdaResponse([]byte(raw), step, state); responseErr != nil {
			return nil, responseErr
		}

		state.Position++
	}

	payload, err := json.Marshal(map[string]any{"bytes": []byte(state.Fixture.Text)})
	if err != nil {
		return nil, err
	}

	stream = append(stream, eventstream.Event("event", "chunk", payload))
	if common.Value(in.EndSession) {
		state.Ended = true
		if checkErr1 := s.recordMemory(ctx, scope, in, agent, state); checkErr1 != nil {
			return nil, checkErr1
		}
	}

	return &api.InvokeAgentResponse{
		Completion:  new(stream),
		ContentType: new("application/json"),
		SessionId:   in.SessionId,
		MemoryId:    in.MemoryId,
	}, nil
}

func stepParameters(step *Step, group *controlapi.AgentActionGroup) ([]map[string]string, error) {
	types := map[string]string{}

	if step.Function != "" {
		if group.FunctionSchema == nil || group.FunctionSchema.Functions == nil {
			return nil, invalid("Fixture function requires a function schema.")
		}

		found := false
		for _, f := range *group.FunctionSchema.Functions {
			if common.Value(f.Name) != step.Function {
				continue
			}

			found = true
			for name, p := range f.Parameters {
				types[name] = common.Value(p.Type)
				if common.Value(p.Required) {
					if _, ok := step.Parameters[name]; !ok {
						return nil, invalid("Missing function parameter %s.", name)
					}
				}
			}
		}

		if !found {
			return nil, invalid("Fixture function is absent from the action group schema.")
		}
	} else if group.ApiSchema == nil {
		return nil, invalid("Fixture API step requires an API schema.")
	}

	out := []map[string]string{}

	for _, key := range slices.Sorted(maps.Keys(step.Parameters)) {
		typ := types[key]
		if typ == "" {
			typ = "string"
		}

		out = append(out, map[string]string{"name": key, "type": typ, "value": step.Parameters[key]})
	}

	return out, nil
}

type lambdaResponse struct {
	Attributes       map[string]string `json:"sessionAttributes"`
	PromptAttributes map[string]string `json:"promptSessionAttributes"`
	Response         struct {
		ResponseBody map[string]struct {
			Body *string `json:"body"`
		} `json:"responseBody"`
		FunctionResponse *struct {
			ResponseBody map[string]struct {
				Body *string `json:"body"`
			} `json:"responseBody"`
			ResponseState string `json:"responseState"`
		} `json:"functionResponse"`
		HTTPStatusCode *int   `json:"httpStatusCode"`
		ActionGroup    string `json:"actionGroup"`
		Function       string `json:"function"`
		APIPath        string `json:"apiPath"`
		HTTPMethod     string `json:"httpMethod"`
	} `json:"response"`
	Version                     string          `json:"messageVersion"`
	KnowledgeBasesConfiguration json.RawMessage `json:"knowledgeBasesConfiguration"`
}

func applyLambdaResponse(payload []byte, step *Step, state *conversation) error {
	var out lambdaResponse
	if checkErr2 := json.Unmarshal(payload, &out); checkErr2 != nil {
		return invalid("Malformed action group Lambda response: %v", checkErr2)
	}

	if out.Version != "1.0" || out.Response.ActionGroup != step.ActionGroup {
		return invalid("Lambda response requires messageVersion 1.0 and matching actionGroup.")
	}

	if len(out.KnowledgeBasesConfiguration) > 0 {
		return invalid("Knowledge-base retrieval in Lambda responses is not supported.")
	}

	bodies := out.Response.ResponseBody
	if step.Function != "" {
		if out.Response.Function != step.Function || out.Response.FunctionResponse == nil {
			return invalid("Lambda response must match the requested function.")
		}

		response := out.Response.FunctionResponse
		if response.ResponseState != "" && response.ResponseState != "FAILURE" && response.ResponseState != "REPROMPT" {
			return invalid("Invalid function responseState.")
		}

		bodies = response.ResponseBody
		if len(bodies) != 1 {
			return invalid("Function response requires TEXT responseBody.")
		}

		if _, ok := bodies["TEXT"]; !ok {
			return invalid("Function response requires TEXT responseBody.")
		}
	} else if out.Response.APIPath != step.APIPath || out.Response.HTTPMethod != step.HTTPMethod ||
		out.Response.HTTPStatusCode == nil || *out.Response.HTTPStatusCode < 100 || *out.Response.HTTPStatusCode > 599 {
		return invalid("Lambda response requires matching API metadata and httpStatusCode.")
	}

	if len(bodies) == 0 {
		return invalid("Lambda responseBody is required.")
	}

	for _, body := range bodies {
		if body.Body == nil {
			return invalid("Lambda responseBody requires string bodies.")
		}
	}

	if out.Attributes != nil {
		state.Attributes = out.Attributes
	}

	if out.PromptAttributes != nil {
		state.PromptAttributes = out.PromptAttributes
	}

	return nil
}
