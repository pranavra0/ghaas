package invocation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"ghaas/internal/invocation"
	"ghaas/internal/state"
)

type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, _ []string) (int, error) {
	<-ctx.Done()
	return -1, ctx.Err()
}

func TestRuntimePersistsFailureAfterContextTimeout(t *testing.T) {
	store := state.NewMemory()
	runtime := invocation.Runtime{Store: store, Runner: blockingRunner{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	id := invocation.InvocationID("timeout/2026-08-29")
	record, err := runtime.Invoke(ctx, "timeout", id, []string{"ignored"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Invoke error = %v, want deadline exceeded", err)
	}
	if record.Status != invocation.StatusFailed {
		t.Fatalf("returned status = %s, want failed", record.Status)
	}
	stored, err := store.Get(context.Background(), "timeout", id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != invocation.StatusFailed {
		t.Fatalf("stored status = %s, want failed", stored.Status)
	}
}

func TestTransitionHelpersRecordMetadata(t *testing.T) {
	now := time.Date(2026, 8, 29, 9, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
	pending := invocation.Invocation{SchemaVersion: 1, Function: "hello", ID: "hello/key", Status: invocation.StatusPending, CreatedAt: now}
	running, err := invocation.Start(pending, now, 42)
	if err != nil {
		t.Fatal(err)
	}
	if running.Attempts != 1 || running.WorkflowRunID != 42 || running.StartedAt == nil || running.StartedAt.Location() != time.UTC {
		t.Fatalf("running metadata = %#v", running)
	}
	done, err := invocation.Succeed(running, now.Add(time.Second), invocation.InvocationResult{})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != invocation.StatusSucceeded || done.CompletedAt == nil || done.Result == nil {
		t.Fatalf("completed record = %#v", done)
	}
}
