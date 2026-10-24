package bedrockruntime

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Fixture matches the requested model identifier and the last user message.
// Exactly one of Contains and Regex is required; the first matching ID in lexical order wins.
type Fixture struct {
	ToolUse    *api.ToolUseBlock `json:"toolUse,omitempty"`
	ModelID    string            `json:"modelId"`
	Contains   string            `json:"contains,omitempty"`
	Regex      string            `json:"regex,omitempty"`
	Text       string            `json:"text"`
	StopReason string            `json:"stopReason,omitempty"`
}

func (f *Fixture) matches(text string) bool {
	if f.Contains != "" {
		return strings.Contains(text, f.Contains)
	}

	pattern, err := regexp.Compile(f.Regex)
	return err == nil && pattern.MatchString(text)
}

func (f *Fixture) validate() error {
	if f.ModelID == "" || (f.Contains == "") == (f.Regex == "") {
		return fmt.Errorf("modelId and exactly one of contains or regex are required")
	}

	if f.Regex != "" {
		if _, err := regexp.Compile(f.Regex); err != nil {
			return fmt.Errorf("invalid regex: %w", err)
		}
	}

	if !slices.Contains([]string{"", stopEndTurn, stopMaxTokens, stopSequence, stopToolUse}, f.StopReason) {
		return fmt.Errorf("unsupported stopReason")
	}

	if f.ToolUse != nil {
		if common.Value(f.ToolUse.Name) == "" || common.Value(f.ToolUse.ToolUseId) == "" || f.ToolUse.Input == nil ||
			f.ToolUse.Type != nil {
			return fmt.Errorf("toolUse requires name, toolUseId and input; type is unsupported")
		}

		if f.StopReason != "" && f.StopReason != stopToolUse {
			return fmt.Errorf("toolUse requires tool_use stopReason")
		}

		if _, ok := (*f.ToolUse.Input).(map[string]any); !ok {
			return fmt.Errorf("tool input must be a JSON object")
		}
	} else if f.StopReason == stopToolUse {
		return fmt.Errorf("tool_use requires a toolUse block")
	}

	return nil
}

// Handler manages fixtures for the configured default account, across Regions.
func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, "bedrock", (*Fixture).validate)
}
