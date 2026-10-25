package bedrockagentruntime

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Step describes a user-selected action. Parameters are strings, as in Lambda action events.
type Step struct {
	Parameters  map[string]string `json:"parameters,omitempty"`
	RequestBody map[string]any    `json:"requestBody,omitempty"`
	ActionGroup string            `json:"actionGroup"`
	APIPath     string            `json:"apiPath,omitempty"`
	HTTPMethod  string            `json:"httpMethod,omitempty"`
	Function    string            `json:"function,omitempty"`
}

// Fixture supplies the complete answer and optional deterministic action plan.
type Fixture struct {
	AgentID       string `json:"agentId"`
	AliasID       string `json:"aliasId,omitempty"`
	Contains      string `json:"contains,omitempty"`
	Regex         string `json:"regex,omitempty"`
	Text          string `json:"text"`
	MemorySummary string `json:"memorySummary,omitempty"`
	Steps         []Step `json:"steps,omitempty"`
}

func (f *Fixture) matches(agent, alias, input string) bool {
	if f.AgentID != agent || (f.AliasID != "" && f.AliasID != alias) {
		return false
	}

	if f.Contains != "" {
		return strings.Contains(input, f.Contains)
	}

	pattern, err := regexp.Compile(f.Regex)
	return err == nil && pattern.MatchString(input)
}

func (f *Fixture) validate() error {
	if f.AgentID == "" || (f.Contains == "") == (f.Regex == "") {
		return fmt.Errorf("agentId and exactly one of contains or regex are required")
	}

	if f.Regex != "" {
		if _, checkErr1 := regexp.Compile(f.Regex); checkErr1 != nil {
			return fmt.Errorf("invalid regex: %w", checkErr1)
		}
	}

	for _, step := range f.Steps {
		if step.ActionGroup == "" || (step.Function == "") == (step.APIPath == "" || step.HTTPMethod == "") {
			return fmt.Errorf("each step requires actionGroup and either function or apiPath/httpMethod")
		}
	}

	return nil
}

// Handler manages user-defined agent fixtures through the shared admin handler.
func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, "bedrock-agent", (*Fixture).validate)
}
