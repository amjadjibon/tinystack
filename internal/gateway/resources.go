package gateway

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// serveResources lists every service's resources in every account and region,
// or in one account for a signed-in person who is not an administrator.
func (s *Server) serveResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", http.StatusBadRequest)
		return
	}

	out, err := catalog.Resources(r.Context(), s.services)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if account, scoped := accountScope(r.Context()); scoped {
		out = slices.DeleteFunc(out, func(o catalog.Owned) bool { return o.Account != account })
	}

	slices.SortFunc(out, func(a, b catalog.Owned) int {
		return cmp.Or(
			cmp.Compare(a.Service, b.Service),
			cmp.Compare(a.Account, b.Account),
			cmp.Compare(a.Region, b.Region),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.Name, b.Name),
		)
	})

	if out == nil {
		out = []catalog.Owned{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]catalog.Owned{"resources": out})
}
