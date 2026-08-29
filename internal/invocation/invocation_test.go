package invocation

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestTransitionTable(t *testing.T) {
	cases := []struct {
		from, to InvocationStatus
		ok       bool
	}{
		{StatusPending, StatusRunning, true},
		{StatusRunning, StatusSucceeded, true},
		{StatusRunning, StatusFailed, true},
		{StatusFailed, StatusPending, true},
		{StatusFailed, StatusExhausted, true},
		{StatusPending, StatusSucceeded, false},
		{StatusSucceeded, StatusRunning, false},
		{StatusExhausted, StatusPending, false},
	}
	for _, tc := range cases {
		i := Invocation{Status: tc.from}
		err := i.Transition(tc.to)
		if tc.ok && err != nil {
			t.Errorf("%s -> %s: unexpected error: %v", tc.from, tc.to, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s: got %v, want invalid transition", tc.from, tc.to, err)
		}
	}
}

func TestInvocationIDs(t *testing.T) {
	a, err := NewScheduledID("weekly", "2026-W35")
	if err != nil || a != "weekly/2026-W35" {
		t.Fatalf("scheduled ID = %q, %v", a, err)
	}
	b, err := NewManualID("weekly", "0198FB37-1234-4abc-8def-0123456789ab")
	if err != nil || b != "weekly/0198fb37-1234-4abc-8def-0123456789ab" {
		t.Fatalf("manual ID = %q, %v", b, err)
	}
	if _, err := NewManualID("weekly", "not-a-uuid"); err == nil {
		t.Fatal("invalid UUID was accepted")
	}
}

func TestRunnerPreservesArgumentBoundariesAndExitCode(t *testing.T) {
	var out bytes.Buffer
	runner := Runner{Stdout: &out, Stderr: &out}
	code, err := runner.Run(context.Background(), []string{"printf", "%s|%s", "a b", "c"})
	if err != nil || code != 0 || out.String() != "a b|c" {
		t.Fatalf("run = code %d, err %v, output %q", code, err, out.String())
	}
	code, err = runner.Run(context.Background(), []string{"false"})
	if code != 1 || err == nil {
		t.Fatalf("false = code %d, err %v", code, err)
	}
}
