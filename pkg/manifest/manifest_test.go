package manifest

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeStrictManifest(t *testing.T) {
	data := []byte(`version: 1
defaults:
  timeout: 15m
functions:
  hello:
    runtime: command
    command: [echo, hello]
    timeout: 2.5s
    schedule:
      cron: "*/5 * * * *"
      timezone: UTC
    env:
      MESSAGE: hello
    secrets: [TOKEN]
    concurrency:
      max: 1
`)
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.Version != 1 || time.Duration(got.Defaults.Timeout) != 15*time.Minute {
		t.Fatalf("unexpected top-level values: %#v", got)
	}
	fn := got.Functions["hello"]
	if fn.Runtime != RuntimeCommand || len(fn.Command) != 2 || time.Duration(fn.Timeout) != 2500*time.Millisecond {
		t.Fatalf("unexpected function values: %#v", fn)
	}
	if fn.Environment["MESSAGE"] != "hello" || len(fn.Secrets) != 1 || fn.Concurrency.Max != 1 {
		t.Fatalf("unexpected environment values: %#v", fn)
	}
}

func TestDecodeRejectsUnknownAndNonScalarRuntime(t *testing.T) {
	cases := []string{
		`version: 1
functions:
  hello:
    runtime: command
    command: [echo]
    state: {backend: memory}
`,
		`version: 1
functions:
  hello:
    runtime: command
    command: [echo]
    retry: {max_attempts: 2}
`,
		`version: 1
functions:
  hello:
    runtime: [command]
    command: [echo]
`,
	}
	for _, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", data)
		}
	}
}

func TestDecodeRejectsInvalidDurationAndMultipleDocuments(t *testing.T) {
	invalidDuration := `version: 1
functions:
  hello:
    runtime: command
    command: [echo]
    timeout: definitely-not-a-duration
`
	if _, err := Parse([]byte(strings.TrimSpace(invalidDuration))); err == nil {
		t.Fatal("Parse() succeeded, want invalid duration error")
	}
	multiple := "version: 1\nfunctions: {hello: {runtime: command, command: [echo]}}\n---\nversion: 1\nfunctions: {}\n"
	if _, err := Parse([]byte(multiple)); err == nil {
		t.Fatal("Parse() succeeded, want multiple-document error")
	}
}

func TestEffectiveTimeout(t *testing.T) {
	defaults := Defaults{Timeout: Duration(15 * time.Minute)}
	if got := (Function{}).EffectiveTimeout(defaults); got != defaults.Timeout {
		t.Fatalf("inherited timeout = %v, want %v", got, defaults.Timeout)
	}
	explicit := Function{Timeout: Duration(2 * time.Minute)}
	if got := explicit.EffectiveTimeout(defaults); got != explicit.Timeout {
		t.Fatalf("explicit timeout = %v, want %v", got, explicit.Timeout)
	}
}
