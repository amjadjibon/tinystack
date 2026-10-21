package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// CloudFormationResponses accepts the capability URL callbacks custom resource
// providers PUT, without requiring AWS credentials or a console session.
func CloudFormationResponses(respond func(context.Context, string, []byte) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			http.Error(w, "Response must be at most 4096 bytes", http.StatusBadRequest)
			return
		}

		id := strings.TrimPrefix(r.URL.Path, "/_tinystack/cloudformation/responses/")
		if err := respond(r.Context(), id, body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}
