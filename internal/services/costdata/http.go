package costdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Handler manages fixtures for the configured default account. The gateway supplies admin authorization.
func Handler(store storage.Store, account string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fail := func(status int, err error) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": err.Error()})
		}

		if r.URL.RawQuery != "" {
			fail(400, fmt.Errorf("query parameters are not supported"))
			return
		}

		const base = "/_tinystack/costs/fixtures"
		id := strings.TrimPrefix(r.URL.Path, base)
		list := id == ""
		if !list {
			id = strings.TrimPrefix(id, "/")
			if !IDPattern.MatchString(id) {
				fail(400, fmt.Errorf("invalid fixture ID"))
				return
			}
		}

		scope := request.Scope{AccountID: account}
		key := scope.Key("costdata", "fixture", id)
		if r.Method == http.MethodGet {
			rows, err := Load(r.Context(), store, scope)
			if err != nil {
				fail(500, err)
				return
			}

			if list {
				out := make([]map[string]any, 0, len(rows))
				for _, row := range rows {
					out = append(
						out,
						map[string]any{
							"id":         row.ID,
							"revision":   row.Revision,
							"lineCount":  row.LineCount,
							"provenance": row.Provenance,
							"updatedAt":  row.UpdatedAt,
						},
					)
				}

				_ = json.NewEncoder(w).Encode(map[string]any{"fixtures": out})
				return
			}

			for _, row := range rows {
				if row.ID == id {
					_ = json.NewEncoder(w).Encode(row)
					return
				}
			}

			fail(404, fmt.Errorf("fixture not found"))
			return
		}

		if list || (r.Method != http.MethodPut && r.Method != http.MethodDelete) {
			w.Header().Set("Allow", "GET, PUT, DELETE")
			w.WriteHeader(405)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				fail(403, fmt.Errorf("cross-origin requests are not allowed"))
				return
			}
		}

		if r.Method == http.MethodDelete {
			found := false
			err := store.Update(
				r.Context(),
				func(tx storage.WriteTx) error { _, found = tx.Get(key); tx.Delete(key); return nil },
			)
			if err != nil {
				fail(500, err)
				return
			}

			if !found {
				fail(404, fmt.Errorf("fixture not found"))
				return
			}

			w.WriteHeader(204)
			return
		}

		var raw json.RawMessage
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		if err := d.Decode(&raw); err != nil {
			fail(400, err)
			return
		}

		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			fail(400, fmt.Errorf("expected exactly one fixture object"))
			return
		}

		// Reject null values instead of silently turning them into zero values.
		tokens := json.NewDecoder(bytes.NewReader(raw))
		tokens.UseNumber()
		for {
			token, err := tokens.Token()
			if err == io.EOF {
				break
			}

			if err != nil || token == nil {
				fail(400, fmt.Errorf("null values are not supported"))
				return
			}
		}

		var in *Input
		strict := json.NewDecoder(bytes.NewReader(raw))
		strict.DisallowUnknownFields()
		if err := strict.Decode(&in); err != nil {
			fail(400, err)
			return
		}

		if err := Validate(in, account); err != nil {
			fail(400, err)
			return
		}

		row := Fixture{
			Input:     *in,
			ID:        id,
			Revision:  common.RandomID(32, "0123456789abcdef"),
			UpdatedAt: time.Now().UTC(),
			LineCount: len(in.Lines),
		}

		err := store.Update(r.Context(), func(tx storage.WriteTx) error {
			if _, exists := tx.Get(key); !exists && len(tx.Keys(scope.Prefix("costdata", "fixture"))) >= MaxFixtures {
				return fmt.Errorf("at most %d fixtures per account", MaxFixtures)
			}

			return storage.Write(tx, key, row)
		})
		if err != nil {
			fail(400, err)
			return
		}

		_ = json.NewEncoder(w).Encode(row)
	})
}
