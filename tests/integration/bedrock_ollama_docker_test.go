package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	runtime "github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	rt "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestBedrockOllamaDocker(t *testing.T) {
	if os.Getenv("TINYSTACK_DOCKER_TESTS") != "1" {
		t.Skip("set TINYSTACK_DOCKER_TESTS=1 to run Ollama in Docker")
	}

	cfg := testConfig()
	cfg.Bedrock.Inference = "ollama"
	cfg.Bedrock.Ollama.Timeout = 10 * time.Minute
	cfg.Bedrock.Ollama.Models = map[string]string{"anthropic.": "smollm2:135m"}
	s := serverConfig(t, cfg, &fakeRuntime{})
	_, client := bedrockClients(s, "test", "us-east-1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := client.Converse(
		ctx,
		&runtime.ConverseInput{
			ModelId: new(bedrockClaude),
			Messages: []rt.Message{
				{
					Role:    rt.ConversationRoleUser,
					Content: []rt.ContentBlock{&rt.ContentBlockMemberText{Value: "Say hello"}},
				},
			},
			InferenceConfig: &rt.InferenceConfiguration{MaxTokens: new(int32(8))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	text := out.Output.(*rt.ConverseOutputMemberMessage).Value.Content[0].(*rt.ContentBlockMemberText).Value
	if text == "" || out.Usage == nil || *out.Usage.TotalTokens == 0 {
		t.Fatalf("completion: %+v", out)
	}
}
