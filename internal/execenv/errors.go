package execenv

import "fmt"

type FileErrorCode string

const (
	FileErrorAborted          FileErrorCode = "aborted"
	FileErrorNotFound         FileErrorCode = "not_found"
	FileErrorPermissionDenied FileErrorCode = "permission_denied"
	FileErrorNotDirectory     FileErrorCode = "not_directory"
	FileErrorIsDirectory      FileErrorCode = "is_directory"
	FileErrorInvalid          FileErrorCode = "invalid"
	FileErrorNotSupported     FileErrorCode = "not_supported"
	FileErrorUnknown          FileErrorCode = "unknown"
)

type FileError struct {
	Code FileErrorCode
	Path string
	Err  error
}

func (e *FileError) Error() string {
	if e.Path == "" {
		if e.Err == nil {
			return fmt.Sprintf("file error: %s", e.Code)
		}
		return fmt.Sprintf("file error: %s: %v", e.Code, e.Err)
	}
	if e.Err == nil {
		return fmt.Sprintf("file error: %s: %s", e.Code, e.Path)
	}
	return fmt.Sprintf("file error: %s: %s: %v", e.Code, e.Path, e.Err)
}

func (e *FileError) Unwrap() error {
	return e.Err
}

type ExecutionErrorCode string

const (
	ExecutionErrorAborted          ExecutionErrorCode = "aborted"
	ExecutionErrorTimeout          ExecutionErrorCode = "timeout"
	ExecutionErrorShellUnavailable ExecutionErrorCode = "shell_unavailable"
	ExecutionErrorSpawnError       ExecutionErrorCode = "spawn_error"
	ExecutionErrorCallbackError    ExecutionErrorCode = "callback_error"
	ExecutionErrorUnknown          ExecutionErrorCode = "unknown"
)

type ExecutionError struct {
	Code ExecutionErrorCode
	Err  error
}

func (e *ExecutionError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("execution error: %s", e.Code)
	}
	return fmt.Sprintf("execution error: %s: %v", e.Code, e.Err)
}

func (e *ExecutionError) Unwrap() error {
	return e.Err
}
