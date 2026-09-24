package awserror

import (
	"errors"
	"fmt"
)

type Error struct {
	Code, Message string
	// Headers are response headers required by an operation error, such as the
	// current object length on an unsatisfiable byte range.
	Headers map[string]string
	// QueryCode is the awsQuery error code; query-compatible JSON services also send it.
	QueryCode  string
	StatusCode int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Fault reports the AWS Query fault type.
func (e *Error) Fault() string {
	if e.StatusCode >= 500 {
		return "Receiver"
	}

	return "Sender"
}

func New(code, message string) *Error { return &Error{Code: code, Message: message, StatusCode: 400} }

func Validation(format string, args ...any) *Error {
	return New("ValidationException", fmt.Sprintf(format, args...))
}

// From returns a copy of err's AWS error, hiding any other error as an internal failure.
func From(err error) *Error {
	var awsErr *Error
	if !errors.As(err, &awsErr) {
		return &Error{Code: "InternalServerError", Message: "An internal error occurred", StatusCode: 500}
	}

	out := *awsErr
	return &out
}
