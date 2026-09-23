package awsjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

func Decode(body io.Reader, input any) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return awserror.Validation("Cannot read request body: %v", err)
	}

	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return awserror.New("SerializationException", "Expected a JSON object")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if guardErr := decoder.Decode(input); guardErr != nil {
		return awserror.Validation("Invalid or unsupported request field: %v", guardErr)
	}

	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return awserror.New("SerializationException", "Expected one JSON object")
	}

	return nil
}

func Write(w http.ResponseWriter, status int, output any) {
	data, err := json.Marshal(output)
	if err != nil {
		WriteError(w, &awserror.Error{
			Code:       "InternalServerError",
			Message:    "Response encoding failed",
			StatusCode: 500,
		})
		return
	}

	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// WriteError encodes e; a QueryCode is also sent for query-compatible clients.
func WriteError(w http.ResponseWriter, e *awserror.Error) {
	if e.QueryCode != "" {
		w.Header().Set("X-Amzn-Query-Error", e.QueryCode+";"+e.Fault())
	}

	data, _ := json.Marshal(struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}{e.Code, e.Message})
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(data)
}
