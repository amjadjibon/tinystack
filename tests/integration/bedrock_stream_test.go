package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	runtime "github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	rt "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestBedrockResponseStreams(t *testing.T) {
	t.Parallel()
	s := server(t)
	_, client := bedrockClients(s, "test", "us-east-1")
	ctx := context.Background()
	for _, tc := range []struct{ model, body string }{
		{bedrockClaude, `{"anthropic_version":"bedrock-2023-05-31","max_tokens":20,
"messages":[{"role":"user","content":"hello"}]}`},
		{"amazon.titan-text-express-v1", `{"inputText":"hello"}`},
		{"amazon.nova-micro-v1:0", `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`},
		{"meta.llama3-8b-instruct-v1:0", `{"prompt":"hello"}`},
	} {
		t.Run(tc.model, func(t *testing.T) {
			native, err := client.InvokeModelWithResponseStream(ctx, &runtime.InvokeModelWithResponseStreamInput{
				ModelId: new(tc.model), Body: []byte(tc.body), ContentType: new("application/json"),
			})
			if err != nil {
				t.Fatal(err)
			}

			reader := native.GetStream()
			defer reader.Close()
			text := ""
			metrics := false
			count := 0
			for event := range reader.Events() {
				chunk, ok := event.(*rt.ResponseStreamMemberChunk)
				if !ok {
					t.Fatalf("unexpected native event %T", event)
				}

				count++
				var value map[string]any
				if decodeErr := json.Unmarshal(chunk.Value.Bytes, &value); decodeErr != nil {
					t.Fatal(decodeErr)
				}

				for _, key := range []string{"outputText", "generation"} {
					if delta, ok := value[key].(string); ok {
						text += delta
					}
				}

				if delta, ok := value["delta"].(map[string]any); ok {
					if part, ok := delta["text"].(string); ok {
						text += part
					}
				}

				if delta, ok := value["contentBlockDelta"].(map[string]any); ok {
					text += delta["delta"].(map[string]any)["text"].(string)
				}

				if value["amazon-bedrock-invocationMetrics"] != nil {
					metrics = true
				}
			}

			if reader.Err() != nil || text != "TinyStack: hello" || count < 3 || !metrics {
				t.Fatalf("native text=%q count=%d metrics=%v error=%v", text, count, metrics, reader.Err())
			}

			converse, err := client.ConverseStream(ctx, &runtime.ConverseStreamInput{
				ModelId: new(tc.model), Messages: bedrockMessages("hello"),
			})
			if err != nil {
				t.Fatal(err)
			}

			stream := converse.GetStream()
			defer stream.Close()
			var textParts strings.Builder
			names := []string{}
			for event := range stream.Events() {
				switch value := event.(type) {
				case *rt.ConverseStreamOutputMemberMessageStart:
					names = append(names, "start")
				case *rt.ConverseStreamOutputMemberContentBlockDelta:
					textParts.WriteString(value.Value.Delta.(*rt.ContentBlockDeltaMemberText).Value)
				case *rt.ConverseStreamOutputMemberContentBlockStop:
					names = append(names, "blockStop")
				case *rt.ConverseStreamOutputMemberMessageStop:
					names = append(names, string(value.Value.StopReason))
				case *rt.ConverseStreamOutputMemberMetadata:
					names = append(names, "metadata")
					if value.Value.Usage == nil || *value.Value.Usage.TotalTokens != 3 || value.Value.Metrics == nil {
						t.Fatalf("metadata: %#v", value.Value)
					}
				default:
					t.Fatalf("unexpected converse event %T", event)
				}
			}

			if stream.Err() != nil || textParts.String() != "TinyStack: hello" ||
				strings.Join(names, ",") != "start,blockStop,end_turn,metadata" {
				t.Fatalf("converse: %q %v %v", textParts.String(), names, stream.Err())
			}
		})
	}
}

func TestBedrockStreamFixturesAndValidation(t *testing.T) {
	t.Parallel()
	s := server(t)
	_, client := bedrockClients(s, "test", "us-east-1")
	ctx := context.Background()
	bedrockFixture(t, s, "tool", fmt.Sprintf(`{"modelId":%q,"contains":"weather","text":"",
 "toolUse":{"toolUseId":"call-1","name":"weather","input":{"city":"Paris"}}}`, bedrockClaude))
	in := &runtime.ConverseStreamInput{ModelId: new(bedrockClaude), Messages: bedrockMessages("weather")}
	_, err := client.ConverseStream(ctx, in)
	bedrockError(t, err, "ValidationException")
	in.ToolConfig = &rt.ToolConfiguration{Tools: []rt.Tool{&rt.ToolMemberToolSpec{Value: rt.ToolSpecification{
		Name: new("weather"), InputSchema: &rt.ToolInputSchemaMemberJson{
			Value: document.NewLazyDocument(map[string]any{"type": "object"}),
		},
	}}}}
	out, err := client.ConverseStream(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	stream := out.GetStream()
	defer stream.Close()
	start, delta, stop := false, false, false
	for event := range stream.Events() {
		switch value := event.(type) {
		case *rt.ConverseStreamOutputMemberContentBlockStart:
			tool := value.Value.Start.(*rt.ContentBlockStartMemberToolUse)
			start = *tool.Value.Name == "weather" && *tool.Value.ToolUseId == "call-1"
		case *rt.ConverseStreamOutputMemberContentBlockDelta:
			tool := value.Value.Delta.(*rt.ContentBlockDeltaMemberToolUse)
			delta = *tool.Value.Input == `{"city":"Paris"}`
		case *rt.ConverseStreamOutputMemberMessageStop:
			stop = value.Value.StopReason == rt.StopReasonToolUse
		}
	}

	if stream.Err() != nil || !start || !delta || !stop {
		t.Fatalf("tool stream: %v %v %v %v", start, delta, stop, stream.Err())
	}

	_, err = client.InvokeModelWithResponseStream(ctx, &runtime.InvokeModelWithResponseStreamInput{
		ModelId: new("amazon.titan-embed-text-v2:0"), ContentType: new("application/json"),
		Body: []byte(`{"inputText":"hello"}`),
	})
	bedrockError(t, err, "ValidationException")
	_, err = client.InvokeModelWithResponseStream(ctx, &runtime.InvokeModelWithResponseStreamInput{
		ModelId: new(bedrockClaude), ContentType: new("application/json"), Body: []byte(`{"prompt":"wrong"}`),
	})
	bedrockError(t, err, "ValidationException")
	_, err = client.InvokeModelWithResponseStream(ctx, &runtime.InvokeModelWithResponseStreamInput{
		ModelId: new("amazon.titan-text-express-v1"), ContentType: new("application/json"),
		Body: []byte(`{"inputText":"hello"}`), GuardrailIdentifier: new("unsupported"),
	})
	bedrockError(t, err, "NotImplemented")
	_, err = client.ConverseStream(ctx, &runtime.ConverseStreamInput{
		ModelId:  new(bedrockClaude),
		Messages: bedrockMessages("hello"), InferenceConfig: &rt.InferenceConfiguration{Temperature: new(float32(0.5))},
	})
	bedrockError(t, err, "NotImplemented")
}
