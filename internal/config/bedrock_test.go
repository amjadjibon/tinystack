package config

import (
	"testing"
	"time"
)

func TestBedrockValidation(t *testing.T) {
	for _, tc := range []struct {
		change func(*BedrockConfig)
		name   string
		valid  bool
	}{
		{name: "default", change: func(*BedrockConfig) {}, valid: true},
		{name: "inference", change: func(c *BedrockConfig) { c.Inference = "remote" }, valid: false},
		{name: "url", change: func(c *BedrockConfig) { c.Ollama.URL = "://bad" }, valid: false},
		{name: "scheme", change: func(c *BedrockConfig) { c.Ollama.URL = "ftp://localhost" }, valid: false},
		{name: "server", change: func(c *BedrockConfig) {
			c.Inference = "ollama"
			c.Ollama.URL = "http://127.0.0.1:1234"
		}, valid: true},
		{name: "timeout", change: func(c *BedrockConfig) { c.Ollama.Timeout = 0 }, valid: false},
		{name: "negative", change: func(c *BedrockConfig) { c.Ollama.Timeout = -time.Second }, valid: false},
		{name: "prefix", change: func(c *BedrockConfig) { c.Ollama.Models = map[string]string{"": "model"} }, valid: false},
		{name: "model", change: func(c *BedrockConfig) { c.Ollama.Models = map[string]string{"prefix": ""} }, valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.change(&c.Bedrock)
			err := c.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
}
