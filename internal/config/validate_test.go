package config

import (
	"ghaas/pkg/manifest"
	"strings"
	"testing"
	"time"
)

func validManifest() manifest.Manifest {
	return manifest.Manifest{
		Version: 1,
		Functions: map[string]manifest.Function{
			"hello": {
				Runtime: manifest.RuntimeCommand,
				Command: []string{"echo", "hello"},
			},
		},
	}
}

func TestValidateAcceptsValidManifest(t *testing.T) {
	m := validManifest()
	m.Functions["hello"] = manifest.Function{
		Runtime:     manifest.RuntimeCommand,
		Command:     []string{"echo", "hello"},
		Schedule:    &manifest.ScheduleConfig{Cron: "15 9 * * 5", Timezone: "America/New_York"},
		Environment: map[string]string{"MESSAGE": "hello"},
		Secrets:     []string{"TOKEN"},
		Concurrency: manifest.ConcurrencyConfig{Max: 1},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateReportsDeterministicContextualErrors(t *testing.T) {
	m := manifest.Manifest{
		Version: 2,
		Functions: map[string]manifest.Function{
			"z bad": {
				Runtime: "python", Command: nil,
				Timeout:     manifest.Duration(-1),
				Concurrency: manifest.ConcurrencyConfig{Max: 2},
			},
			"a": {
				Runtime: manifest.RuntimeCommand, Command: []string{""},
				Schedule:    &manifest.ScheduleConfig{Cron: "* * * *", Timezone: "No/Such_Zone"},
				Environment: map[string]string{"GHAAS_FUNCTION": "x", "bad-name?": "x"},
				Secrets:     []string{"TOKEN", "TOKEN", "bad-name?"},
			},
		},
	}
	err := Validate(m)
	if err == nil {
		t.Fatal("Validate() succeeded, want errors")
	}
	message := err.Error()
	for _, fragment := range []string{`function "a"`, `function "z bad"`, "schedule.cron", "schedule.timezone", "GHAAS_FUNCTION", "duplicated", "collides"} {
		if !strings.Contains(message, fragment) {
			t.Errorf("error %q does not contain %q", message, fragment)
		}
	}
	if strings.Index(message, `function "a"`) > strings.Index(message, `function "z bad"`) {
		t.Fatalf("errors are not sorted by function name: %q", message)
	}
}

func TestValidateRejectsUnsupportedStateAndRetry(t *testing.T) {
	m := validManifest()
	m.Functions["hello"] = manifest.Function{
		Runtime: manifest.RuntimeCommand,
		Command: []string{"echo"},
		State:   &manifest.StateConfig{},
		Retry:   &manifest.RetryConfig{},
	}
	if err := Validate(m); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Validate() error = %v, want unsupported state/retry", err)
	}
}
func TestValidateAcceptsStateRetryWindowAndSLO(t *testing.T) {
	m := validManifest()
	m.Functions["hello"] = manifest.Function{
		Runtime: manifest.RuntimeCommand,
		Command: []string{"echo", "hello"},
		Schedule: &manifest.ScheduleConfig{
			Cron:     "15 9 * * 5",
			Timezone: "America/New_York",
			ExecutionWindow: &manifest.ExecutionWindow{
				Start: "09:15",
				End:   "19:00",
			},
		},
		State:       &manifest.StateConfig{Backend: manifest.StateBackendBranch},
		Retry:       &manifest.RetryConfig{MaxAttempts: 4, Backoff: manifest.Duration(5 * time.Minute)},
		OnExhausted: &manifest.OnExhaustedConfig{Issue: true},
		SLO:         &manifest.SLOConfig{SuccessRate: 99.9, ScheduleDelay: manifest.Duration(15 * time.Minute)},
	}
	if err := Validate(m); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsInvalidReliabilityConfig(t *testing.T) {
	m := validManifest()
	m.Functions["hello"] = manifest.Function{
		Runtime: manifest.RuntimeCommand,
		Command: []string{"echo"},
		Schedule: &manifest.ScheduleConfig{
			Cron:     "* * * * *",
			Timezone: "UTC",
			ExecutionWindow: &manifest.ExecutionWindow{
				Start: "19:00",
				End:   "09:15",
			},
		},
		State: &manifest.StateConfig{Backend: "sqlite"},
		Retry: &manifest.RetryConfig{MaxAttempts: 0},
		SLO:   &manifest.SLOConfig{SuccessRate: 101},
	}
	err := Validate(m)
	if err == nil {
		t.Fatal("Validate() succeeded, want semantic errors")
	}
	for _, fragment := range []string{"execution_window.end", "unsupported state backend", "max_attempts", "success_rate"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not contain %q", err, fragment)
		}
	}
}
