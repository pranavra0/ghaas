package invocation_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"ghaas/internal/invocation"
	"ghaas/internal/state"
)

type retryRunner struct{ calls atomic.Int32 }

func (r *retryRunner) Run(context.Context, []string) (int, error) {
	if r.calls.Add(1) == 1 {
		return 1, context.Canceled
	}
	return 0, nil
}

func TestRuntimeRetriesWithoutStaleResult(t *testing.T) {
	runner := &retryRunner{}
	r := invocation.Runtime{
		Store: state.NewMemory(), Runner: runner,
		Retry: invocation.RetryPolicy{MaxAttempts: 2},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	got, err := r.Invoke(context.Background(), "retry", "retry/run", []string{"ignored"})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if got.Status != invocation.StatusSucceeded || got.Attempts != 2 {
		t.Fatalf("record = %#v, want succeeded after two attempts", got)
	}
}
