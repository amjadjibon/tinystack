package gateway

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// resource is a catalog resource labeled with the service that holds it.
type resource struct {
	Service string `json:"service"`
	catalog.Resource
}

// serveResources lists every service's resources in every account and region.
func (s *Server) serveResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", http.StatusBadRequest)
		return
	}

	out := []resource{}
	for name, service := range s.services {
		if service.Inventory == nil {
			continue
		}

		resources, err := service.Inventory.Resources(r.Context())
		if err != nil {
			http.Error(w, name+": "+err.Error(), http.StatusInternalServerError)
			return
		}

		for i := range resources {
			out = append(out, resource{Service: name, Resource: resources[i]})
		}
	}

	slices.SortFunc(out, func(a, b resource) int {
		return cmp.Or(
			cmp.Compare(a.Service, b.Service),
			cmp.Compare(a.Account, b.Account),
			cmp.Compare(a.Region, b.Region),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.Name, b.Name),
		)
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]resource{"resources": out})
}
