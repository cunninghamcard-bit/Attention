package pipeline

import (
	"context"
	"errors"
	"testing"
)

func TestBuildOrderAndShortCircuit(t *testing.T) {
	var trace []string
	mk := func(name string, callNext bool) RunMiddleware {
		return func(ctx context.Context, tc *RunContext, next RunHandler) error {
			trace = append(trace, name+"-in")
			var err error
			if callNext {
				err = next(ctx, tc)
			}
			trace = append(trace, name+"-out")
			return err
		}
	}
	final := func(ctx context.Context, tc *RunContext) error {
		trace = append(trace, "final")
		return nil
	}

	if err := Build(
		final,
		mk("a", true),
		mk("b", true),
	)(context.Background(), &RunContext{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a-in", "b-in", "final", "b-out", "a-out"}
	if len(trace) != len(want) {
		t.Fatalf("order: %v", trace)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("order: %v", trace)
		}
	}

	trace = nil
	_ = Build(
		final,
		mk("a", true),
		mk("stop", false),
	)(context.Background(), &RunContext{})
	for _, s := range trace {
		if s == "final" {
			t.Fatal("short-circuit failed")
		}
	}
}

func TestBuildErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	final := func(ctx context.Context, tc *RunContext) error {
		return boom
	}
	err := Build(final)(context.Background(), &RunContext{})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom got %v", err)
	}
}
