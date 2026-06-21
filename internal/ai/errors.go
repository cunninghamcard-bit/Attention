package ai

import (
	"errors"
	"fmt"
	"iter"
	"time"
)

var (
	ErrStreamNotStarted      = errors.New("stream: Iter() not called")
	ErrStreamNotDone         = errors.New("stream: iteration not complete")
	ErrStreamMissingResult   = errors.New("stream: ended without producing a message")
	ErrStreamAlreadyStarted  = errors.New("ai.Stream: Iter() called more than once")
	ErrStreamAlreadyConsumed = errors.New("ai.Stream: iterator consumed more than once")
)

type APIError struct {
	API     API
	Model   string
	Message string
}

func (e *APIError) Error() string {
	if e.Model == "" {
		return fmt.Sprintf("%s: %s", e.API, e.Message)
	}
	return fmt.Sprintf("%s %s: %s", e.API, e.Model, e.Message)
}

func errorIter(err error) iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {
		yield(nil, err)
	}
}

func errorMessageEvent(model Model, err error) *StreamEvent {
	message := &Message{
		Role:         RoleAssistant,
		Content:      []ContentBlock{},
		API:          model.API,
		Provider:     model.Provider,
		Model:        model.ID,
		Usage:        &Usage{Cost: &Cost{}},
		StopReason:   StopReasonError,
		ErrorMessage: err.Error(),
		Timestamp:    time.Now().UnixMilli(),
	}
	return &StreamEvent{
		Type:    EventMessageComplete,
		Message: message,
		Usage:   message.Usage,
	}
}
