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

func Write(w http.ResponseWriter, status int, output any, err error) {
	if err != nil {
		var awsErr *awserror.Error
		if !errors.As(err, &awsErr) {
			awsErr = &awserror.Error{
				Code:       "InternalServerError",
				Message:    "An internal error occurred",
				StatusCode: 500,
			}
		}
		status = awsErr.StatusCode
		output = struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}{awsErr.Code, awsErr.Message}
	}
	data, marshalErr := json.Marshal(output)
	if marshalErr != nil {
		status = 500
		data = []byte(`{"__type":"InternalServerError","message":"Response encoding failed"}`)
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
