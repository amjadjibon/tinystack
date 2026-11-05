// Package idempotency replays the committed result of a request retried with the same client token. Tokens are
// kept under the request's scope at <scope>/<service>/token/<action>-<sha256 of token>, beside the service's records.
package idempotency

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type entry struct {
	ExpiresAt time.Time `json:",omitempty"`
	Input     string
	Result    json.RawMessage
}

// Key returns where a token's result is kept, or "" when the request carries no token.
func Key(scope request.Scope, service, action string, token *string) string {
	if common.Value(token) == "" {
		return ""
	}

	return scope.Key(service, "token", action+fmt.Sprintf("-%x", sha256.Sum256([]byte(*token))))
}

// Replay returns the result remembered under key. found is false for an empty key or an unused token; a token reused
// with different input fails with mismatch. An expired token counts as unused.
func Replay[T any](tx storage.ReadTx, key string, input any, mismatch error) (result T, found bool, err error) {
	if key == "" {
		return result, false, nil
	}

	previous, found, err := storage.Read[entry](tx, key)
	if err != nil || !found {
		return result, false, err
	}

	if !previous.ExpiresAt.IsZero() && !time.Now().Before(previous.ExpiresAt) {
		return result, false, nil
	}

	fingerprint, err := fingerprint(input)
	if err != nil {
		return result, false, err
	}

	if previous.Input != fingerprint {
		return result, false, mismatch
	}

	err = json.Unmarshal(previous.Result, &result)
	return result, err == nil, err
}

// Remember keeps result under key for later retries; it does nothing for an empty key.
// expiresAt optionally limits the replay window; without it the result does not expire.
func Remember(tx storage.WriteTx, key string, input, result any, expiresAt ...time.Time) error {
	if key == "" {
		return nil
	}

	fingerprint, err := fingerprint(input)
	if err != nil {
		return err
	}

	data, err := json.Marshal(result)
	if err != nil {
		return err
	}

	record := entry{Input: fingerprint, Result: data}
	if len(expiresAt) > 0 {
		record.ExpiresAt = expiresAt[0]
	}

	return storage.Write(tx, key, record)
}

// fingerprint encodes a struct by Go field name, so label and query members tagged json:"-" still count.
func fingerprint(input any) (string, error) {
	value := reflect.Indirect(reflect.ValueOf(input))
	if value.Kind() == reflect.Struct {
		fields := make(map[string]any, value.NumField())
		for i := range value.NumField() {
			if field := value.Type().Field(i); field.IsExported() {
				fields[field.Name] = value.Field(i).Interface()
			}
		}

		input = fields
	}

	data, err := json.Marshal(input)
	return string(data), err
}
