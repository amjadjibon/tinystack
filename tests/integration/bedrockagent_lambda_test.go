package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	sdk "github.com/aws/aws-sdk-go-v2/service/bedrockagent"
	agenttypes "github.com/aws/aws-sdk-go-v2/service/bedrockagent/types"
	runtime "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime"
	rt "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime/types"

	"github.com/amjadjibon/tinystack/internal/services/lambda"
)

type agentLambdaRuntime struct{ events chan map[string]any }

func (r *agentLambdaRuntime) Invoke(_ context.Context, _ *lambda.Function, payload []byte) (lambda.Result, error) {
	var event map[string]any
	if checkErr1 := json.Unmarshal(payload, &event); checkErr1 != nil {
		return lambda.Result{}, checkErr1
	}

	r.events <- event
	body, err := json.Marshal(
		map[string]any{
			"messageVersion": "1.0",
			"response": map[string]any{
				"actionGroup": event["actionGroup"],
				"function":    event["function"],
				"functionResponse": map[string]any{
					"responseBody": map[string]any{"TEXT": map[string]any{"body": "local result"}},
				},
			},
			"sessionAttributes": map[string]string{"lambda": "kept"},
		},
	)
	return lambda.Result{Payload: body}, err
}

func (*agentLambdaRuntime) Release(string) {}

func TestBedrockAgentLambdaExecution(t *testing.T) {
	t.Parallel()
	local := &agentLambdaRuntime{events: make(chan map[string]any, 2)}

	s := serverWith(t, local)
	c, r, id := preparedAgent(t, s)
	function := createFunction(t, lambdaClient(s, "test"), "agent-action", "app.action")
	_, err := c.CreateAgentActionGroup(
		context.Background(),
		&sdk.CreateAgentActionGroupInput{
			AgentId:             id,
			AgentVersion:        new("DRAFT"),
			ActionGroupName:     new("local"),
			ActionGroupExecutor: &agenttypes.ActionGroupExecutorMemberLambda{Value: *function.FunctionArn},
			FunctionSchema: &agenttypes.FunctionSchemaMemberFunctions{
				Value: []agenttypes.Function{{Name: new("lookup")}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	agentFixture(
		t,
		s,
		"lambda",
		map[string]any{
			"agentId":  *id,
			"contains": "local",
			"text":     "fixture final answer",
			"steps":    []any{map[string]any{"actionGroup": "local", "function": "lookup"}},
		},
	)
	for range 2 {
		out, err := r.InvokeAgent(
			context.Background(),
			&runtime.InvokeAgentInput{
				AgentId:      id,
				AgentAliasId: new("TSTALIASID"),
				SessionId:    new("local-lambda-conversation"),
				InputText:    new("call local"),
				EnableTrace:  new(true),
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		stream := out.GetStream()
		text := ""
		for event := range stream.Events() {
			if chunk, ok := event.(*rt.ResponseStreamMemberChunk); ok {
				text += string(chunk.Value.Bytes)
			} else {
				t.Errorf("unexpected event %T", event)
			}
		}

		if checkErr2 := stream.Err(); checkErr2 != nil || text != "fixture final answer" {
			t.Fatalf("answer %q: %v", text, checkErr2)
		}

		stream.Close()
	}

	first, second := <-local.events, <-local.events
	if first["messageVersion"] != "1.0" || first["function"] != "lookup" ||
		first["sessionId"] != "local-lambda-conversation" {
		t.Fatalf("Lambda event: %+v", first)
	}

	attributes, _ := second["sessionAttributes"].(map[string]any)
	if attributes["lambda"] != "kept" {
		t.Fatalf("session attributes: %+v", second)
	}
}

func (*agentLambdaRuntime) Close() {}
