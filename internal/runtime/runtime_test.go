package runtime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pranavra0/ghaas/internal/invocation"
	"github.com/pranavra0/ghaas/internal/state"
	"github.com/pranavra0/ghaas/pkg/manifest"
)

func runtimeLoader(m manifest.Manifest) Loader {
	return func(string) (manifest.Manifest, error) { return m, nil }
}

func TestInvokePreservesArgvAndMetadataPrecedence(t *testing.T) {
	t.Setenv("GHAAS_INVOCATION_ID", "input-id")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"echo": {Runtime: manifest.RuntimeCommand, Command: []string{"printf", "%s|%s|%s|%s", "a b", "$HOME", "x=y", ""}, Environment: map[string]string{"GHAAS_FUNCTION": "spoof", "CUSTOM": "manifest"}},
	}}
	var out bytes.Buffer
	code, err := Invoke(context.Background(), "echo", Options{LoadManifest: runtimeLoader(m), Environment: []string{"CUSTOM=ambient", "HOME=/tmp"}, Stdout: &out, Stderr: &out, Runner: invocation.Runner{}})
	if err != nil || code != 0 {
		t.Fatalf("Invoke = %d, %v", code, err)
	}
	if got, want := out.String(), "a b|$HOME|x=y|"; got != want {
		t.Fatalf("argv output = %q, want %q", got, want)
	}

}
func TestInvokeMergesManifestAndOwnedMetadata(t *testing.T) {
	t.Setenv("GHAAS_INVOCATION_ID", "input-id")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"env": {Runtime: manifest.RuntimeCommand, Command: []string{"env"}, Environment: map[string]string{"CUSTOM": "manifest", "GHAAS_FUNCTION": "spoof"}},
	}}
	var out bytes.Buffer
	code, err := Invoke(context.Background(), "env", Options{LoadManifest: runtimeLoader(m), Environment: []string{"CUSTOM=ambient", "GHAAS_FUNCTION=ambient", "GHAAS_INVOCATION_ID=input-id", "GITHUB_EVENT_NAME=workflow_dispatch"}, Stdout: &out, Stderr: &out, Runner: invocation.Runner{}})
	if err != nil || code != 0 {
		t.Fatalf("Invoke = %d, %v", code, err)
	}
	text := out.String()
	for _, expected := range []string{"CUSTOM=manifest", "GHAAS_FUNCTION=env", "GHAAS_INVOCATION_ID=env/input-id", "GHAAS_ATTEMPT=1", "GHAAS_TRIGGER=manual", "GHAAS_WORKFLOW_RUN_ATTEMPT=1", "GHAAS_ATTEMPT_REASON=workflow_dispatch"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("environment missing %q: %s", expected, text)
		}
	}
}
func TestInvokeStripsStateCredentialsFromChild(t *testing.T) {
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"env": {
			Runtime: manifest.RuntimeCommand,
			Command: []string{"env"},
			Environment: map[string]string{
				"CUSTOM":             "manifest",
				"GITHUB_TOKEN":       "manifest-token",
				"GH_TOKEN":           "manifest-gh-token",
				"GHAAS_STATE_TOKEN":  "manifest-state-token",
				"GHAAS_STATE_SECRET": "manifest-state-secret",
			},
		},
	}}
	var out bytes.Buffer
	code, err := Invoke(context.Background(), "env", Options{
		LoadManifest: runtimeLoader(m),
		Environment: []string{
			"CUSTOM=ambient",
			"GITHUB_TOKEN=ambient-token",
			"GH_TOKEN=ambient-gh-token",
			"GHAAS_STATE_TOKEN=ambient-state-token",
			"GHAAS_STATE_SECRET=ambient-state-secret",
		},
		Stdout: &out, Stderr: &out,
	})
	if err != nil || code != 0 {
		t.Fatalf("Invoke = %d, %v", code, err)
	}
	text := out.String()
	for _, forbidden := range []string{"GITHUB_TOKEN=", "GH_TOKEN=", "GHAAS_STATE_TOKEN=", "GHAAS_STATE_SECRET="} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("child environment leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "CUSTOM=manifest") {
		t.Fatalf("child environment lost ordinary values: %s", text)
	}
}

func TestInvokeStripsStateCredentialsFromRunnerEnvironment(t *testing.T) {
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"env": {Runtime: manifest.RuntimeCommand, Command: []string{"env"}},
	}}
	var out bytes.Buffer
	code, err := Invoke(context.Background(), "env", Options{
		LoadManifest: runtimeLoader(m),
		Runner: invocation.Runner{
			Env: []string{
				"CUSTOM=ambient",
				"GITHUB_TOKEN=ambient-token",
				"GH_TOKEN=ambient-gh-token",
				"GHAAS_STATE_TOKEN=ambient-state-token",
				"GHAAS_STATE_SECRET=ambient-state-secret",
			},
		},
		Stdout: &out, Stderr: &out,
	})
	if err != nil || code != 0 {
		t.Fatalf("Invoke = %d, %v", code, err)
	}
	text := out.String()
	for _, forbidden := range []string{"GITHUB_TOKEN=", "GH_TOKEN=", "GHAAS_STATE_TOKEN=", "GHAAS_STATE_SECRET="} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("runner environment leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "CUSTOM=ambient") {
		t.Fatalf("runner environment lost ordinary values: %s", text)
	}
}

func TestInvokeTimeoutCancelsCommand(t *testing.T) {
	m := manifest.Manifest{Version: 1, Defaults: manifest.Defaults{Timeout: manifest.Duration(20 * time.Millisecond)}, Functions: map[string]manifest.Function{
		"slow": {Runtime: manifest.RuntimeCommand, Command: []string{"sleep", "1"}},
	}}
	started := time.Now()
	code, err := Invoke(context.Background(), "slow", Options{LoadManifest: runtimeLoader(m), Runner: invocation.Runner{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timeout = %d, %v", code, err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestInvokeUsesProviderRunFallback(t *testing.T) {
	t.Setenv("GHAAS_INVOCATION_ID", "")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_EVENT_NAME", "schedule")
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"env": {Runtime: manifest.RuntimeCommand, Command: []string{"env"}},
	}}
	var out bytes.Buffer
	code, err := Invoke(context.Background(), "env", Options{LoadManifest: runtimeLoader(m), Stdout: &out, Stderr: &out, Runner: invocation.Runner{}})
	if err != nil || code != 0 {
		t.Fatalf("Invoke = %d, %v", code, err)
	}
	text := out.String()
	if !strings.Contains(text, "GHAAS_INVOCATION_ID=env/42") {
		t.Fatalf("fallback ID missing: %s", text)
	}
	if !strings.Contains(text, "GHAAS_TRIGGER=schedule") {
		t.Fatalf("schedule trigger missing: %s", text)
	}
}
func TestInvokeHonorsCancellation(t *testing.T) {
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"slow": {Runtime: manifest.RuntimeCommand, Command: []string{"sleep", "1"}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, err := Invoke(ctx, "slow", Options{LoadManifest: runtimeLoader(m), Runner: invocation.Runner{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("cancellation = %d, %v", code, err)
	}
}

type durableRuntimeStore struct {
	invocation          state.Invocation
	acquires            int
	completes           int
	providers           []state.Provider
	function            string
	id                  string
	max                 int
	leaseTTL            time.Duration
	renewErr            error
	completeCtxErr      error
	completeHasDeadline bool
	exhaustOnFailure    bool
}

func (s *durableRuntimeStore) Ensure(_ context.Context, function, id string, maxAttempts ...int) (state.Invocation, error) {
	s.function, s.id = function, id
	if len(maxAttempts) > 0 {
		s.max = maxAttempts[0]
	}
	if s.invocation.Status == "" {
		max := 2
		if len(maxAttempts) > 0 {
			max = maxAttempts[0]
		}
		s.invocation = state.Invocation{Function: function, ID: id, Status: state.StatusPending, MaxAttempts: max}
	}
	return s.invocation, nil
}

func (s *durableRuntimeStore) Acquire(ctx context.Context, function, id, owner string) (state.Invocation, state.Lease, error) {
	s.acquires++
	s.invocation.Attempts++
	s.invocation.Function, s.invocation.ID = function, id
	s.invocation.Status = state.StatusRunning
	ttl := s.leaseTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	return s.invocation, state.Lease{Function: function, ID: id, Owner: owner, Token: "opaque", Fence: uint64(s.acquires), ExpiresAt: time.Now().Add(ttl)}, nil
}

func (s *durableRuntimeStore) Renew(_ context.Context, lease state.Lease) (state.Lease, error) {
	if s.renewErr != nil {
		return lease, s.renewErr
	}
	return lease, nil
}
func (s *durableRuntimeStore) BindProvider(_ context.Context, _ state.Lease, runID int64, runAttempt int, reason string) (state.Invocation, error) {
	provider := state.Provider{RunID: runID, RunAttempt: runAttempt, Reason: reason}
	s.providers = append(s.providers, provider)
	s.invocation.Provider = &provider
	return s.invocation, nil
}

func (s *durableRuntimeStore) Complete(ctx context.Context, _ state.Lease, succeeded bool, _ ...state.Result) (state.Invocation, error) {
	s.completes++
	s.completeCtxErr = ctx.Err()
	_, s.completeHasDeadline = ctx.Deadline()
	if succeeded {
		s.invocation.Status = state.StatusSucceeded
	} else if s.exhaustOnFailure {
		s.invocation.Status = state.StatusExhausted
	} else {
		s.invocation.Status = state.StatusFailed
	}
	return s.invocation, nil
}

func TestInvokeDurableRetriesWithLogicalMetadata(t *testing.T) {
	t.Setenv("GHAAS_INVOCATION_ID", "input-id")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_RUN_ATTEMPT", "3")
	retry := &manifest.RetryConfig{MaxAttempts: 2}
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"retry": {Runtime: manifest.RuntimeCommand, Retry: retry, Command: []string{"sh", "-c", `if [ "$GHAAS_ATTEMPT" = "1" ]; then printf 'first%s' "$GHAAS_WORKFLOW_RUN_ATTEMPT"; exit 9; fi; printf 'second%s' "$GHAAS_WORKFLOW_RUN_ATTEMPT"`}},
	}}
	var out bytes.Buffer
	store := &durableRuntimeStore{}
	code, err := Invoke(context.Background(), "retry", Options{
		LoadManifest: runtimeLoader(m), Store: store, Owner: "test",
		Environment: []string{"GHAAS_INVOCATION_ID=input-id", "GITHUB_EVENT_NAME=workflow_dispatch", "GITHUB_RUN_ID=42", "GITHUB_RUN_ATTEMPT=3"},
		Stdout:      &out, Stderr: &out,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil || code != 0 {
		t.Fatalf("durable Invoke = %d, %v", code, err)
	}
	if got, want := out.String(), "first3second3"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if store.acquires != 2 || store.completes != 2 {
		t.Fatalf("transitions acquire=%d complete=%d, want 2/2", store.acquires, store.completes)
	}
	if len(store.providers) != 1 || store.providers[0].RunID != 42 || store.providers[0].RunAttempt != 3 || store.providers[0].Reason != "workflow_dispatch" {
		t.Fatalf("provider attachment = %#v, want run 42 attempt 3 workflow_dispatch", store.providers)
	}
	if store.function != "retry" || store.id != "retry/input-id" || store.max != 2 {
		t.Fatalf("Ensure = %s/%s max=%d, want retry/input-id max=2", store.function, store.id, store.max)
	}
}
func TestInvokeCompletesAfterTransientRenewalError(t *testing.T) {
	m := manifest.Manifest{Version: 1, Defaults: manifest.Defaults{Timeout: manifest.Duration(time.Second)}, Functions: map[string]manifest.Function{
		"success": {Runtime: manifest.RuntimeCommand, Command: []string{"sh", "-c", "sleep 0.3"}},
	}}
	store := &durableRuntimeStore{leaseTTL: 400 * time.Millisecond, renewErr: errors.New("temporary renewal failure")}
	code, err := Invoke(context.Background(), "success", Options{
		LoadManifest: runtimeLoader(m), Store: store, Owner: "test",
		Environment: []string{"GHAAS_INVOCATION_ID=renewal", "GITHUB_EVENT_NAME=workflow_dispatch"},
		Stdout:      &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if err != nil || code != 0 {
		t.Fatalf("Invoke = %d, %v", code, err)
	}
	if store.completes != 1 || store.invocation.Status != state.StatusSucceeded {
		t.Fatalf("completion = %d status=%q, want one succeeded completion", store.completes, store.invocation.Status)
	}
}

func TestInvokeCanceledParentStillFencesCompletion(t *testing.T) {
	m := manifest.Manifest{Version: 1, Defaults: manifest.Defaults{Timeout: manifest.Duration(time.Second)}, Functions: map[string]manifest.Function{
		"cancel": {Runtime: manifest.RuntimeCommand, Command: []string{"sleep", "1"}},
	}}
	store := &durableRuntimeStore{exhaustOnFailure: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	code, err := Invoke(ctx, "cancel", Options{
		LoadManifest: runtimeLoader(m), Store: store, Owner: "test",
		Environment: []string{"GHAAS_INVOCATION_ID=canceled", "GITHUB_EVENT_NAME=workflow_dispatch"},
		Stdout:      &bytes.Buffer{}, Stderr: &bytes.Buffer{},
	})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Invoke = %d, %v; want cancellation", code, err)
	}
	if store.completes != 1 || store.invocation.Status != state.StatusExhausted {
		t.Fatalf("completion = %d status=%q, want one terminal completion", store.completes, store.invocation.Status)
	}
	if store.completeCtxErr != nil || !store.completeHasDeadline {
		t.Fatalf("completion context err=%v deadline=%v, want bounded non-canceled context", store.completeCtxErr, store.completeHasDeadline)
	}
}

func TestInvokeDurableScheduleUsesRunIDIdentity(t *testing.T) {
	m := manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"scheduled": {Runtime: manifest.RuntimeCommand, Command: []string{"sh", "-c", "printf '%s' \"$GHAAS_INVOCATION_ID\""}},
	}}
	var out bytes.Buffer
	store := &durableRuntimeStore{}
	code, err := Invoke(context.Background(), "scheduled", Options{
		LoadManifest: runtimeLoader(m), Store: store,
		Environment: []string{"GITHUB_EVENT_NAME=schedule", "GITHUB_RUN_ID=99"},
		Stdout:      &out, Stderr: &out,
	})
	if err != nil || code != 0 {
		t.Fatalf("scheduled Invoke = %d, %v", code, err)
	}
	if store.id != "scheduled/99" || out.String() != "scheduled/99" {
		t.Fatalf("schedule identity = %q output=%q, want scheduled/99", store.id, out.String())
	}
}
