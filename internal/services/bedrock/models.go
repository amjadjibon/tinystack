package bedrock

import (
	"context"
	"fmt"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

// Model describes one fixed catalogue entry. Dimension is zero for text-generation models.
type Model struct {
	ID        string
	Name      string
	Provider  string
	Family    string
	Inputs    []string
	Inference []string
	Dimension int
	Streaming bool
}

// Catalogue snapshot: AWS model IDs, independent of regional live availability.
var models = []Model{
	{
		ID:        "anthropic.claude-3-haiku-20240307-v1:0",
		Name:      "Claude 3 Haiku",
		Provider:  providerAnthropic,
		Family:    familyAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Name:      "Claude 3.5 Sonnet v2",
		Provider:  providerAnthropic,
		Family:    familyAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-3-7-sonnet-20250219-v1:0",
		Name:      "Claude 3.7 Sonnet",
		Provider:  providerAnthropic,
		Family:    familyAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-sonnet-4-20250514-v1:0",
		Name:      "Claude Sonnet 4",
		Provider:  providerAnthropic,
		Family:    familyAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-opus-4-20250514-v1:0",
		Name:      "Claude Opus 4",
		Provider:  providerAnthropic,
		Family:    familyAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-sonnet-4-5-20250929-v1:0",
		Name:      "Claude Sonnet 4.5",
		Provider:  providerAnthropic,
		Family:    familyAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-haiku-4-5-20251001-v1:0",
		Name:      "Claude Haiku 4.5",
		Provider:  providerAnthropic,
		Family:    familyAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "amazon.titan-text-lite-v1",
		Name:      "Titan Text G1 - Lite",
		Provider:  providerAmazon,
		Family:    "titan",
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand, inferenceProvisioned},
		Streaming: true,
	},
	{
		ID:        "amazon.titan-text-express-v1",
		Name:      "Titan Text G1 - Express",
		Provider:  providerAmazon,
		Family:    "titan",
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand, inferenceProvisioned},
		Streaming: true,
	},
	{
		ID:        "amazon.titan-embed-text-v2:0",
		Name:      "Titan Text Embeddings V2",
		Provider:  providerAmazon,
		Family:    "titan-embed",
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand},
		Dimension: 1024,
	},
	{
		ID:        "amazon.nova-micro-v1:0",
		Name:      "Nova Micro",
		Provider:  providerAmazon,
		Family:    familyNova,
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "amazon.nova-lite-v1:0",
		Name:      "Nova Lite",
		Provider:  providerAmazon,
		Family:    familyNova,
		Inputs:    []string{modalityText, modalityImage, modalityVideo},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "amazon.nova-pro-v1:0",
		Name:      "Nova Pro",
		Provider:  providerAmazon,
		Family:    familyNova,
		Inputs:    []string{modalityText, modalityImage, modalityVideo},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "meta.llama3-8b-instruct-v1:0",
		Name:      "Llama 3 8B Instruct",
		Provider:  providerMeta,
		Family:    familyLlama,
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "meta.llama3-70b-instruct-v1:0",
		Name:      "Llama 3 70B Instruct",
		Provider:  providerMeta,
		Family:    familyLlama,
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "meta.llama3-3-70b-instruct-v1:0",
		Name:      "Llama 3.3 70B Instruct",
		Provider:  providerMeta,
		Family:    familyLlama,
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "cohere.embed-english-v3",
		Name:      "Embed English",
		Provider:  "Cohere",
		Family:    "cohere-embed",
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand},
		Dimension: 1024,
	},
	{
		ID:        "cohere.embed-multilingual-v3",
		Name:      "Embed Multilingual",
		Provider:  "Cohere",
		Family:    "cohere-embed",
		Inputs:    []string{modalityText},
		Inference: []string{inferenceOnDemand},
		Dimension: 1024,
	},
	{
		ID:        "anthropic.claude-opus-4-5-20251101-v1:0",
		Name:      "Claude Opus 4.5",
		Family:    familyAnthropic,
		Provider:  providerAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-opus-4-6-v1",
		Name:      "Claude Opus 4.6",
		Family:    familyAnthropic,
		Provider:  providerAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-sonnet-4-6",
		Name:      "Claude Sonnet 4.6",
		Family:    familyAnthropic,
		Provider:  providerAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "anthropic.claude-opus-4-8",
		Name:      "Claude Opus 4.8",
		Family:    familyAnthropic,
		Provider:  providerAnthropic,
		Inputs:    []string{modalityText, modalityImage},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
	{
		ID:        "amazon.nova-2-lite-v1:0",
		Name:      "Nova 2 Lite",
		Family:    familyNova,
		Provider:  providerAmazon,
		Inputs:    []string{modalityText, modalityImage, modalityVideo},
		Inference: []string{inferenceOnDemand},
		Streaming: true,
	},
}

func findModel(id string) (*Model, error) {
	for i := range models {
		model := &models[i]
		if model.ID == id {
			return common.Clone(model), nil
		}
	}

	return nil, invalid("The provided model identifier is invalid.")
}

func summary(model *Model, region string) api.FoundationModelSummary {
	output := modalityText
	if model.Dimension > 0 {
		output = "EMBEDDING"
	}

	status := "ACTIVE"
	if model.ID == "anthropic.claude-3-5-sonnet-20241022-v2:0" ||
		model.ID == "anthropic.claude-3-7-sonnet-20250219-v1:0" {
		status = "LEGACY"
	}

	return api.FoundationModelSummary{
		ModelId:                 new(model.ID),
		ModelName:               new(model.Name),
		ProviderName:            new(model.Provider),
		ModelArn:                new(fmt.Sprintf("arn:aws:bedrock:%s::foundation-model/%s", region, model.ID)),
		InputModalities:         model.Inputs,
		OutputModalities:        []string{output},
		InferenceTypesSupported: model.Inference,
		ResponseStreamingSupported: new(
			model.Streaming,
		),
		ModelLifecycle: &api.FoundationModelLifecycle{Status: new(status)},
	}
}

// ListFoundationModels filters the built-in catalogue.
func (s *Service) ListFoundationModels(
	ctx context.Context,
	in *api.ListFoundationModelsRequest,
) (*api.ListFoundationModelsResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in.ByOutputModality != nil &&
		!slices.Contains(
			[]string{modalityText, modalityImage, "EMBEDDING", modalityVideo, "AUDIO", "SPEECH"},
			*in.ByOutputModality,
		) {
		return nil, invalid("Invalid output modality.")
	}

	if in.ByInferenceType != nil &&
		!slices.Contains([]string{inferenceOnDemand, inferenceProvisioned}, *in.ByInferenceType) {
		return nil, invalid("Invalid inference type.")
	}

	out := &api.ListFoundationModelsResponse{ModelSummaries: []api.FoundationModelSummary{}}
	for i := range models {
		model := &models[i]
		row := summary(model, scope.Region)
		if in.ByProvider != nil && *in.ByProvider != model.Provider {
			continue
		}

		if in.ByOutputModality != nil && !slices.Contains(row.OutputModalities, *in.ByOutputModality) {
			continue
		}

		if in.ByInferenceType != nil && !slices.Contains(row.InferenceTypesSupported, *in.ByInferenceType) {
			continue
		}

		out.ModelSummaries = append(out.ModelSummaries, row)
	}

	return out, nil
}

// GetFoundationModel returns a model by ID or its regional foundation-model ARN.
func (s *Service) GetFoundationModel(
	ctx context.Context,
	in *api.GetFoundationModelRequest,
) (*api.GetFoundationModelResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	id := common.Value(in.ModelIdentifier)
	prefix := fmt.Sprintf("arn:aws:bedrock:%s::foundation-model/", scope.Region)
	if len(id) > len(prefix) && id[:len(prefix)] == prefix {
		id = id[len(prefix):]
	}

	model, err := findModel(id)
	if err != nil {
		return nil, err
	}

	row := summary(model, scope.Region)
	return &api.GetFoundationModelResponse{ModelDetails: &api.FoundationModelDetails{
		ModelId:                    row.ModelId,
		ModelName:                  row.ModelName,
		ModelArn:                   row.ModelArn,
		ProviderName:               row.ProviderName,
		InputModalities:            row.InputModalities,
		OutputModalities:           row.OutputModalities,
		InferenceTypesSupported:    row.InferenceTypesSupported,
		ResponseStreamingSupported: row.ResponseStreamingSupported,
		ModelLifecycle:             row.ModelLifecycle,
	}}, nil
}
