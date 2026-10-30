package bedrockruntime

import (
	"context"
	"encoding/json"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/eventstream"
	"github.com/amjadjibon/tinystack/internal/common"
)

type streamBuilder struct {
	err    error
	events eventstream.Stream
}

func (b *streamBuilder) add(name string, value any) {
	if b.err != nil {
		return
	}

	payload, err := json.Marshal(value)
	if err != nil {
		b.err = err
		return
	}

	b.events = append(b.events, eventstream.Event("event", name, payload))
}

func deltas(text string) []string {
	if text == "" {
		return []string{""}
	}

	runes := []rune(text)
	size := max(1, (len(runes)+2)/3)
	out := []string{}
	for len(runes) > 0 {
		n := min(size, len(runes))
		out = append(out, string(runes[:n]))
		runes = runes[n:]
	}

	return out
}

// ConverseStream applies the same validation and fixture selection as Converse.
func (s *Service) ConverseStream(
	ctx context.Context,
	in *api.ConverseStreamRequest,
) (*api.ConverseStreamResponse, error) {
	out, reply, err := s.converse(ctx, &api.ConverseRequest{
		System:          in.System,
		ModelId:         in.ModelId,
		Messages:        in.Messages,
		InferenceConfig: in.InferenceConfig,
		ToolConfig:      in.ToolConfig,
	}, true)
	if err != nil {
		return nil, err
	}

	stream := &streamBuilder{}
	stream.add("messageStart", map[string]any{fieldRole: roleAssistant})
	block := out.Output.Message.Content[0]
	if block.ToolUse != nil {
		tool := block.ToolUse
		stream.add("contentBlockStart", map[string]any{
			fieldBlockIndex: 0,
			"start": map[string]any{
				"toolUse": map[string]any{"toolUseId": tool.ToolUseId, "name": tool.Name},
			},
		})
		input, e := json.Marshal(tool.Input)
		if e != nil {
			return nil, e
		}

		stream.add("contentBlockDelta", map[string]any{
			fieldBlockIndex: 0,
			fieldDelta:      map[string]any{"toolUse": map[string]any{"input": string(input)}},
		})
	} else {
		for _, text := range replyParts(reply) {
			stream.add(
				"contentBlockDelta",
				map[string]any{fieldBlockIndex: 0, fieldDelta: map[string]any{fieldText: text}},
			)
		}
	}

	stream.add("contentBlockStop", map[string]any{fieldBlockIndex: 0})
	stream.add("messageStop", map[string]any{fieldConverseStop: out.StopReason})
	stream.add("metadata", map[string]any{fieldUsage: out.Usage, "metrics": out.Metrics})
	if stream.err != nil {
		return nil, stream.err
	}

	return &api.ConverseStreamResponse{Stream: &stream.events}, nil
}

// InvokeModelWithResponseStream emits provider-native deltas inside chunk events.
func (s *Service) InvokeModelWithResponseStream(
	ctx context.Context, in *api.InvokeModelWithResponseStreamRequest,
) (*api.InvokeModelWithResponseStreamResponse, error) {
	if err := validateNativeHeaders(in.ContentType, in.Accept); err != nil {
		return nil, err
	}

	model, err := s.control.Resolve(ctx, common.Value(in.ModelId))
	if err != nil {
		return nil, err
	}

	if model.Dimension > 0 {
		return nil, invalid("Embedding models do not support response streaming.")
	}

	parsed, err := s.parseNative(model, in.Body)
	if err != nil {
		return nil, err
	}

	reply, err := s.response(ctx, common.Value(in.ModelId), parsed.Last, parsed.MaxTokens)
	if err != nil {
		return nil, err
	}

	parsed.Chat.Stream = true
	if err := s.infer(ctx, model.ID, reply, parsed.Chat); err != nil {
		return nil, err
	}

	if reply.Tool != nil {
		return nil, invalid("Tool fixtures are supported only by Converse.")
	}

	input, output := replyUsage(reply, parsed.All)
	stream := &streamBuilder{}
	chunk := func(value any) {
		encoded, err := json.Marshal(value)
		if err != nil {
			stream.err = err
			return
		}

		stream.add("chunk", map[string]any{"bytes": encoded})
	}

	metrics := map[string]any{
		"inputTokenCount": input, "outputTokenCount": output,
		"invocationLatency": reply.Latency, "firstByteLatency": reply.Latency,
	}

	switch model.Family {
	case familyAnthropic:
		chunk(map[string]any{fieldType: "message_start", "message": map[string]any{
			"id": "msg_tinystack", fieldType: "message", fieldRole: roleAssistant, fieldModel: model.ID,
			fieldContent: []any{}, fieldStopReason: nil, stopSequence: nil,
			fieldUsage: map[string]any{"input_tokens": input, fieldOutputTokens: 0},
		}})
		chunk(map[string]any{
			fieldType: "content_block_start", fieldIndex: 0,
			"content_block": map[string]any{fieldType: fieldText, fieldText: ""},
		})
		for _, text := range replyParts(reply) {
			chunk(map[string]any{
				fieldType: "content_block_delta", fieldIndex: 0,
				fieldDelta: map[string]any{fieldType: "text_delta", fieldText: text},
			})
		}

		chunk(map[string]any{fieldType: "content_block_stop", fieldIndex: 0})
		chunk(map[string]any{fieldType: "message_delta", fieldDelta: map[string]any{
			fieldStopReason: reply.Stop, stopSequence: nil,
		}, fieldUsage: map[string]any{fieldOutputTokens: output}})
		chunk(map[string]any{fieldType: "message_stop", "amazon-bedrock-invocationMetrics": metrics})
	case familyNova:
		// Nova uses the Converse event vocabulary inside the native chunk envelope.
		chunk(map[string]any{"messageStart": map[string]any{fieldRole: roleAssistant}})
		for _, text := range replyParts(reply) {
			chunk(map[string]any{"contentBlockDelta": map[string]any{
				fieldBlockIndex: 0, fieldDelta: map[string]any{fieldText: text},
			}})
		}

		chunk(map[string]any{"contentBlockStop": map[string]any{fieldBlockIndex: 0}})
		chunk(map[string]any{"messageStop": map[string]any{fieldConverseStop: reply.Stop}})
		chunk(map[string]any{"metadata": map[string]any{
			fieldUsage: map[string]any{
				"inputTokens": input, "outputTokens": output, "totalTokens": input + output,
			},
			"metrics": map[string]any{"latencyMs": reply.Latency},
		}, "amazon-bedrock-invocationMetrics": metrics})
	case familyTitan, familyLlama:
		parts := replyParts(reply)
		for i, text := range parts {
			last := i == len(parts)-1
			var value map[string]any
			if model.Family == familyTitan {
				value = map[string]any{
					"outputText": text, fieldIndex: 0, fieldInputTextTokens: input,
					"totalOutputTextTokenCount": output, "completionReason": nil,
				}

				if last {
					reason := "FINISH"
					if reply.Stop == stopMaxTokens {
						reason = "LENGTH"
					}

					value["completionReason"] = reason
				}
			} else {
				value = map[string]any{
					"generation": text, "prompt_token_count": input,
					"generation_token_count": output, fieldStopReason: nil,
				}

				if last {
					reason := ollamaStop
					if reply.Stop == stopMaxTokens {
						reason = ollamaLength
					}

					value[fieldStopReason] = reason
				}
			}

			if last {
				value["amazon-bedrock-invocationMetrics"] = metrics
			}

			chunk(value)
		}
	default:
		return nil, invalid("Unsupported streaming model family.")
	}

	if stream.err != nil {
		return nil, stream.err
	}

	return &api.InvokeModelWithResponseStreamResponse{Body: &stream.events, ContentType: new("application/json")}, nil
}

const (
	familyAnthropic      = "anthropic"
	familyNova           = "nova"
	familyTitan          = "titan"
	familyLlama          = "llama"
	fieldInputTextTokens = "inputTextTokenCount"
	fieldType            = "type"
	fieldRole            = "role"
	fieldContent         = "content"
	fieldStopReason      = "stop_reason"
	stopSequence         = "stop_sequence"
	fieldOutputTokens    = "output_tokens"
	fieldConverseStop    = "stopReason"
	fieldBlockIndex      = "contentBlockIndex"
	fieldDelta           = "delta"
	fieldUsage           = "usage"
	fieldIndex           = "index"
)
