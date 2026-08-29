package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"ghaas/internal/invocation"
)

func TestMemoryCreateGetCASAndDefensiveCopies(t *testing.T) {
	store := NewMemory()
	started := time.Unix(10, 0).UTC()
	v := invocation.Invocation{
		SchemaVersion: 1,
		Function:      "fn",
		ID:            "fn/one",
		Status:        invocation.StatusPending,
		CreatedAt:     time.Unix(1, 0).UTC(),
		StartedAt:     &started,
		Result:        &invocation.InvocationResult{ExitCode: 0},
	}
	if err := store.Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	started = time.Unix(99, 0).UTC()
	v.Result.ExitCode = 8
	got, err := store.Get(context.Background(), "fn", "fn/one")
	if err != nil {
		t.Fatal(err)
	}
	if got.StartedAt.Unix() != 10 || got.Result.ExitCode != 0 {
		t.Fatalf("store retained caller mutation: %#v", got)
	}
	got.Result.ExitCode = 7
	got2, _ := store.Get(context.Background(), "fn", "fn/one")
	if got2.Result.ExitCode != 0 {
		t.Fatal("Get did not return a defensive copy")
	}

	next := *got2
	next.Status = invocation.StatusRunning
	if err := store.CompareAndSwap(context.Background(), *got2, next); err != nil {
		t.Fatal("CAS: ", err)
	}
	if err := store.CompareAndSwap(context.Background(), *got2, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS = %v, want conflict", err)
	}
	if err := store.Create(context.Background(), v); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Create = %v, want conflict", err)
	}
}

func TestMemoryListLimitAndOrdering(t *testing.T) {
	store := NewMemory()
	for _, key := range []string{"two", "one"} {
		created := time.Unix(map[string]int64{"one": 1, "two": 2}[key], 0).UTC()
		if err := store.Create(context.Background(), invocation.Invocation{
			SchemaVersion: 1, Function: "fn", ID: invocation.InvocationID("fn/" + key),
			Status: invocation.StatusPending, CreatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := store.List(context.Background(), "fn", 1)
	if err != nil || len(items) != 1 || items[0].ID != "fn/one" {
		t.Fatalf("list = %#v, %v", items, err)
	}
}
