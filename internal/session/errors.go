package session

import "fmt"

type ErrorCode string

const (
	ErrorInvalidSession    ErrorCode = "invalid_session"
	ErrorInvalidEntry      ErrorCode = "invalid_entry"
	ErrorNotFound          ErrorCode = "not_found"
	ErrorInvalidForkTarget ErrorCode = "invalid_fork_target"
	ErrorStorage           ErrorCode = "storage"
	ErrorUnknown           ErrorCode = "unknown"
)

type Error struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Err)
}

func (e *Error) Unwrap() error {
	return e.Err
}

func sessionError(code ErrorCode, message string) error {
	return &Error{Code: code, Message: message}
}

func wrapSessionError(code ErrorCode, message string, err error) error {
	return &Error{Code: code, Message: message, Err: err}
}
