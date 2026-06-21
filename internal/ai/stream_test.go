package ai

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
)

func TestStreamResultLifecycle(t *testing.T) {
	final := &Message{Role: RoleAssistant, StopReason: StopReasonStop}
	s := newStream(func(yield func(*StreamEvent, error) bool) {
		if !yield(&StreamEvent{Type: EventTextDelta, Delta: &ContentBlock{Type: ContentText, Text: "hi"}}, nil) {
			return
		}
		yield(&StreamEvent{Type: EventMessageComplete, Message: final}, nil)
	})

	if _, err := s.Result(); !errors.Is(err, ErrStreamNotStarted) {
		t.Fatalf("Result before Iter error = %v, want ErrStreamNotStarted", err)
	}

	var events int
	for _, err := range s.Iter() {
		if err != nil {
			t.Fatal(err)
		}
		events++
	}

	got, err := s.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got != final {
		t.Fatalf("Result returned %#v, want final message", got)
	}
	if events != 2 {
		t.Fatalf("events = %d, want 2", events)
	}
}

func TestPiStyleStreamEntrypoints(t *testing.T) {
	withTestProvider(t, APIAnthropicMessages, providerFunc(
		func(_ context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error] {
			model, _ := GetModel("", opts.Model)
			return func(yield func(*StreamEvent, error) bool) {
				err := &APIError{
					API:     model.API,
					Model:   model.ID,
					Message: "test provider is not implemented yet",
				}
				yield(errorMessageEvent(model, err), nil)
			}
		},
	))

	model, ok := GetModel("", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("test model not registered")
	}

	stream := StreamSimple(context.Background(), model, Context{}, SimpleStreamOptions{})
	for _, err := range stream.Iter() {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.Result(); err != nil {
		t.Fatal(err)
	}

	if _, err := CompleteSimple(context.Background(), model, Context{}, SimpleStreamOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestStreamResultAfterEarlyBreak(t *testing.T) {
	partial := &Message{
		Role:      RoleAssistant,
		Content:   []ContentBlock{{Type: ContentText, Text: "partial"}},
		Timestamp: 1,
	}
	s := newStream(func(yield func(*StreamEvent, error) bool) {
		if !yield(&StreamEvent{Type: EventTextDelta, Message: partial}, nil) {
			return
		}
		yield(&StreamEvent{Type: EventMessageComplete, Message: &Message{Role: RoleAssistant}}, nil)
	})

	for range s.Iter() {
		break
	}

	got, err := s.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got != partial {
		t.Fatalf("Result after early break = %#v, want partial message", got)
	}
	if got.StopReason != StopReasonAborted {
		t.Fatalf("stop reason = %q, want aborted", got.StopReason)
	}
}

func TestStreamDoubleIterPanics(t *testing.T) {
	s := newStream(emptyStream())
	_ = s.Iter()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("second Iter did not panic")
		}
	}()
	_ = s.Iter()
}

func TestStreamIteratorErrorBecomesResultError(t *testing.T) {
	want := errors.New("network reset")
	s := newStream(func(yield func(*StreamEvent, error) bool) {
		yield(nil, want)
	})

	for _, err := range s.Iter() {
		if !errors.Is(err, want) {
			t.Fatalf("iterator error = %v, want %v", err, want)
		}
	}

	if _, err := s.Result(); !errors.Is(err, want) {
		t.Fatalf("Result error = %v, want %v", err, want)
	}
}

func TestStreamResultReturnsPartialWithIteratorError(t *testing.T) {
	want := errors.New("network reset")
	partial := &Message{
		Role:      RoleAssistant,
		Content:   []ContentBlock{{Type: ContentText, Text: "partial"}},
		Timestamp: 1,
	}
	s := newStream(func(yield func(*StreamEvent, error) bool) {
		if !yield(&StreamEvent{Type: EventTextDelta, Message: partial}, nil) {
			return
		}
		yield(nil, want)
	})

	for _, err := range s.Iter() {
		if err != nil && !errors.Is(err, want) {
			t.Fatalf("iterator error = %v, want %v", err, want)
		}
	}

	got, err := s.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got != partial {
		t.Fatalf("Result partial = %#v, want partial message", got)
	}
	if got.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", got.StopReason)
	}
	if got.ErrorMessage != want.Error() {
		t.Fatalf("error message = %q, want %q", got.ErrorMessage, want.Error())
	}
}

func TestCompleteUnknownModelReturnsError(t *testing.T) {
	_, err := Complete(context.Background(), StreamOptions{Model: "missing"})
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("Complete error = %v, want unknown model", err)
	}
}

func TestCompleteReturnsPartialWithIteratorError(t *testing.T) {
	want := errors.New("network reset")
	partial := &Message{
		Role:      RoleAssistant,
		Content:   []ContentBlock{{Type: ContentText, Text: "partial"}},
		Timestamp: 1,
	}
	withTestProvider(t, APIAnthropicMessages, providerFunc(
		func(context.Context, *StreamOptions) iter.Seq2[*StreamEvent, error] {
			return func(yield func(*StreamEvent, error) bool) {
				if !yield(&StreamEvent{Type: EventTextDelta, Message: partial}, nil) {
					return
				}
				yield(nil, want)
			}
		},
	))

	got, err := Complete(context.Background(), StreamOptions{Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	if got != partial {
		t.Fatalf("Complete partial = %#v, want partial message", got)
	}
}

func TestCompleteKeepsProviderErrorMessageWhenTransportAlsoFails(t *testing.T) {
	transportErr := errors.New("connection reset")
	providerMessage := &Message{
		Role:         RoleAssistant,
		Content:      []ContentBlock{{Type: ContentText, Text: "partial"}},
		StopReason:   StopReasonError,
		ErrorMessage: "rate limit",
		Timestamp:    1,
	}
	withTestProvider(t, APIAnthropicMessages, providerFunc(
		func(context.Context, *StreamOptions) iter.Seq2[*StreamEvent, error] {
			return func(yield func(*StreamEvent, error) bool) {
				if !yield(&StreamEvent{
					Type:    EventMessageComplete,
					Message: providerMessage,
				}, nil) {
					return
				}
				yield(nil, transportErr)
			}
		},
	))

	got, err := Complete(context.Background(), StreamOptions{Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	if got != providerMessage {
		t.Fatalf("Complete message = %#v, want provider message", got)
	}
	if got.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want provider error", got.StopReason)
	}
	if got.ErrorMessage != "rate limit" {
		t.Fatalf("error message = %q, want provider error", got.ErrorMessage)
	}
}

func TestCompleteProviderAPIErrorMessage(t *testing.T) {
	withTestProvider(t, APIAnthropicMessages, providerFunc(
		func(_ context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error] {
			model, _ := GetModel("", opts.Model)
			return func(yield func(*StreamEvent, error) bool) {
				err := &APIError{
					API:     model.API,
					Model:   model.ID,
					Message: "test provider is not implemented yet",
				}
				yield(errorMessageEvent(model, err), nil)
			}
		},
	))

	msg, err := Complete(context.Background(), StreamOptions{Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != StopReasonError {
		t.Fatalf("stop reason = %q, want error", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "test provider is not implemented yet") {
		t.Fatalf("error message = %q, want provider error", msg.ErrorMessage)
	}
}

type providerFunc func(ctx context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error]

func (fn providerFunc) Stream(ctx context.Context, opts *StreamOptions) iter.Seq2[*StreamEvent, error] {
	return fn(ctx, opts)
}

func withTestProvider(t *testing.T, api API, provider Provider) {
	t.Helper()
	previous := providers[api]
	providers[api] = provider
	t.Cleanup(func() {
		providers[api] = previous
	})
}

func emptyStream() iter.Seq2[*StreamEvent, error] {
	return func(yield func(*StreamEvent, error) bool) {}
}
