package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/bedrock"
	bt "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	runtime "github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	rt "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
)

const bedrockClaude = "anthropic.claude-sonnet-4-5-20250929-v1:0"

func bedrockClients(s *httptest.Server, key, region string) (control *sdk.Client, inference *runtime.Client) {
	return sdk.New(
			sdk.Options{Region: region, BaseEndpoint: new(s.URL), Credentials: creds(key), Retryer: aws.NopRetryer{}},
		),
		runtime.New(
			runtime.Options{
				Region:       region,
				BaseEndpoint: new(s.URL),
				Credentials:  creds(key),
				Retryer:      aws.NopRetryer{},
			},
		)
}

func bedrockError(t *testing.T, err error, code string) {
	t.Helper()
	var apiError smithy.APIError
	if !errors.As(err, &apiError) || apiError.ErrorCode() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestBedrockCatalogueProfiles(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	c, r := bedrockClients(s, "test", "us-east-1")
	list, err := c.ListFoundationModels(ctx, &sdk.ListFoundationModelsInput{})
	if err != nil || len(list.ModelSummaries) < 15 {
		t.Fatalf("catalogue: %v %v", list, err)
	}

	filtered, err := c.ListFoundationModels(
		ctx,
		&sdk.ListFoundationModelsInput{
			ByProvider:       new("Amazon"),
			ByOutputModality: bt.ModelModalityEmbedding,
			ByInferenceType:  bt.InferenceTypeOnDemand,
		},
	)
	if err != nil || len(filtered.ModelSummaries) != 1 ||
		*filtered.ModelSummaries[0].ModelId != "amazon.titan-embed-text-v2:0" {
		t.Fatalf("filtered: %#v %v", filtered, err)
	}

	detail, err := c.GetFoundationModel(ctx, &sdk.GetFoundationModelInput{ModelIdentifier: new(bedrockClaude)})
	if err != nil || *detail.ModelDetails.ProviderName != "Anthropic" ||
		!*detail.ModelDetails.ResponseStreamingSupported {
		t.Fatalf("model: %#v %v", detail, err)
	}

	_, err = c.ListFoundationModels(
		ctx,
		&sdk.ListFoundationModelsInput{ByCustomizationType: bt.ModelCustomizationFineTuning},
	)
	bedrockError(t, err, "NotImplemented")
	_, err = c.GetFoundationModel(ctx, &sdk.GetFoundationModelInput{ModelIdentifier: new("unknown")})
	bedrockError(t, err, "ValidationException")
	profiles, err := c.ListInferenceProfiles(ctx, &sdk.ListInferenceProfilesInput{MaxResults: new(int32(1))})
	if err != nil || len(profiles.InferenceProfileSummaries) != 1 || profiles.NextToken == nil {
		t.Fatalf("profiles: %#v %v", profiles, err)
	}

	p, err := c.GetInferenceProfile(
		ctx,
		&sdk.GetInferenceProfileInput{InferenceProfileIdentifier: new("us." + bedrockClaude)},
	)
	if err != nil || p.Type != bt.InferenceProfileTypeSystemDefined || len(p.Models) != 2 {
		t.Fatalf("system: %#v %v", p, err)
	}

	input := &sdk.CreateInferenceProfileInput{
		InferenceProfileName: new("my-profile"),
		ClientRequestToken:   new("stable-token"),
		ModelSource:          &bt.InferenceProfileModelSourceMemberCopyFrom{Value: *detail.ModelDetails.ModelArn},
		Tags:                 []bt.Tag{{Key: new("team"), Value: new("core")}},
	}

	app, err := c.CreateInferenceProfile(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	repeated, err := c.CreateInferenceProfile(ctx, input)
	if err != nil || *repeated.InferenceProfileArn != *app.InferenceProfileArn {
		t.Fatalf("idempotency: %#v %v", repeated, err)
	}

	response, err := r.Converse(
		ctx,
		&runtime.ConverseInput{ModelId: app.InferenceProfileArn, Messages: bedrockMessages("profile message")},
	)
	if err != nil || bedrockText(response) != "TinyStack: profile message" {
		t.Fatalf("profile invoke: %#v %v", response, err)
	}

	_, other := bedrockClients(s, "222222222222", "us-east-1")
	_, err = other.Converse(
		ctx,
		&runtime.ConverseInput{ModelId: app.InferenceProfileArn, Messages: bedrockMessages("hello")},
	)
	bedrockError(t, err, "ResourceNotFoundException")
	_, otherRegion := bedrockClients(s, "test", "us-west-2")
	_, err = otherRegion.Converse(ctx, &runtime.ConverseInput{
		ModelId: app.InferenceProfileArn, Messages: bedrockMessages("hello"),
	})
	bedrockError(t, err, "ResourceNotFoundException")

	_, err = c.TagResource(
		ctx,
		&sdk.TagResourceInput{
			ResourceARN: app.InferenceProfileArn,
			Tags:        []bt.Tag{{Key: new("env"), Value: new("test")}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: app.InferenceProfileArn})
	if err != nil || len(tags.Tags) != 2 {
		t.Fatalf("tags: %#v %v", tags, err)
	}

	_, err = c.UntagResource(
		ctx,
		&sdk.UntagResourceInput{ResourceARN: app.InferenceProfileArn, TagKeys: []string{"team"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.DeleteInferenceProfile(
		ctx,
		&sdk.DeleteInferenceProfileInput{InferenceProfileIdentifier: app.InferenceProfileArn},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = r.Converse(
		ctx,
		&runtime.ConverseInput{ModelId: app.InferenceProfileArn, Messages: bedrockMessages("hello")},
	)
	bedrockError(t, err, "ResourceNotFoundException")
}

func TestBedrockGuardrailsLogging(t *testing.T) {
	t.Parallel()
	s := server(t)
	c, r := bedrockClients(s, "test", "us-east-1")
	ctx := context.Background()
	create := &sdk.CreateGuardrailInput{
		Name:                    new("local-policy"),
		BlockedInputMessaging:   new("blocked input"),
		BlockedOutputsMessaging: new("blocked output"),
		WordPolicyConfig: &bt.GuardrailWordPolicyConfig{
			WordsConfig: []bt.GuardrailWordConfig{{Text: new("secret")}},
		},
		Tags: []bt.Tag{{Key: new("team"), Value: new("core")}},
	}

	g, err := c.CreateGuardrail(ctx, create)
	if err != nil {
		t.Fatal(err)
	}

	version, err := c.CreateGuardrailVersion(
		ctx,
		&sdk.CreateGuardrailVersionInput{GuardrailIdentifier: g.GuardrailId, Description: new("snapshot")},
	)
	if err != nil || *version.Version != "1" {
		t.Fatalf("version: %#v %v", version, err)
	}

	_, err = c.UpdateGuardrail(
		ctx,
		&sdk.UpdateGuardrailInput{
			GuardrailIdentifier:     g.GuardrailId,
			Name:                    new("updated"),
			BlockedInputMessaging:   new("new input"),
			BlockedOutputsMessaging: new("new output"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	old, err := c.GetGuardrail(
		ctx,
		&sdk.GetGuardrailInput{GuardrailIdentifier: g.GuardrailArn, GuardrailVersion: version.Version},
	)
	if err != nil || *old.Name != "local-policy" || len(old.WordPolicy.Words) != 1 {
		t.Fatalf("snapshot: %#v %v", old, err)
	}

	list, err := c.ListGuardrails(ctx, &sdk.ListGuardrailsInput{GuardrailIdentifier: g.GuardrailId})
	if err != nil || len(list.Guardrails) != 2 {
		t.Fatalf("list: %#v %v", list, err)
	}

	apply, err := r.ApplyGuardrail(
		ctx,
		&runtime.ApplyGuardrailInput{
			GuardrailIdentifier: g.GuardrailId,
			GuardrailVersion:    version.Version,
			Source:              rt.GuardrailContentSourceInput,
			Content: []rt.GuardrailContentBlock{
				&rt.GuardrailContentBlockMemberText{Value: rt.GuardrailTextBlock{Text: new("A SECRET value")}},
			},
		},
	)
	if err != nil || apply.Action != rt.GuardrailActionGuardrailIntervened ||
		*apply.Outputs[0].Text != "blocked input" {
		t.Fatalf("apply: %#v %v", apply, err)
	}

	tags, err := c.ListTagsForResource(ctx, &sdk.ListTagsForResourceInput{ResourceARN: g.GuardrailArn})
	if err != nil || len(tags.Tags) != 1 {
		t.Fatalf("guardrail tags: %#v %v", tags, err)
	}

	_, err = c.DeleteGuardrail(
		ctx,
		&sdk.DeleteGuardrailInput{GuardrailIdentifier: g.GuardrailId, GuardrailVersion: version.Version},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.GetGuardrail(
		ctx,
		&sdk.GetGuardrailInput{GuardrailIdentifier: g.GuardrailId, GuardrailVersion: version.Version},
	)
	bedrockError(t, err, "ResourceNotFoundException")
	_, err = c.DeleteGuardrail(ctx, &sdk.DeleteGuardrailInput{GuardrailIdentifier: g.GuardrailId})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.PutModelInvocationLoggingConfiguration(
		ctx,
		&sdk.PutModelInvocationLoggingConfigurationInput{
			LoggingConfig: &bt.LoggingConfig{
				TextDataDeliveryEnabled: new(true),
				S3Config:                &bt.S3Config{BucketName: new("logs")},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	logging, err := c.GetModelInvocationLoggingConfiguration(ctx, &sdk.GetModelInvocationLoggingConfigurationInput{})
	if err != nil || logging.LoggingConfig == nil || *logging.LoggingConfig.S3Config.BucketName != "logs" {
		t.Fatalf("logging: %#v %v", logging, err)
	}

	_, err = c.DeleteModelInvocationLoggingConfiguration(ctx, &sdk.DeleteModelInvocationLoggingConfigurationInput{})
	if err != nil {
		t.Fatal(err)
	}

	logging, err = c.GetModelInvocationLoggingConfiguration(ctx, &sdk.GetModelInvocationLoggingConfigurationInput{})
	if err != nil || logging.LoggingConfig != nil {
		t.Fatalf("deleted logging: %#v %v", logging, err)
	}
}

func bedrockMessages(text string) []rt.Message {
	return []rt.Message{
		{Role: rt.ConversationRoleUser, Content: []rt.ContentBlock{&rt.ContentBlockMemberText{Value: text}}},
	}
}

func bedrockText(out *runtime.ConverseOutput) string {
	if out == nil {
		return ""
	}

	message, ok := out.Output.(*rt.ConverseOutputMemberMessage)
	if !ok || len(message.Value.Content) == 0 {
		return ""
	}

	text, ok := message.Value.Content[0].(*rt.ContentBlockMemberText)
	if !ok {
		return ""
	}

	return text.Value
}

func bedrockFixture(t *testing.T, s *httptest.Server, id, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, s.URL+"/_tinystack/bedrock/fixtures/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("fixture: %d %s", response.StatusCode, data)
	}
}

func TestBedrockConverseFixtures(t *testing.T) {
	t.Parallel()
	s := server(t)
	_, r := bedrockClients(s, "test", "us-east-1")
	ctx := context.Background()
	input := &runtime.ConverseInput{ModelId: new(bedrockClaude), Messages: bedrockMessages("hello world")}
	out, err := r.Converse(ctx, input)
	if err != nil || bedrockText(out) != "TinyStack: hello world" || aws.ToInt32(out.Usage.TotalTokens) != 5 {
		t.Fatalf("echo: %#v %v", out, err)
	}

	input.InferenceConfig = &rt.InferenceConfiguration{MaxTokens: new(int32(1))}
	out, err = r.Converse(ctx, input)
	if err != nil || bedrockText(out) != "TinyStack:" || out.StopReason != rt.StopReasonMaxTokens {
		t.Fatalf("truncated: %#v %v", out, err)
	}

	input.InferenceConfig = nil
	bedrockFixture(
		t,
		s,
		"hello",
		fmt.Sprintf(`{"modelId":%q,"contains":"hello","text":"fixture reply","stopReason":"end_turn"}`, bedrockClaude),
	)
	out, err = r.Converse(ctx, input)
	if err != nil || bedrockText(out) != "fixture reply" {
		t.Fatalf("fixture: %#v %v", out, err)
	}

	bedrockFixture(
		t,
		s,
		"tool",
		fmt.Sprintf(
			`{"modelId":%q,"regex":"weather.*Paris","text":"",
 "toolUse":{"toolUseId":"call-1","name":"weather","input":{"city":"Paris"}}}`,
			bedrockClaude,
		),
	)
	input.Messages = bedrockMessages("weather in Paris")
	input.ToolConfig = &rt.ToolConfiguration{
		Tools: []rt.Tool{
			&rt.ToolMemberToolSpec{
				Value: rt.ToolSpecification{
					Name: new("weather"),
					InputSchema: &rt.ToolInputSchemaMemberJson{
						Value: document.NewLazyDocument(map[string]any{"type": "object"}),
					},
				},
			},
		},
	}

	out, err = r.Converse(ctx, input)
	if err != nil || out.StopReason != rt.StopReasonToolUse {
		t.Fatalf("tool: %#v %v", out, err)
	}

	message := out.Output.(*rt.ConverseOutputMemberMessage)
	tool := message.Value.Content[0].(*rt.ContentBlockMemberToolUse)
	if *tool.Value.Name != "weather" {
		t.Fatal(tool)
	}

	input.ToolConfig = nil
	_, err = r.Converse(ctx, input)
	bedrockError(t, err, "ValidationException")
	input.Messages = bedrockMessages("hello")
	input.InferenceConfig = &rt.InferenceConfiguration{Temperature: new(float32(0.5))}
	_, err = r.Converse(ctx, input)
	bedrockError(t, err, "NotImplemented")
	count, err := r.CountTokens(
		ctx,
		&runtime.CountTokensInput{
			ModelId: new(bedrockClaude),
			Input: &rt.CountTokensInputMemberConverse{
				Value: rt.ConverseTokensRequest{Messages: bedrockMessages("one two three")},
			},
		},
	)
	if err != nil || aws.ToInt32(count.InputTokens) != 3 {
		t.Fatalf("tokens: %#v %v", count, err)
	}
}

func TestBedrockNativeModels(t *testing.T) {
	t.Parallel()
	s := server(t)
	_, r := bedrockClients(s, "test", "us-east-1")
	ctx := context.Background()
	cases := []struct{ model, body, field string }{
		{
			bedrockClaude,
			`{"anthropic_version":"bedrock-2023-05-31",
 "messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_tokens":20}`,
			"content",
		},
		{
			"amazon.titan-text-express-v1",
			`{"inputText":"hello","textGenerationConfig":{"maxTokenCount":20}}`,
			"results",
		},
		{
			"amazon.nova-micro-v1:0",
			`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"inferenceConfig":{"maxTokens":20}}`,
			"output",
		},
		{"meta.llama3-8b-instruct-v1:0", `{"prompt":"hello","max_gen_len":20}`, "generation"},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			out, err := r.InvokeModel(
				ctx,
				&runtime.InvokeModelInput{
					ModelId:     new(tc.model),
					ContentType: new("application/json"),
					Body:        []byte(tc.body),
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			var value map[string]any
			if json.Unmarshal(out.Body, &value) != nil || value[tc.field] == nil ||
				!bytes.Contains(out.Body, []byte("TinyStack: hello")) {
				t.Fatalf("native: %s", out.Body)
			}
		})
	}

	for _, tc := range []struct {
		model, body string
		dimension   int
	}{
		{"amazon.titan-embed-text-v2:0", `{"inputText":"hello"}`, 1024},
		{"amazon.titan-embed-text-v2:0", `{"inputText":"hello","dimensions":256}`, 256},
		{"cohere.embed-english-v3", `{"texts":["hello"],"input_type":"search_query"}`, 1024},
		{"cohere.embed-multilingual-v3", `{"texts":["hello"],"input_type":"search_document"}`, 1024},
	} {
		input := &runtime.InvokeModelInput{
			ModelId:     new(tc.model),
			ContentType: new("application/json"),
			Body:        []byte(tc.body),
		}

		first, err := r.InvokeModel(ctx, input)
		if err != nil {
			t.Fatal(err)
		}

		second, err := r.InvokeModel(ctx, input)
		if err != nil || !reflect.DeepEqual(first.Body, second.Body) {
			t.Fatalf("determinism: %v", err)
		}

		var decoded struct {
			Embedding  []float64   `json:"embedding"`
			Embeddings [][]float64 `json:"embeddings"`
		}

		if err = json.Unmarshal(first.Body, &decoded); err != nil {
			t.Fatal(err)
		}

		vector := decoded.Embedding
		if vector == nil {
			vector = decoded.Embeddings[0]
		}

		norm := 0.0
		for _, v := range vector {
			norm += v * v
		}

		if len(vector) != tc.dimension || math.Abs(norm-1) > 1e-9 {
			t.Fatalf("embedding dimension %d norm %v", len(vector), norm)
		}
	}

	for _, tc := range []struct{ model, body string }{
		{bedrockClaude, `{"prompt":"wrong family"}`},
		{"amazon.titan-text-lite-v1", `{"inputText":"hello","temperature":0.5}`},
		{"unknown", `{"inputText":"hello"}`},
	} {
		_, err := r.InvokeModel(
			ctx,
			&runtime.InvokeModelInput{
				ModelId:     new(tc.model),
				ContentType: new("application/json"),
				Body:        []byte(tc.body),
			},
		)
		code := "ValidationException"
		if tc.model == "unknown" {
			code = "ResourceNotFoundException"
		}

		bedrockError(t, err, code)
	}
}

func TestBedrockFixtureResetAndValidation(t *testing.T) {
	t.Parallel()
	s := server(t)
	ctx := context.Background()
	_, r := bedrockClients(s, "test", "us-east-1")
	model := "amazon.titan-text-lite-v1"
	input := &runtime.InvokeModelInput{
		ModelId:     new(model),
		ContentType: new("application/json"),
		Body:        []byte(`{"inputText":"fixture hello"}`),
	}
	bedrockFixture(
		t,
		s,
		"a-first",
		fmt.Sprintf(`{"modelId":%q,"regex":"fixture.*","text":"one two three","stopReason":"stop_sequence"}`, model),
	)
	bedrockFixture(t, s, "z-second", fmt.Sprintf(`{"modelId":%q,"contains":"hello","text":"should not win"}`, model))
	out, err := r.InvokeModel(ctx, input)
	if err != nil || !bytes.Contains(out.Body, []byte("one two three")) {
		t.Fatalf("fixture order: %#v %v", out, err)
	}
	input.Body = []byte(`{"inputText":"fixture hello","textGenerationConfig":{"maxTokenCount":2}}`)
	out, err = r.InvokeModel(ctx, input)
	if err != nil || !bytes.Contains(out.Body, []byte(`"outputText":"one two"`)) ||
		!bytes.Contains(out.Body, []byte(`"completionReason":"LENGTH"`)) {
		t.Fatalf("fixture truncation: %#v %v", out, err)
	}
	_, isolated := bedrockClients(s, "222222222222", "us-east-1")
	out, err = isolated.InvokeModel(ctx, input)
	if err != nil || !bytes.Contains(out.Body, []byte("TinyStack: fixture")) {
		t.Fatalf("fixture isolation: %#v %v", out, err)
	}
	for _, body := range []string{
		`{"modelId":"x","regex":"[","text":"bad"}`,
		`{"modelId":"x","contains":"a","regex":"b","text":"bad"}`,
		`{"modelId":"x","contains":"a","text":"bad","extra":true}`,
		`{"modelId":"x","contains":"a","text":"bad","stopReason":"tool_use"}`,
		`null`,
	} {
		req, requestErr := http.NewRequestWithContext(
			ctx,
			http.MethodPut,
			s.URL+"/_tinystack/bedrock/fixtures/invalid",
			strings.NewReader(body),
		)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("invalid fixture %s: %d", body, response.StatusCode)
		}
	}

	response, err := http.Get(s.URL + "/_tinystack/bedrock/fixtures")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !bytes.Contains(data, []byte("a-first")) {
		t.Fatalf("fixture listing: %s", data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/_tinystack/reset", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("reset: %d", response.StatusCode)
	}
	input.Body = []byte(`{"inputText":"fixture hello"}`)
	out, err = r.InvokeModel(ctx, input)
	if err != nil || !bytes.Contains(out.Body, []byte("TinyStack: fixture hello")) {
		t.Fatalf("fixture reset: %#v %v", out, err)
	}
}
