package awsjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// contentType returns the media type of an AWS JSON protocol version.
func contentType(protocol string) string {
	if protocol == catalog.AWSJSON11 {
		return "application/x-amz-json-1.1"
	}

	return "application/x-amz-json-1.0"
}

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

func Write(w http.ResponseWriter, protocol string, status int, output any) {
	data, err := json.Marshal(output)
	if err != nil {
		WriteError(w, protocol, &awserror.Error{
			Code:       "InternalServerError",
			Message:    "Response encoding failed",
			StatusCode: 500,
		}, "")
		return
	}

	w.Header().Set("Content-Type", contentType(protocol))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// WriteError encodes e, with the message under the modeled message member name;
// a QueryCode is also sent for query-compatible clients.
func WriteError(w http.ResponseWriter, protocol string, e *awserror.Error, messageMember string) {
	if messageMember == "" {
		messageMember = "message"
	}

	if e.QueryCode != "" {
		w.Header().Set("X-Amzn-Query-Error", e.QueryCode+";"+e.Fault())
	}

	body := map[string]any{"__type": e.Code, messageMember: e.Message}
	maps.Copy(body, e.Fields)
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", contentType(protocol))
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(data)
}
