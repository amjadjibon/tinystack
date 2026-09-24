// Package restjson implements the AWS REST JSON 1.0 protocol from generated
// bindings: the rest package places HTTP-bound members, and generated JSON tags
// describe document bodies.
package restjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/rest"
)

const contentType = "application/json"

// Decode fills input from the request. labels holds the decoded URI labels and
// body the request payload. Modeled inputs the service does not implement fail.
func Decode(r *http.Request, b *catalog.Binding, labels map[string]string, body []byte, input any) error {
	return rest.Decode(r, b, labels, body, input, decodeJSON)
}

// decodeJSON decodes one JSON object strictly: fields the generated type does
// not declare fail instead of being ignored.
func decodeJSON(body []byte, v any) error {
	malformed := awserror.New("SerializationException", "The request body is not a valid JSON object")
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return malformed
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return rest.NotImplemented("TinyStack does not support the " + field + " field.")
		}

		if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return awserror.New("SerializationException", "Invalid value for "+typeErr.Field)
		}

		return malformed
	}

	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return malformed
	}

	return nil
}

// Write encodes a successful response.
func Write(w http.ResponseWriter, b *catalog.Binding, output any) {
	resp := rest.Encode(w, b, output)
	body := resp.Payload
	if resp.Document {
		encoded, err := json.Marshal(output)
		if err != nil {
			WriteError(w, awserror.From(err), "message")
			return
		}

		body = encoded
		w.Header().Set("Content-Type", contentType)
	}

	rest.Send(w, resp.Status, body, false)
}

// WriteError encodes a REST JSON error: the code in X-Amzn-ErrorType and the
// body, and the message under the modeled message member name.
func WriteError(w http.ResponseWriter, e *awserror.Error, messageMember string) {
	if messageMember == "" {
		messageMember = "message"
	}

	kind := "User"
	if e.Fault() == "Receiver" {
		kind = "Service"
	}

	body, _ := json.Marshal(map[string]string{"__type": e.Code, "Type": kind, messageMember: e.Message})
	w.Header().Set("X-Amzn-ErrorType", e.Code)
	w.Header().Set("Content-Type", contentType)
	w.Header().Del("Content-Length")
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(body)
}
