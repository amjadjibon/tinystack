package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// BedrockConfig selects deterministic or local model inference.
type BedrockConfig struct {
	Inference string       `yaml:"inference"`
	Ollama    OllamaConfig `yaml:"ollama"`
}

// OllamaConfig configures the server and Bedrock-to-Ollama model mapping.
type OllamaConfig struct {
	Models  map[string]string `yaml:"models"`
	URL     string            `yaml:"url"`
	Image   string            `yaml:"image"`
	Timeout time.Duration     `yaml:"timeout"`
	Pull    bool              `yaml:"pull"`
}

// Bedrock inference modes: local answers deterministically from fixtures or an echo; ollama runs real models.
const (
	InferenceLocal  = "local"
	InferenceOllama = "ollama"
)

const defaultOllamaChat = "llama3.2"

// DefaultBedrock returns reproducible inference and opt-in Ollama defaults.
func DefaultBedrock() BedrockConfig {
	return BedrockConfig{Inference: InferenceLocal, Ollama: OllamaConfig{
		Image: "ollama/ollama:0.40.13", Timeout: 120 * time.Second, Pull: true,
		Models: map[string]string{
			"anthropic.claude-": defaultOllamaChat, "amazon.titan-text-": defaultOllamaChat,
			"amazon.titan-embed-": "nomic-embed-text", "meta.llama": defaultOllamaChat, "mistral.": defaultOllamaChat,
		},
	}}
}

// Validate checks inference configuration even when inference is disabled.
func (c BedrockConfig) Validate() error {
	if c.Inference != InferenceLocal && c.Inference != InferenceOllama {
		return fmt.Errorf("bedrock.inference must be local or ollama")
	}

	if c.Ollama.URL != "" {
		u, err := url.Parse(c.Ollama.URL)
		if err != nil {
			return fmt.Errorf("bedrock.ollama.url: %w", err)
		}

		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("bedrock.ollama.url must be an HTTP server URL")
		}
	}

	if c.Ollama.Timeout <= 0 {
		return fmt.Errorf("bedrock.ollama.timeout must be positive")
	}

	if strings.TrimSpace(c.Ollama.Image) == "" {
		return fmt.Errorf("bedrock.ollama.image must not be empty")
	}

	for prefix, model := range c.Ollama.Models {
		if strings.TrimSpace(prefix) == "" || strings.TrimSpace(model) == "" {
			return fmt.Errorf("bedrock.ollama.models entries must not be empty")
		}
	}

	return nil
}
