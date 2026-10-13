package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/config"
)

func TestHealthReportsIdentityWithoutCredentials(t *testing.T) {
	for _, mode := range []string{"", config.AuthorizationOff, config.AuthorizationSoft, config.AuthorizationEnforce} {
		t.Run("authorization="+mode, func(t *testing.T) {
			cfg := config.Default()
			cfg.Identity.Mode = config.IdentityVerify
			cfg.Identity.Authorization = mode
			cfg.Identity.SecretKeys = map[string]string{"private-key": "private-secret"}
			s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}

			response := httptest.NewRecorder()
			s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_tinystack/health", http.NoBody))
			var health struct {
				Identity struct{ Mode, Authorization string }
			}

			if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}

			want := mode
			if want == "" {
				want = config.AuthorizationOff
			}

			if response.Code != http.StatusOK || health.Identity.Mode != config.IdentityVerify ||
				health.Identity.Authorization != want {
				t.Fatalf("health: %d %s", response.Code, response.Body.String())
			}

			if strings.Contains(response.Body.String(), "private-") {
				t.Fatal("health exposed configured credentials")
			}
		})
	}
}
