package hook

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOnAndEmit(t *testing.T) {
	r := NewRegistry()
	var called bool

	r.On("agent_start", func(_ context.Context, event any) (any, error) {
		called = true
		_, ok := event.(AgentStartEvent)
		if !ok {
			t.Fatalf("event type = %T, want AgentStartEvent", event)
		}
		return nil, nil
	})

	result, err := r.Emit(context.Background(), AgentStartEvent{Type: "agent_start"})
	if err != nil {
		t.Fatalf("Emit error = %v", err)
	}
	if result != nil {
		t.Fatalf("result = %v, want nil", result)
	}
	if !called {
		t.Fatal("handler not called")
	}
}

func TestMultipleHandlersLastNonNilWins(t *testing.T) {
	r := NewRegistry()

	r.On("context", func(_ context.Context, _ any) (any, error) {
		return ContextResult{Messages: []any{"first"}}, nil
	})
	r.On("context", func(_ context.Context, _ any) (any, error) {
		return nil, nil // no opinion
	})
	r.On("context", func(_ context.Context, _ any) (any, error) {
		return ContextResult{Messages: []any{"third"}}, nil
	})

	result, err := r.Emit(context.Background(), ContextEvent{Type: "context"})
	if err != nil {
		t.Fatalf("Emit error = %v", err)
	}
	got, ok := result.(ContextResult)
	if !ok {
		t.Fatalf("result type = %T, want ContextResult", result)
	}
	if len(got.Messages) != 1 || got.Messages[0] != "third" {
		t.Fatalf("result = %v, want third", got)
	}
}

func TestHandlerErrorIsReportedAndDispatchContinues(t *testing.T) {
	r := NewRegistry()
	testErr := errors.New("boom")
	var secondCalled bool
	var reported []string
	r.OnHandlerError = func(eventType string, err error) {
		if !errors.Is(err, testErr) {
			t.Fatalf("reported err = %v, want %v", err, testErr)
		}
		reported = append(reported, eventType)
	}

	r.On("context", func(_ context.Context, _ any) (any, error) {
		return nil, testErr
	})
	r.On("context", func(_ context.Context, _ any) (any, error) {
		secondCalled = true
		return ContextResult{}, nil
	})

	// pi reports handler errors as recoverable extension errors and keeps
	// dispatching; they never abort the emitting operation (runner.ts:698-707).
	result, err := r.Emit(context.Background(), ContextEvent{Type: "context"})
	if err != nil {
		t.Fatalf("Emit error = %v, want nil", err)
	}
	if result == nil {
		t.Fatal("result = nil, want second handler result")
	}
	if !secondCalled {
		t.Fatal("second handler was not called after error")
	}
	if len(reported) != 1 || reported[0] != "context" {
		t.Fatalf("reported = %v, want one context error", reported)
	}
}

func TestHasHandlers(t *testing.T) {
	r := NewRegistry()

	if r.HasHandlers("agent_start") {
		t.Fatal("HasHandlers = true before registration")
	}

	r.On("agent_start", func(_ context.Context, _ any) (any, error) {
		return nil, nil
	})

	if !r.HasHandlers("agent_start") {
		t.Fatal("HasHandlers = false after registration")
	}
	if r.HasHandlers("agent_end") {
		t.Fatal("HasHandlers = true for unregistered event type")
	}
}

func TestEmitNoHandlersReturnsNilNil(t *testing.T) {
	r := NewRegistry()

	result, err := r.Emit(context.Background(), AgentStartEvent{Type: "agent_start"})
	if err != nil {
		t.Fatalf("Emit error = %v", err)
	}
	if result != nil {
		t.Fatalf("result = %v, want nil", result)
	}
}

func TestSessionBeforeCancelShortCircuits(t *testing.T) {
	r := NewRegistry()
	var secondCalled bool

	r.On("session_before_compact", func(_ context.Context, _ any) (any, error) {
		return SessionBeforeCompactResult{Cancel: true}, nil
	})
	r.On("session_before_compact", func(_ context.Context, _ any) (any, error) {
		secondCalled = true
		return SessionBeforeCompactResult{
			Compaction: &CompactionResult{Summary: "later"},
		}, nil
	})

	// pi returns immediately when a session_before_* handler cancels — later
	// handlers never run and cannot undo the cancel (runner.ts:692-696).
	result, err := r.Emit(context.Background(), SessionBeforeCompactEvent{Type: "session_before_compact"})
	if err != nil {
		t.Fatalf("Emit error = %v", err)
	}
	got, ok := result.(SessionBeforeCompactResult)
	if !ok {
		t.Fatalf("result type = %T, want SessionBeforeCompactResult", result)
	}
	if !got.Cancel {
		t.Fatal("Cancel = false, want cancelling result returned")
	}
	if secondCalled {
		t.Fatal("second handler ran after cancel")
	}
}

func TestEmitFirstReturnsFirstResultAndStops(t *testing.T) {
	r := NewRegistry()
	var secondCalled bool

	r.On("user_bash", func(_ context.Context, _ any) (any, error) {
		return UserBashEventResult{}, nil
	})
	r.On("user_bash", func(_ context.Context, _ any) (any, error) {
		secondCalled = true
		return UserBashEventResult{}, nil
	})

	// pi's emitUserBash returns the FIRST non-undefined handler result and
	// skips all remaining handlers (runner.ts:829-856).
	result, err := r.EmitFirst(context.Background(), UserBashEvent{Type: "user_bash"})
	if err != nil {
		t.Fatalf("EmitFirst error = %v", err)
	}
	if result == nil {
		t.Fatal("result = nil, want first handler result")
	}
	if secondCalled {
		t.Fatal("second handler ran after first result")
	}
}

func TestHandlersReturnsSnapshot(t *testing.T) {
	r := NewRegistry()
	first := func(context.Context, any) (any, error) { return nil, nil }
	second := func(context.Context, any) (any, error) { return nil, nil }

	r.On("turn_end", first)
	snapshot := r.Handlers("turn_end")
	r.On("turn_end", second)

	if len(snapshot) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snapshot))
	}
	if len(r.Handlers("turn_end")) != 2 {
		t.Fatalf("current handlers len = %d, want 2", len(r.Handlers("turn_end")))
	}
	snapshot[0] = second
	if len(r.Handlers("turn_end")) != 2 {
		t.Fatal("mutating snapshot changed registry handler count")
	}
}

func TestConcurrentSafety(t *testing.T) {
	r := NewRegistry()
	var count atomic.Int64

	r.On("turn_end", func(_ context.Context, _ any) (any, error) {
		count.Add(1)
		return nil, nil
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(3)

		go func() {
			defer wg.Done()
			r.On("turn_end", func(_ context.Context, _ any) (any, error) {
				return nil, nil
			})
		}()

		go func() {
			defer wg.Done()
			r.HasHandlers("turn_end")
		}()

		go func() {
			defer wg.Done()
			r.Emit(context.Background(), TurnEndEvent{Type: "turn_end"})
		}()
	}

	wg.Wait()
}

func TestEmitNilEventReturnsNilNil(t *testing.T) {
	r := NewRegistry()

	result, err := r.Emit(context.Background(), nil)
	if err != nil {
		t.Fatalf("Emit error = %v", err)
	}
	if result != nil {
		t.Fatalf("result = %v, want nil", result)
	}
}

func TestEmitNonStructEventReturnsError(t *testing.T) {
	r := NewRegistry()

	_, err := r.Emit(context.Background(), "not a struct")
	if err == nil {
		t.Fatal("Emit error = nil, want error for non-struct event")
	}
}

func TestEmitEventWithoutTypeFieldReturnsError(t *testing.T) {
	r := NewRegistry()

	_, err := r.Emit(context.Background(), struct{ Name string }{Name: "bad"})
	if err == nil {
		t.Fatal("Emit error = nil, want error for event without Type field")
	}
}
