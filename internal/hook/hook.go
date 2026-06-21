// Package hook provides an event registry for lifecycle hooks.
package hook

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"
)

// Handler processes an event and returns an optional result.
// Returning (nil, nil) means "no opinion" — the previous result is kept.
type Handler func(ctx context.Context, event any) (any, error)

// Registry dispatches events to registered handlers.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string][]Handler

	// OnHandlerError receives handler failures. pi reports handler errors as
	// recoverable extension errors and continues dispatch (runner.ts:698-707);
	// they never abort the emitting operation. Nil falls back to stderr.
	OnHandlerError func(eventType string, err error)
}

// cancellable is implemented by session_before_* results; pi's generic emit
// returns immediately when a handler cancels, so later handlers can never
// override the cancel (runner.ts:692-696).
type cancellable interface {
	Cancelled() bool
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		handlers: make(map[string][]Handler),
	}
}

// On registers a handler for the given event type.
func (r *Registry) On(eventType string, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[eventType] = append(r.handlers[eventType], handler)
}

// HasHandlers reports whether any handlers are registered for the event type.
func (r *Registry) HasHandlers(eventType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.handlers[eventType]) > 0
}

// Handlers returns a snapshot of handlers registered for the event type.
func (r *Registry) Handlers(eventType string) []Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()

	handlers := r.handlers[eventType]
	if len(handlers) == 0 {
		return nil
	}
	out := make([]Handler, len(handlers))
	copy(out, handlers)
	return out
}

// Emit dispatches an event to all handlers registered for its type.
//
// Handlers are called in registration order; the last non-nil result wins,
// except a cancelling session_before_* result which returns immediately.
// Handler errors are reported via OnHandlerError and dispatch continues,
// matching pi's runner (runner.ts:680-712).
func (r *Registry) Emit(ctx context.Context, event any) (any, error) {
	if event == nil {
		return nil, nil
	}

	eventType, err := eventTypeOf(event)
	if err != nil {
		return nil, err
	}

	var result any
	for _, h := range r.Handlers(eventType) {
		res, err := h(ctx, event)
		if err != nil {
			r.ReportHandlerError(eventType, err)
			continue
		}
		if res != nil {
			result = res
			if c, ok := res.(cancellable); ok && c.Cancelled() {
				return result, nil
			}
		}
	}

	return result, nil
}

// EmitFirst dispatches an event and returns the FIRST non-nil handler result,
// skipping the remaining handlers — pi's user_bash semantics
// (runner.ts:829-856).
func (r *Registry) EmitFirst(ctx context.Context, event any) (any, error) {
	if event == nil {
		return nil, nil
	}

	eventType, err := eventTypeOf(event)
	if err != nil {
		return nil, err
	}

	for _, h := range r.Handlers(eventType) {
		res, err := h(ctx, event)
		if err != nil {
			r.ReportHandlerError(eventType, err)
			continue
		}
		if res != nil {
			return res, nil
		}
	}
	return nil, nil
}

// ReportHandlerError routes a handler failure to OnHandlerError (stderr
// fallback). Manual per-handler dispatch sites use it to keep pi's
// report-and-continue semantics (runner.ts:698-707).
func (r *Registry) ReportHandlerError(eventType string, err error) {
	if r.OnHandlerError != nil {
		r.OnHandlerError(eventType, err)
		return
	}
	fmt.Fprintf(os.Stderr, "extension %s handler error: %v\n", eventType, err)
}

// eventTypeOf extracts the Type field from an event struct via reflection.
func eventTypeOf(event any) (string, error) {
	v := reflect.ValueOf(event)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return "", fmt.Errorf("hook: event must be a struct, got %T", event)
	}
	f := v.FieldByName("Type")
	if !f.IsValid() || f.Kind() != reflect.String {
		return "", fmt.Errorf("hook: event struct %T has no string Type field", event)
	}
	return f.String(), nil
}
