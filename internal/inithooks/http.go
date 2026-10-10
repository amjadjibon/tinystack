package inithooks

import (
	"encoding/json"
	"net/http"
	"path"
	"slices"
	"strings"
)

// Paths lists the paths ServeHTTP answers under prefix: the prefix itself, for
// every stage's progress, and one per stage.
func Paths(prefix string) []string {
	paths := []string{prefix}
	for _, stage := range Stages {
		paths = append(paths, prefix+"/"+string(stage))
	}

	return paths
}

// ServeHTTP reports progress as JSON: every stage at the path ending in "init",
// and one stage at the path ending in its name, like LocalStack's /_localstack/init.
func (r *Runner) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", http.StatusBadRequest)
		return
	}

	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var body any = r.Status()
	if last := path.Base(strings.TrimSuffix(req.URL.Path, "/")); last != "init" {
		stage := Stage(strings.ToLower(last))
		if !slices.Contains(Stages, stage) {
			http.NotFound(w, req)
			return
		}

		body = r.StageStatus(stage)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
