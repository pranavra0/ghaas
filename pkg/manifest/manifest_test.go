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
}

func TestDecodeRejectsUnknownAndNonScalarRuntime(t *testing.T) {
	cases := []string{
		`version: 1
functions:
  hello:
    runtime: command
    command: [echo]
    extra: true
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

func TestDecodeRejectsInvalidDuration(t *testing.T) {
	data := `version: 1
functions:
  hello:
    runtime: command
    command: [echo]
    timeout: definitely-not-a-duration
`
	_, err := Parse([]byte(strings.TrimSpace(data)))
	if err == nil {
		t.Fatal("Parse() succeeded, want invalid duration error")
	}
}
