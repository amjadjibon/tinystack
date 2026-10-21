package bedrockruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
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

var fixtureID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

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

	if !slices.Contains([]string{"", stopEndTurn, stopMaxTokens, "stop_sequence", stopToolUse}, f.StopReason) {
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
// Gateway admin authorization protects this endpoint, as for costs/fixtures.
func Handler(store storage.Store, account string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, err error) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{fieldMessage: err.Error()})
		}

		if r.URL.RawQuery != "" {
			fail(400, fmt.Errorf("query parameters are not supported"))
			return
		}

		id := strings.TrimPrefix(r.URL.Path, "/_tinystack/bedrock/fixtures")
		id = strings.TrimPrefix(id, "/")
		if id != "" && !fixtureID.MatchString(id) {
			fail(400, fmt.Errorf("invalid fixture ID"))
			return
		}

		scope := request.Scope{AccountID: account}
		key := scope.Key("bedrock", "fixture", id)
		if r.Method == http.MethodGet {
			rows := map[string]*Fixture{}
			err := store.View(r.Context(), func(tx storage.ReadTx) error {
				if id != "" {
					row, ok, e := storage.Read[Fixture](tx, key)
					if e != nil {
						return e
					}

					if !ok {
						return fmt.Errorf("fixture not found")
					}

					rows[id] = row
					return nil
				}

				for _, k := range tx.Keys(scope.Prefix("bedrock", "fixture")) {
					row, _, e := storage.Read[Fixture](tx, k)
					if e != nil {
						return e
					}

					rows[strings.TrimPrefix(k, scope.Prefix("bedrock", "fixture"))] = row
				}

				return nil
			})
			if err != nil {
				fail(404, err)
				return
			}

			if id != "" {
				_ = json.NewEncoder(w).Encode(rows[id])
				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"fixtures": rows})
			return
		}

		if id == "" || (r.Method != http.MethodPut && r.Method != http.MethodDelete) {
			w.Header().Set("Allow", "GET, PUT, DELETE")
			w.WriteHeader(405)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Host != r.Host {
				fail(403, fmt.Errorf("cross-origin requests are not allowed"))
				return
			}
		}

		if r.Method == http.MethodDelete {
			err := store.Update(r.Context(), func(tx storage.WriteTx) error { tx.Delete(key); return nil })
			if err != nil {
				fail(500, err)
				return
			}

			w.WriteHeader(204)
			return
		}

		var in *Fixture
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			fail(400, err)
			return
		}

		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			fail(400, fmt.Errorf("expected exactly one fixture"))
			return
		}

		if in == nil {
			fail(400, fmt.Errorf("fixture must be an object"))
			return
		}

		if err := in.validate(); err != nil {
			fail(400, err)
			return
		}

		err := store.Update(r.Context(), func(tx storage.WriteTx) error {
			if _, exists := tx.Get(key); !exists && len(tx.Keys(scope.Prefix("bedrock", "fixture"))) >= 1000 {
				return fmt.Errorf("at most 1000 fixtures per account")
			}

			return storage.Write(tx, key, in)
		})
		if err != nil {
			fail(400, err)
			return
		}

		_ = json.NewEncoder(w).Encode(in)
	})
}
