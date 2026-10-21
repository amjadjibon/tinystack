package bedrockruntime

import (
	"context"
	"strings"

	controlapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/common"
)

// ApplyGuardrail applies case-insensitive literal word matching to text blocks.
func (s *Service) ApplyGuardrail(
	ctx context.Context,
	in *api.ApplyGuardrailRequest,
) (*api.ApplyGuardrailResponse, error) {
	source := common.Value(in.Source)
	if (source != "INPUT" && source != "OUTPUT") || len(in.Content) == 0 || in.GuardrailVersion == nil {
		return nil, invalid("source, content and guardrailVersion are required.")
	}

	detail, err := s.control.GetGuardrail(
		ctx,
		&controlapi.GetGuardrailRequest{
			GuardrailIdentifier: in.GuardrailIdentifier,
			GuardrailVersion:    in.GuardrailVersion,
		},
	)
	if err != nil {
		return nil, err
	}

	parts := []string{}
	for _, block := range in.Content {
		if block.Text == nil || common.Value(block.Text.Text) == "" {
			return nil, invalid("Only nonempty text guardrail content is supported.")
		}

		parts = append(parts, *block.Text.Text)
	}

	text := strings.ToLower(strings.Join(parts, "\n"))
	out := &api.ApplyGuardrailResponse{
		Action:      new("NONE"),
		Usage:       &api.GuardrailUsage{},
		Outputs:     []api.GuardrailOutputContent{},
		Assessments: []api.GuardrailAssessment{},
	}

	if detail.WordPolicy != nil {
		for _, word := range detail.WordPolicy.Words {
			if !strings.Contains(text, strings.ToLower(common.Value(word.Text))) {
				continue
			}

			out.Action = new("GUARDRAIL_INTERVENED")
			message := detail.BlockedInputMessaging
			if source == "OUTPUT" {
				message = detail.BlockedOutputsMessaging
			}

			out.Outputs = []api.GuardrailOutputContent{{Text: message}}
			break
		}
	}

	return out, nil
}
