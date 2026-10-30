package bedrockruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/services/bedrockruntime/ollama"
)

type ollamaBackend struct {
	runtime *ollama.Runtime
	client  *http.Client
	// downloads has no timeout: first-use model pulls are multi-gigabyte and bounded only by the caller.
	downloads *http.Client
	pulled    map[string]bool
	config    config.OllamaConfig
	mu        sync.Mutex
}

// UseOllama enables real inference. No HTTP or Docker work happens until invocation.
func (s *Service) UseOllama(cfg config.OllamaConfig, owner string) {
	if len(cfg.Models) == 0 {
		cfg.Models = config.DefaultBedrock().Ollama.Models
	}

	s.ollama = &ollamaBackend{
		config: cfg, client: &http.Client{Timeout: cfg.Timeout}, downloads: &http.Client{}, pulled: map[string]bool{},
	}

	if cfg.URL == "" {
		s.ollama.runtime = ollama.New(cfg.Image, owner)
	}
}

// Stop releases the owned server while retaining downloaded models.
func (s *Service) Stop() {
	if s.ollama != nil && s.ollama.runtime != nil {
		s.ollama.runtime.Stop()
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaRequest struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
	Input    []string       `json:"input,omitempty"`
	Stream   bool           `json:"stream"`
}

type ollamaResponse struct {
	Message    chatMessage `json:"message"`
	Error      string      `json:"error"`
	Reason     string      `json:"done_reason"`
	Embeddings [][]float64 `json:"embeddings"`
	Input      int64       `json:"prompt_eval_count"`
	Output     int64       `json:"eval_count"`
	Done       bool        `json:"done"`
}

func (b *ollamaBackend) mapped(id string) (string, error) {
	prefix, model := "", ""
	for candidate, mapped := range b.config.Models {
		if strings.HasPrefix(id, candidate) && len(candidate) > len(prefix) {
			prefix, model = candidate, mapped
		}
	}

	if model == "" {
		return "", awserror.New("ResourceNotFoundException", "No Ollama model mapping for "+id)
	}

	return model, nil
}

func post(ctx context.Context, client *http.Client, endpoint, path string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(endpoint, "/")+path,
		bytes.NewReader(encoded),
	)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, awserror.New("ServiceUnavailableException", err.Error())
	}

	return res, nil
}

func ollamaError(res *http.Response, model string) error {
	var body ollamaResponse
	err := json.NewDecoder(res.Body).Decode(&body)
	if err != nil {
		body.Error = fmt.Sprintf("Ollama HTTP %d", res.StatusCode)
	}

	if res.StatusCode == http.StatusNotFound {
		return awserror.New("ResourceNotFoundException", model+": "+body.Error)
	}

	return awserror.New("ModelErrorException", body.Error)
}

// call bounds each model request by the configured timeout; starting the container (which may pull the image)
// and pulling a missing model are bounded only by the caller's context.
func (b *ollamaBackend) call(ctx context.Context, id string, req *ollamaRequest) (*ollamaResponse, []string, error) {
	model, err := b.mapped(id)
	if err != nil {
		return nil, nil, err
	}

	req.Model = model
	endpoint := b.config.URL
	if b.runtime != nil {
		endpoint, err = b.runtime.Endpoint(ctx)
		if err != nil {
			return nil, nil, awserror.New("ServiceUnavailableException", err.Error())
		}
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, b.config.Timeout)
	defer cancel()

	path := "/api/chat"
	if req.Input != nil {
		path = "/api/embed"
	}

	res, err := post(ctx, b.client, endpoint, path, req)
	if err != nil {
		return nil, nil, err
	}

	if res.StatusCode == http.StatusNotFound && b.config.Pull {
		_ = res.Body.Close()
		err = b.pull(parent, endpoint, model)
		if err != nil {
			return nil, nil, err
		}

		cancel()
		ctx, cancel = context.WithTimeout(parent, b.config.Timeout)
		defer cancel()
		res, err = post(ctx, b.client, endpoint, path, req)
		if err != nil {
			return nil, nil, err
		}
	}

	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, nil, ollamaError(res, model)
	}

	decoder := json.NewDecoder(res.Body)
	out := &ollamaResponse{}
	parts := []string{}
	for {
		var chunk ollamaResponse
		err := decoder.Decode(&chunk)
		if err == io.EOF {
			if req.Stream && !out.Done {
				return nil, nil, awserror.New("ModelErrorException", "Ollama stream ended before done")
			}

			break
		}

		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
				return nil, nil, awserror.New("ServiceUnavailableException", err.Error())
			}

			return nil, nil, awserror.New("ModelErrorException", err.Error())
		}

		if chunk.Error != "" {
			return nil, nil, awserror.New("ModelErrorException", chunk.Error)
		}

		if chunk.Message.Content != "" {
			parts = append(parts, chunk.Message.Content)
		}

		*out = chunk
		if !req.Stream || chunk.Done {
			break
		}
	}

	out.Message.Content = strings.Join(parts, "")
	return out, parts, nil
}

func (b *ollamaBackend) pull(ctx context.Context, endpoint, model string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pulled[model] {
		return nil
	}

	res, err := post(ctx, b.downloads, endpoint, "/api/pull", map[string]any{fieldModel: model, "stream": false})
	if err != nil {
		return err
	}

	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return ollamaError(res, model)
	}

	var out ollamaResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return awserror.New("ModelErrorException", err.Error())
	}

	if out.Error != "" {
		return awserror.New("ModelErrorException", out.Error)
	}

	b.pulled[model] = true
	return nil
}

func chatRequest(
	messages []api.Message,
	system []api.SystemContentBlock,
	cfg *api.InferenceConfiguration,
) *ollamaRequest {
	req := &ollamaRequest{Options: map[string]any{}}
	for _, block := range system {
		req.Messages = append(req.Messages, chatMessage{Role: roleSystem, Content: common.Value(block.Text)})
	}

	for _, message := range messages {
		parts := []string{}
		for _, block := range message.Content {
			parts = append(parts, common.Value(block.Text))
		}

		req.Messages = append(
			req.Messages,
			chatMessage{Role: common.Value(message.Role), Content: strings.Join(parts, "\n")},
		)
	}

	if cfg != nil {
		if cfg.MaxTokens != nil {
			req.Options["num_predict"] = *cfg.MaxTokens
		}

		if cfg.Temperature != nil {
			req.Options["temperature"] = *cfg.Temperature
		}

		if cfg.TopP != nil {
			req.Options["top_p"] = *cfg.TopP
		}

		if len(cfg.StopSequences) > 0 {
			req.Options[ollamaStop] = cfg.StopSequences
		}
	}

	return req
}

func (s *Service) infer(ctx context.Context, id string, out *reply, req *ollamaRequest) error {
	if s.ollama == nil || out.Fixture {
		return nil
	}

	start := time.Now()
	result, parts, err := s.ollama.call(ctx, id, req)
	if err != nil {
		return err
	}

	out.Text, out.Input, out.Output, out.Parts = result.Message.Content, result.Input, result.Output, parts
	out.Latency = time.Since(start).Milliseconds()
	out.Real = true
	// Ollama reports "stop" both for a natural end and for a matched stop sequence, so only length is distinguished.
	out.Stop = stopEndTurn
	if result.Reason == ollamaLength {
		out.Stop = stopMaxTokens
	}

	return nil
}

func replyParts(out *reply) []string {
	if out.Real {
		return out.Parts
	}

	return deltas(out.Text)
}

func replyUsage(out *reply, all string) (input, output int64) {
	if out.Real {
		return out.Input, out.Output
	}

	return tokenCount(all), tokenCount(out.Text)
}

const (
	roleUser     = "user"
	roleSystem   = "system"
	fieldModel   = "model"
	ollamaStop   = "stop"
	ollamaLength = "length"
)
