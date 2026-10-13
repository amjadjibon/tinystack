package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/config"
)

func TestConsoleCapabilitiesIncludeConfiguredAccounts(t *testing.T) {
	for _, mode := range []string{config.IdentityParse, config.IdentityVerify} {
		t.Run(mode, func(t *testing.T) {
			cfg := config.Default()
			cfg.Identity.Mode = mode
			if mode == config.IdentityVerify {
				cfg.Identity.SecretKeys = map[string]string{"private-default-key": "private-secret"}
			}

			cfg.Identity.Accounts = map[string]string{
				"private-default-key": cfg.Defaults.Account,
				"private-team-key":    "111111111111",
				"private-team-alias":  "111111111111",
			}
			cfg.Accounts = map[string]config.AccountConfig{
				"222222222222": {}, "111111111111": {},
			}
			s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
				catalog.Service{Name: "cloudwatch", SigningName: "monitoring"})
			if err != nil {
				t.Fatal(err)
			}

			response := httptest.NewRecorder()
			s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_tinystack/actions", http.NoBody))
			var capabilities struct {
				SigningNames map[string]string `json:"signing_names"`
				Account      string
				Accounts     []string
			}

			if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil {
				t.Fatal(err)
			}

			want := []string{cfg.Defaults.Account, "111111111111", "222222222222"}
			if response.Code != http.StatusOK || capabilities.Account != cfg.Defaults.Account ||
				!slices.Equal(capabilities.Accounts, want) {
				t.Fatalf("capabilities: %d %s", response.Code, response.Body.String())
			}

			if strings.Contains(response.Body.String(), "private-") {
				t.Fatal("capabilities exposed access-key mappings")
			}

			if capabilities.SigningNames["cloudwatch"] != "monitoring" {
				t.Fatalf("incorrect signing names: %v", capabilities.SigningNames)
			}
		})
	}
}
