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
func TestDecodeCompleteManifestModel(t *testing.T) {
	data := []byte(`version: 1
defaults:
  timeout: 15m
functions:
  worker:
    runtime: command
    command: [echo, ok]
    schedule:
      cron: "15 9 * * 5"
      timezone: America/New_York
      execution_window:
        start: "09:15"
        end: "19:00"
    retry:
      max_attempts: 3
      backoff: 5m
    state:
      backend: branch
      branch: ghaas-state
    on_exhausted:
      issue: true
    slo:
      success_rate: 99.9
      schedule_delay: 15m
`)
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	fn := got.Functions["worker"]
	if fn.Schedule == nil || fn.Schedule.ExecutionWindow == nil {
		t.Fatalf("execution window was not decoded: %#v", fn.Schedule)
	}
	if fn.Schedule.ExecutionWindow.Start != "09:15" || fn.Schedule.ExecutionWindow.End != "19:00" {
		t.Fatalf("unexpected execution window: %#v", fn.Schedule.ExecutionWindow)
	}
	if fn.Retry == nil || fn.Retry.MaxAttempts != 3 || time.Duration(fn.Retry.Backoff) != 5*time.Minute {
		t.Fatalf("unexpected retry: %#v", fn.Retry)
	}
	if fn.State == nil || fn.State.Backend != StateBackendBranch || fn.State.EffectiveBranch() != "ghaas-state" {
		t.Fatalf("unexpected state: %#v", fn.State)
	}
	if fn.OnExhausted == nil || !fn.OnExhausted.Issue || fn.SLO == nil || fn.SLO.SuccessRate != 99.9 {
		t.Fatalf("unexpected reliability config: exhausted=%#v slo=%#v", fn.OnExhausted, fn.SLO)
	}
	if got.Functions["worker"].EffectiveTimeout(got.Defaults) != got.Defaults.Timeout {
		t.Fatalf("default timeout was not inherited")
	}
}

func TestDecodeTargetScheduleRetry(t *testing.T) {
	data := []byte(`version: 1
functions:
  report:
    runtime: command
    command: [echo, ok]
    schedule:
      target: "09:15 Friday"
      timezone: UTC
      retry:
        every: 5m
        until: "19:00"
`)
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	schedule := got.Functions["report"].Schedule
	if schedule == nil || schedule.Target != "09:15 Friday" || schedule.Retry == nil {
		t.Fatalf("target schedule was not decoded: %#v", schedule)
	}
	if time.Duration(schedule.Retry.Every) != 5*time.Minute || schedule.Retry.Until != "19:00" {
		t.Fatalf("unexpected schedule retry: %#v", schedule.Retry)
	}
}
