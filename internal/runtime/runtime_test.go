package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pranavra0/ghaas/internal/invocation"
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
	for _, expected := range []string{"CUSTOM=manifest", "GHAAS_FUNCTION=env", "GHAAS_INVOCATION_ID=env/input-id", "GHAAS_TRIGGER=manual"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("environment missing %q: %s", expected, text)
		}
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
