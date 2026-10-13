package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
)

func TestAuthorizationDenialPreserves403(t *testing.T) {
	for _, mode := range []string{config.AuthorizationEnforce, config.AuthorizationSoft, config.AuthorizationOff} {
		t.Run(mode, func(t *testing.T) {
			s := &Server{}
			c := &call{
				service: &catalog.Service{Errors: map[string]catalog.ErrorBinding{
					"AccessDeniedException": {StatusCode: http.StatusBadRequest},
				}},
				meta: request.Metadata{Protocol: catalog.AWSJSON10},
			}

			if mode != config.AuthorizationOff {
				c.authorization = &AuthorizationRecord{Mode: mode}
			}

			response := httptest.NewRecorder()
			s.fail(response, c, &awserror.Error{
				Code: "AccessDeniedException", Message: "denied", StatusCode: http.StatusForbidden,
			})
			want := http.StatusBadRequest
			if mode == config.AuthorizationEnforce {
				want = http.StatusForbidden
			}

			if response.Code != want {
				t.Fatalf("got %d, want %d", response.Code, want)
			}
		})
	}
}
