package awserror

import "fmt"

type Error struct {
	Code, Message string
	StatusCode    int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func New(code, message string) *Error { return &Error{code, message, 400} }

func Validation(format string, args ...any) *Error {
	return New("ValidationException", fmt.Sprintf(format, args...))
}
