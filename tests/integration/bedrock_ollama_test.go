package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	runtime "github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	rt "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestBedrockOllama(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	missing := false
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost ||
			(r.URL.Path != "/api/chat" && r.URL.Path != "/api/embed" && r.URL.Path != "/api/pull") {
			http.NotFound(w, r)
			return
		}

		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}

		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/pull" {
			missing = false
			fmt.Fprint(w, `{"status":"success"}`)
			return
		}

		if missing {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"model missing"}`)
			return
		}

		if r.URL.Path == "/api/embed" {
			fmt.Fprint(w, `{"embeddings":[[0.1,0.2]],"prompt_eval_count":7}`)
			return
		}

		if body["stream"] == true {
			fmt.Fprintln(w, `{"message":{"content":"real "}}`)
			fmt.Fprintln(
				w,
				`{"message":{"content":"completion"},"done":true,"done_reason":"length","prompt_eval_count":7,"eval_count":2}`,
			)
			return
		}

		fmt.Fprint(
			w,
			`{"message":{"content":"real completion"},"done":true,"done_reason":"length","prompt_eval_count":7,"eval_count":2}`,
		)
	}))
	defer fake.Close()
	cfg := testConfig()
	cfg.Bedrock.Inference = "ollama"
	cfg.Bedrock.Ollama.URL = fake.URL
	cfg.Bedrock.Ollama.Models = map[string]string{
		"cohere.":           "chosen",
		"anthropic.":        "wrong",
		"anthropic.claude-": "chosen",
		"amazon.":           "chosen",
		"meta.":             "chosen",
	}

	s := serverConfig(t, cfg, &fakeRuntime{})
	_, client := bedrockClients(s, "test", "us-east-1")
	ctx := context.Background()
	in := &runtime.ConverseInput{
		ModelId: new(bedrockClaude),
		Messages: []rt.Message{
			{Role: rt.ConversationRoleUser, Content: []rt.ContentBlock{&rt.ContentBlockMemberText{Value: "hello"}}},
		},
		InferenceConfig: &rt.InferenceConfiguration{
			MaxTokens:     new(int32(20)),
			Temperature:   new(float32(0.5)),
			TopP:          new(float32(0.9)),
			StopSequences: []string{"STOP"},
		},
		System: []rt.SystemContentBlock{&rt.SystemContentBlockMemberText{Value: "be brief"}},
	}

	out, err := client.Converse(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	message := out.Output.(*rt.ConverseOutputMemberMessage).Value
	if message.Content[0].(*rt.ContentBlockMemberText).Value != "real completion" ||
		out.Usage == nil || out.Usage.InputTokens == nil || *out.Usage.InputTokens != 7 ||
		out.Usage.OutputTokens == nil ||
		*out.Usage.OutputTokens != 2 ||
		out.StopReason != rt.StopReasonMaxTokens {
		t.Fatalf("response: %+v", out)
	}

	mu.Lock()
	first := requests[0]
	mu.Unlock()
	if first["model"] != "chosen" || first["options"].(map[string]any)["num_predict"] != float64(20) ||
		len(first["messages"].([]any)) != 2 {
		t.Fatalf("mapping/options: %v", first)
	}

	stream, err := client.ConverseStream(ctx, &runtime.ConverseStreamInput{ModelId: in.ModelId, Messages: in.Messages})
	if err != nil {
		t.Fatal(err)
	}

	var text string
	for event := range stream.GetStream().Events() {
		if delta, ok := event.(*rt.ConverseStreamOutputMemberContentBlockDelta); ok {
			text += delta.Value.Delta.(*rt.ContentBlockDeltaMemberText).Value
		}
	}

	if streamErr := stream.GetStream().Err(); streamErr != nil {
		t.Fatal(streamErr)
	}

	if text != "real completion" {
		t.Fatalf("stream: %q", text)
	}

	for _, tc := range []struct{ id, body string }{
		{
			bedrockClaude,
			`{"anthropic_version":"bedrock-2023-05-31","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`,
		},
		{"amazon.titan-text-express-v1", `{"inputText":"hello","textGenerationConfig":{"maxTokenCount":20}}`},
		{"meta.llama3-8b-instruct-v1:0", `{"prompt":"hello","max_gen_len":20}`},
		{"amazon.nova-lite-v1:0", `{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			result, invokeErr := client.InvokeModel(
				ctx,
				&runtime.InvokeModelInput{
					ModelId:     new(tc.id),
					ContentType: new("application/json"),
					Body:        []byte(tc.body),
				},
			)
			if invokeErr != nil {
				t.Fatal(invokeErr)
			}

			if !json.Valid(result.Body) || !bytes.Contains(result.Body, []byte("real completion")) {
				t.Fatalf("native response: %s", result.Body)
			}

			resultStream, nativeStreamErr := client.InvokeModelWithResponseStream(
				ctx,
				&runtime.InvokeModelWithResponseStreamInput{
					ModelId:     new(tc.id),
					ContentType: new("application/json"),
					Body:        []byte(tc.body),
				},
			)
			if nativeStreamErr != nil {
				t.Fatal(nativeStreamErr)
			}

			count := 0
			for range resultStream.GetStream().Events() {
				count++
			}

			if eventErr := resultStream.GetStream().Err(); eventErr != nil {
				t.Fatal(eventErr)
			}

			if count == 0 {
				t.Fatal("empty native stream")
			}
		})
	}

	embed, err := client.InvokeModel(
		ctx,
		&runtime.InvokeModelInput{
			ModelId:     new("amazon.titan-embed-text-v2:0"),
			ContentType: new("application/json"),
			Body:        []byte(`{"inputText":"hello"}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	cohere, err := client.InvokeModel(
		ctx,
		&runtime.InvokeModelInput{
			ModelId:     new("cohere.embed-english-v3"),
			ContentType: new("application/json"),
			Body:        []byte(`{"texts":["hello"],"input_type":"search_query"}`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	var cohereBody struct {
		Embeddings [][]float64 `json:"embeddings"`
	}

	if decodeErr := json.Unmarshal(cohere.Body, &cohereBody); decodeErr != nil {
		t.Fatal(decodeErr)
	}

	if len(cohereBody.Embeddings) != 1 || len(cohereBody.Embeddings[0]) != 2 {
		t.Fatalf("cohere: %s", cohere.Body)
	}

	var vector struct {
		Embedding []float64 `json:"embedding"`
		Tokens    int       `json:"inputTextTokenCount"`
	}

	if decodeErr := json.Unmarshal(embed.Body, &vector); decodeErr != nil {
		t.Fatal(decodeErr)
	}

	if len(vector.Embedding) != 2 || vector.Tokens != 7 {
		t.Fatalf("embedding: %+v", vector)
	}

	mu.Lock()
	missing = true
	before := len(requests)
	mu.Unlock()
	if _, pullErr := client.Converse(ctx, in); pullErr != nil {
		t.Fatal(pullErr)
	}

	mu.Lock()
	after := len(requests)
	mu.Unlock()
	if after-before != 3 {
		t.Fatalf("pull requests: %d", after-before)
	}

	bedrockFixture(t, s, "ollama", fmt.Sprintf(`{"modelId":%q,"contains":"hello","text":"fixture"}`, bedrockClaude))
	fixture, err := client.Converse(ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	if fixture.Output.(*rt.ConverseOutputMemberMessage).Value.Content[0].(*rt.ContentBlockMemberText).Value != "fixture" {
		t.Fatal("fixture precedence")
	}

	mu.Lock()
	afterFixture := len(requests)
	mu.Unlock()
	if afterFixture != after {
		t.Fatal("fixture contacted Ollama")
	}
}

func TestBedrockOllamaErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   string
		status int
	}{
		{"missing", "ResourceNotFoundException", 404}, {"model", "ModelErrorException", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
					http.NotFound(w, r)
					return
				}

				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"error":"broken model"}`)
			}))
			defer fake.Close()
			cfg := testConfig()
			cfg.Bedrock.Inference = "ollama"
			cfg.Bedrock.Ollama.URL = fake.URL
			cfg.Bedrock.Ollama.Pull = false
			s := serverConfig(t, cfg, &fakeRuntime{})
			_, c := bedrockClients(s, "test", "us-east-1")
			in := &runtime.ConverseInput{
				ModelId: new(bedrockClaude),
				Messages: []rt.Message{
					{
						Role:    rt.ConversationRoleUser,
						Content: []rt.ContentBlock{&rt.ContentBlockMemberText{Value: "hello"}},
					},
				},
			}

			_, err := c.Converse(context.Background(), in)
			bedrockError(t, err, tc.code)
			in.ModelId = new("cohere.embed-english-v3")
			_, err = c.Converse(context.Background(), in)
			bedrockError(t, err, "ValidationException")
			in.ModelId = new("amazon.nova-lite-v1:0")
			_, err = c.Converse(context.Background(), in)
			bedrockError(t, err, "ResourceNotFoundException")
			in.ModelId = new(bedrockClaude)
			in.ToolConfig = &rt.ToolConfiguration{Tools: []rt.Tool{}}
			_, err = c.Converse(context.Background(), in)
			bedrockError(t, err, "ValidationException")
			in.ToolConfig = nil
			in.Messages[0].Content = []rt.ContentBlock{
				&rt.ContentBlockMemberToolUse{
					Value: rt.ToolUseBlock{
						Input:     document.NewLazyDocument(map[string]any{}),
						Name:      new("unsupported"),
						ToolUseId: new("call"),
					},
				},
			}

			_, err = c.Converse(context.Background(), in)
			bedrockError(t, err, "ValidationException")
			in.Messages[0].Content = []rt.ContentBlock{&rt.ContentBlockMemberText{Value: "hello"}}
			fake.Close()
			_, err = c.Converse(context.Background(), in)
			bedrockError(t, err, "ServiceUnavailableException")
		})
	}
}
