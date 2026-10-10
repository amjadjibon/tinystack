package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/amjadjibon/tinystack/internal/inithooks"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Reset clears all state: the services' in-memory state and containers, which
// need their records to find them, then every record in one transaction, then
// the request and delivery logs. Background work keeps running and finds
// nothing to do. Work in flight can still write a record back, and some state
// is not cleared; see the documentation of the reset endpoint.
func (a *App) Reset(ctx context.Context) error {
	a.resetMu.Lock()
	defer a.resetMu.Unlock()
	for _, reset := range a.resets {
		reset(ctx)
	}

	if err := storage.Clear(ctx, a.store); err != nil {
		return err
	}

	a.deliveries.Clear()
	a.Gateway.ClearRequests()
	return nil
}

// serveReset clears all state on POST /_tinystack/reset, like the other
// /_tinystack/ endpoints without signing, and with ?init=1 runs the ready
// stage's scripts afterwards.
func (a *App) serveReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// A web page can send a POST to a loopback address; only same-origin pages may.
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
			http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
			return
		}
	}

	query := r.URL.Query()
	rerun := query.Get("init") == "1"
	if len(query) > 1 || (len(query) == 1 && !rerun) {
		http.Error(w, "the only query parameter is init=1", http.StatusBadRequest)
		return
	}

	if err := a.Reset(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := map[string]any{"reset": "ok"}
	if rerun {
		a.Init.Run(r.Context(), inithooks.Ready)
		out["init"] = a.Init.StageStatus(inithooks.Ready)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
