package mlfixtures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Reply associates an exact service-specific input with one operation's modeled output.
type Reply[I any] struct {
	Operation string          `json:"operation"`
	Input     I               `json:"input"`
	Output    json.RawMessage `json:"output"`
}

var ErrAmbiguous = errors.New("multiple matching fixtures")

// Find looks in the caller's account across Regions. An absent match has an empty result.
func Find[I any](
	ctx context.Context, store storage.Store, service, operation string, match func(I) bool,
) (json.RawMessage, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	scope.Region = ""
	var out json.RawMessage
	err = store.View(ctx, func(tx storage.ReadTx) error {
		rows, e := storage.Records[Reply[I]](tx, scope.Prefix(service, "fixture"))
		if e != nil {
			return e
		}

		for _, row := range rows {
			if row.Operation != operation || !match(row.Input) {
				continue
			}

			if out != nil {
				return ErrAmbiguous
			}

			out = row.Output
		}

		return nil
	})
	return out, err
}

// Decode checks the fixture's output against the modeled Go response before it is stored or returned.
func Decode[T any](raw json.RawMessage) (*T, error) {
	var out *T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid fixture output: %w", err)
	}

	if out == nil {
		return nil, errors.New("fixture output must be an object")
	}

	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected exactly one fixture output")
	}

	return out, nil
}

func ValidateOutput[T any](raw json.RawMessage) error {
	_, err := Decode[T](raw)
	return err
}
