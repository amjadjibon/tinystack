// Package bedrockruntime provides deterministic fixtures and optional local Ollama inference.
package bedrockruntime

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/bedrock"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Service shares the control plane's model and profile resolution.
type Service struct {
	store   storage.Store
	control *bedrock.Service
	ollama  *ollamaBackend
}

var _ api.API = (*Service)(nil)

// New constructs the deterministic runtime.
func New(store storage.Store, control *bedrock.Service) *Service {
	return &Service{store: store, control: control}
}

func invalid(message string) error { return awserror.New("ValidationException", message) }

func tokenCount(text string) int64 { return int64(len(strings.Fields(text))) }

func lastMessage(messages []api.Message) (lastText, allText string, err error) {
	if len(messages) == 0 {
		return "", "", invalid("messages must not be empty.")
	}

	last := ""
	all := []string{}
	for _, message := range messages {
		role := common.Value(message.Role)
		if role != roleUser && role != roleAssistant {
			return "", "", invalid("Message role must be user or assistant.")
		}

		if len(message.Content) == 0 {
			return "", "", invalid("Message content cannot be empty.")
		}

		parts := []string{}
		for _, block := range message.Content {
			if block.ToolUse != nil {
				return "", "", invalid("messages.content.toolUse is unsupported")
			}

			if block.Text == nil || *block.Text == "" {
				return "", "", invalid("Only nonempty text input blocks are supported.")
			}

			parts = append(parts, *block.Text)
		}

		text := strings.Join(parts, "\n")
		all = append(all, text)
		if role == roleUser {
			last = text
		}
	}

	if last == "" {
		return "", "", invalid("At least one user message is required.")
	}

	return last, strings.Join(all, "\n"), nil
}

type reply struct {
	Tool                   *api.ToolUseBlock
	Text                   string
	Stop                   string
	Parts                  []string
	Input, Output, Latency int64
	Fixture, Real          bool
}

func (s *Service) response(ctx context.Context, modelID, last string, maxTokens *int64) (*reply, error) {
	if maxTokens != nil && (*maxTokens < 1 || *maxTokens > 1000000) {
		return nil, invalid("maxTokens must be between 1 and 1000000.")
	}

	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &reply{Text: "TinyStack: " + last, Stop: stopEndTurn}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		fixtures, e := storage.Records[Fixture](tx, scope.Prefix("bedrock", "fixture"))
		if e != nil {
			return e
		}

		for _, fixture := range fixtures {
			if fixture.ModelID != modelID {
				continue
			}

			if !fixture.matches(last) {
				continue
			}

			out.Fixture = true
			out.Text = fixture.Text
			out.Stop = fixture.StopReason
			if out.Stop == "" {
				out.Stop = stopEndTurn
			}

			out.Tool = common.Clone(fixture.ToolUse)
			break
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if maxTokens != nil && tokenCount(out.Text) > *maxTokens {
		out.Text = strings.Join(strings.Fields(out.Text)[:int(*maxTokens)], " ")
		out.Stop = stopMaxTokens
		out.Tool = nil
	}

	return out, nil
}

func validateTools(config *api.ToolConfiguration, tool *api.ToolUseBlock) error {
	if config != nil {
		if len(config.Tools) == 0 {
			return invalid("tools cannot be empty.")
		}

		names := map[string]bool{}
		for _, item := range config.Tools {
			if item.ToolSpec == nil || common.Value(item.ToolSpec.Name) == "" || item.ToolSpec.InputSchema == nil ||
				item.ToolSpec.InputSchema.Json == nil {
				return invalid("A tool name and JSON input schema are required.")
			}

			name := *item.ToolSpec.Name
			if names[name] {
				return invalid("Duplicate tool name.")
			}

			names[name] = true
		}
	}

	if tool == nil {
		return nil
	}

	if config == nil {
		return invalid("The matching fixture requires toolConfig.")
	}

	for _, item := range config.Tools {
		if common.Value(item.ToolSpec.Name) == common.Value(tool.Name) {
			return nil
		}
	}

	return invalid("Fixture tool is not declared in toolConfig.")
}

// Converse returns a matching fixture, a local completion, or deterministic text.
func (s *Service) Converse(ctx context.Context, in *api.ConverseRequest) (*api.ConverseResponse, error) {
	out, _, err := s.converse(ctx, in, false)
	return out, err
}

func (s *Service) converse(
	ctx context.Context,
	in *api.ConverseRequest,
	streaming bool,
) (*api.ConverseResponse, *reply, error) {
	cfg := in.InferenceConfig
	sampling := cfg != nil && (cfg.Temperature != nil || cfg.TopP != nil || len(cfg.StopSequences) > 0)
	if s.ollama == nil && (len(in.System) > 0 || sampling) {
		return nil, nil, awserror.New("NotImplemented", "system and sampling options require bedrock.inference: ollama")
	}

	model, err := s.control.Resolve(ctx, common.Value(in.ModelId))
	if err != nil {
		return nil, nil, err
	}

	if model.Dimension > 0 {
		return nil, nil, invalid("Embedding models do not support Converse.")
	}

	last, all, err := lastMessage(in.Messages)
	if err != nil {
		return nil, nil, err
	}

	var maxTokens *int64
	if in.InferenceConfig != nil {
		maxTokens = in.InferenceConfig.MaxTokens
	}

	out, err := s.response(ctx, common.Value(in.ModelId), last, maxTokens)
	if err != nil {
		return nil, nil, err
	}

	if s.ollama != nil && !out.Fixture && in.ToolConfig != nil {
		return nil, nil, invalid("toolConfig is unsupported by Ollama")
	}

	req := chatRequest(in.Messages, in.System, in.InferenceConfig)
	req.Stream = streaming
	if err := s.infer(ctx, model.ID, out, req); err != nil {
		return nil, nil, err
	}

	if err := validateTools(in.ToolConfig, out.Tool); err != nil {
		return nil, nil, err
	}

	blocks := []api.ContentBlock{{Text: new(out.Text)}}
	if out.Tool != nil {
		blocks = []api.ContentBlock{{ToolUse: out.Tool}}
		out.Stop = stopToolUse
	}

	input, output := replyUsage(out, all)
	return &api.ConverseResponse{
		Output: &api.ConverseOutput{Message: &api.Message{Role: new(roleAssistant), Content: blocks}},
		StopReason: new(
			out.Stop,
		),
		Usage:   &api.TokenUsage{InputTokens: new(input), OutputTokens: new(output), TotalTokens: new(input + output)},
		Metrics: &api.ConverseMetrics{LatencyMs: new(out.Latency)},
	}, out, nil
}

const (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopToolUse   = "tool_use"
	fieldMessage  = "message"
	fieldText     = "text"
	roleAssistant = "assistant"
)
