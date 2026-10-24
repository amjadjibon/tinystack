// Package mlfixtures stores user-supplied ML answers for the default account, across Regions.
package mlfixtures

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var fixtureID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Handler manages fixtures for the configured default account, across Regions.
// Gateway admin authorization (adminPaths) protects these endpoints, as for costs/fixtures.
func Handler[T any](store storage.Store, account, service string, validate func(*T) error) http.Handler {
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

		id := strings.TrimPrefix(r.URL.Path, "/_tinystack/"+service+"/fixtures")
		id = strings.TrimPrefix(id, "/")
		if id != "" && !fixtureID.MatchString(id) {
			fail(400, fmt.Errorf("invalid fixture ID"))
			return
		}

		scope := request.Scope{AccountID: account}
		key := scope.Key(service, "fixture", id)
		if r.Method == http.MethodGet {
			rows := map[string]*T{}
			err := store.View(r.Context(), func(tx storage.ReadTx) error {
				if id != "" {
					row, ok, e := storage.Read[T](tx, key)
					if e != nil {
						return e
					}

					if !ok {
						return fmt.Errorf("fixture not found")
					}

					rows[id] = row
					return nil
				}

				for _, k := range tx.Keys(scope.Prefix(service, "fixture")) {
					row, _, e := storage.Read[T](tx, k)
					if e != nil {
						return e
					}

					rows[strings.TrimPrefix(k, scope.Prefix(service, "fixture"))] = row
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
			err := store.Update(r.Context(), func(tx storage.WriteTx) error {
				tx.Delete(key)
				return nil
			})
			if err != nil {
				fail(500, err)
				return
			}

			w.WriteHeader(204)
			return
		}

		var in *T
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

		if err := validate(in); err != nil {
			fail(400, err)
			return
		}

		err := store.Update(r.Context(), func(tx storage.WriteTx) error {
			if _, exists := tx.Get(key); !exists && len(tx.Keys(scope.Prefix(service, "fixture"))) >= 1000 {
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
