package bedrockruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrock"
)

type nativeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type nativeBlock struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text"`
}

type anthropicInput struct {
	MaxTokens *int64          `json:"max_tokens"`
	Version   string          `json:"anthropic_version"`
	Messages  []nativeMessage `json:"messages"`
}

type titanInput struct {
	Config *struct {
		MaxTokens *int64 `json:"maxTokenCount"`
	} `json:"textGenerationConfig,omitempty"`
	InputText string `json:"inputText"`
}

type titanEmbeddingInput struct {
	Dimensions *int   `json:"dimensions,omitempty"`
	Normalize  *bool  `json:"normalize,omitempty"`
	InputText  string `json:"inputText"`
}

type cohereInput struct {
	InputType string   `json:"input_type"`
	Texts     []string `json:"texts"`
}

type llamaInput struct {
	MaxTokens *int64 `json:"max_gen_len"`
	Prompt    string `json:"prompt"`
}

type novaInput struct {
	Config *struct {
		MaxTokens *int64 `json:"maxTokens"`
	} `json:"inferenceConfig,omitempty"`
	Messages []nativeMessage `json:"messages"`
}

func strictJSON(body []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return invalid("Invalid or unsupported native model body: " + err.Error())
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid("Expected exactly one JSON object.")
	}

	return nil
}

func nativeMessages(messages []nativeMessage, anthropic bool) ([]api.Message, error) {
	out := []api.Message{}
	for _, message := range messages {
		blocks := []nativeBlock{}
		var text string
		if anthropic && json.Unmarshal(message.Content, &text) == nil {
			blocks = append(blocks, nativeBlock{Text: text})
		} else if err := strictJSONBlocks(message.Content, &blocks); err != nil {
			return nil, err
		}

		converted := api.Message{Role: new(message.Role)}
		for _, block := range blocks {
			if (anthropic && block.Type != "" && block.Type != fieldText) || (!anthropic && block.Type != "") {
				return nil, invalid("Only text message content is supported.")
			}

			converted.Content = append(converted.Content, api.ContentBlock{Text: new(block.Text)})
		}

		out = append(out, converted)
	}

	return out, nil
}

func strictJSONBlocks(body []byte, out *[]nativeBlock) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return invalid("Only text message content arrays are supported.")
	}

	return nil
}

type nativeRequest struct {
	MaxTokens *int64
	Last      string
	All       string
	Texts     []string
	Dimension int
}

func parseNative(model *bedrock.Model, body []byte) (*nativeRequest, error) {
	out := &nativeRequest{Dimension: model.Dimension}
	var messages []api.Message
	switch model.Family {
	case "anthropic":
		var in anthropicInput
		if err := strictJSON(body, &in); err != nil {
			return nil, err
		}

		if in.Version != "bedrock-2023-05-31" || in.MaxTokens == nil {
			return nil, invalid("anthropic_version bedrock-2023-05-31 and max_tokens are required.")
		}

		var err error
		messages, err = nativeMessages(in.Messages, true)
		if err != nil {
			return nil, err
		}

		out.MaxTokens = in.MaxTokens
	case "nova":
		var in novaInput
		if err := strictJSON(body, &in); err != nil {
			return nil, err
		}

		var err error
		messages, err = nativeMessages(in.Messages, false)
		if err != nil {
			return nil, err
		}

		if in.Config != nil {
			out.MaxTokens = in.Config.MaxTokens
		}
	case "titan":
		var in titanInput
		if err := strictJSON(body, &in); err != nil {
			return nil, err
		}

		out.Last = in.InputText
		if in.Config != nil {
			out.MaxTokens = in.Config.MaxTokens
		}
	case "llama":
		var in llamaInput
		if err := strictJSON(body, &in); err != nil {
			return nil, err
		}

		out.Last = in.Prompt
		out.MaxTokens = in.MaxTokens
	case "titan-embed":
		var in titanEmbeddingInput
		if err := strictJSON(body, &in); err != nil {
			return nil, err
		}

		out.Last = in.InputText
		if in.Dimensions != nil {
			if !slices.Contains([]int{256, 512, 1024}, *in.Dimensions) {
				return nil, invalid("Unsupported embedding dimension.")
			}

			out.Dimension = *in.Dimensions
		}

		if in.Normalize != nil && !*in.Normalize {
			return nil, invalid("Only normalized embeddings are supported.")
		}

		out.Texts = []string{in.InputText}
	case "cohere-embed":
		var in cohereInput
		if err := strictJSON(body, &in); err != nil {
			return nil, err
		}

		if len(in.Texts) == 0 || len(in.Texts) > 96 ||
			!slices.Contains(
				[]string{"search_document", "search_query", "classification", "clustering"},
				in.InputType,
			) {
			return nil, invalid("texts and a supported input_type are required.")
		}

		for _, text := range in.Texts {
			if strings.TrimSpace(text) == "" {
				return nil, invalid("Embedding texts cannot be empty.")
			}
		}

		out.Texts = in.Texts
		out.Last = strings.Join(in.Texts, "\n")
	default:
		return nil, invalid("Unsupported model body family.")
	}

	out.All = out.Last
	if messages != nil {
		var err error
		out.Last, out.All, err = lastMessage(messages)
		if err != nil {
			return nil, err
		}
	}

	if strings.TrimSpace(out.Last) == "" {
		return nil, invalid("Input text cannot be empty.")
	}

	if out.MaxTokens != nil && (*out.MaxTokens < 1 || *out.MaxTokens > 1000000) {
		return nil, invalid("Invalid maximum token count.")
	}

	return out, nil
}

func embedding(text string, dimension int) []float64 {
	vector := make([]float64, dimension)
	sum := 0.0
	for i := range vector {
		hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", text, i)))
		value := float64(binary.BigEndian.Uint32(hash[:4]))/float64(math.MaxUint32)*2 - 1
		vector[i] = value
		sum += value * value
	}

	norm := math.Sqrt(sum)
	for i := range vector {
		vector[i] /= norm
	}

	return vector
}

// InvokeModel validates a provider-native request and returns deterministic JSON.
func (s *Service) InvokeModel(ctx context.Context, in *api.InvokeModelRequest) (*api.InvokeModelResponse, error) {
	if common.Value(in.ContentType) != "application/json" ||
		(in.Accept != nil && *in.Accept != "application/json" && *in.Accept != "*/*") {
		return nil, invalid("Only application/json is supported.")
	}

	model, err := s.control.Resolve(ctx, common.Value(in.ModelId))
	if err != nil {
		return nil, err
	}

	parsed, err := parseNative(model, in.Body)
	if err != nil {
		return nil, err
	}

	var body any
	if model.Dimension > 0 {
		if model.Family == "titan-embed" {
			body = map[string]any{
				"embedding":           embedding(parsed.Last, parsed.Dimension),
				"inputTextTokenCount": tokenCount(parsed.Last),
			}
		} else {
			vectors := make([][]float64, len(parsed.Texts))
			for i, text := range parsed.Texts {
				vectors[i] = embedding(text, parsed.Dimension)
			}

			body = map[string]any{
				"embeddings":    vectors,
				"texts":         parsed.Texts,
				"id":            "tinystack-embedding",
				"response_type": "embeddings_floats",
			}
		}
	} else {
		out, e := s.response(ctx, common.Value(in.ModelId), parsed.Last, parsed.MaxTokens)
		if e != nil {
			return nil, e
		}

		if out.Tool != nil {
			return nil, invalid("Tool fixtures are supported only by Converse.")
		}

		input, output := tokenCount(parsed.All), tokenCount(out.Text)
		switch model.Family {
		case "anthropic":
			body = map[string]any{
				"id":            "msg_tinystack",
				"type":          fieldMessage,
				"role":          roleAssistant,
				"model":         model.ID,
				"content":       []any{map[string]any{"type": fieldText, fieldText: out.Text}},
				"stop_reason":   out.Stop,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": input, "output_tokens": output},
			}
		case "nova":
			body = map[string]any{
				"output": map[string]any{
					fieldMessage: map[string]any{
						"role":    roleAssistant,
						"content": []any{map[string]any{fieldText: out.Text}},
					},
				},
				"stopReason": out.Stop,
				"usage": map[string]any{
					"inputTokens":  input,
					"outputTokens": output,
					"totalTokens":  input + output,
				},
			}
		case "titan":
			reason := "FINISH"
			if out.Stop == stopMaxTokens {
				reason = "LENGTH"
			}

			body = map[string]any{
				"inputTextTokenCount": input,
				"results": []any{
					map[string]any{"tokenCount": output, "outputText": out.Text, "completionReason": reason},
				},
			}
		case "llama":
			reason := "stop"
			if out.Stop == stopMaxTokens {
				reason = "length"
			}

			body = map[string]any{
				"generation":             out.Text,
				"prompt_token_count":     input,
				"generation_token_count": output,
				"stop_reason":            reason,
			}
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	return &api.InvokeModelResponse{Body: encoded, ContentType: new("application/json")}, nil
}

// CountTokens uses the same whitespace-token estimate as invocation usage.
func (s *Service) CountTokens(ctx context.Context, in *api.CountTokensRequest) (*api.CountTokensResponse, error) {
	model, err := s.control.Resolve(ctx, common.Value(in.ModelId))
	if err != nil {
		return nil, err
	}

	if in.Input == nil || (in.Input.InvokeModel == nil) == (in.Input.Converse == nil) {
		return nil, invalid("Exactly one CountTokens input is required.")
	}

	all := ""
	if in.Input.InvokeModel != nil {
		parsed, e := parseNative(model, in.Input.InvokeModel.Body)
		if e != nil {
			return nil, e
		}

		all = parsed.All
	} else {
		if model.Dimension > 0 {
			return nil, invalid("Embedding models do not support Converse.")
		}

		_, text, e := lastMessage(in.Input.Converse.Messages)
		if e != nil {
			return nil, e
		}

		all = text
	}

	return &api.CountTokensResponse{InputTokens: new(tokenCount(all))}, nil
}
