package config

import (
	"github.com/pranavra0/ghaas/pkg/manifest"
	"strings"
	"testing"
	"time"
)

func validManifest() manifest.Manifest {
	return manifest.Manifest{
		Version:  1,
		Defaults: manifest.Defaults{Timeout: manifest.Duration(15 * time.Minute)},
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

func TestValidateRejectsReservedMetadataAndSecretsCollision(t *testing.T) {
	m := validManifest()
	m.Functions["hello"] = manifest.Function{
		Runtime: manifest.RuntimeCommand,
		Command: []string{"echo"},
		Environment: map[string]string{
			"GHAAS_FUNCTION":          "shadow",
			"GHAAS_SCHEDULE_TIMEZONE": "shadow",
		},
		Secrets: []string{"TOKEN", "TOKEN", "VALUE"},
	}
	m.Functions["hello"].Environment["VALUE"] = "ordinary"
	err := Validate(m)
	if err == nil {
		t.Fatal("Validate() succeeded, want reserved/collision errors")
	}
	for _, fragment := range []string{"GHAAS_FUNCTION", "GHAAS_SCHEDULE_TIMEZONE", "duplicated", "collides"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not contain %q", err, fragment)
		}
	}
}

func TestValidateRejectsFutureFieldsAtDecode(t *testing.T) {
	for _, field := range []string{"state", "retry", "target", "execution_window", "on_exhausted", "slo"} {
		data := "version: 1\nfunctions:\n  hello:\n    runtime: command\n    command: [echo]\n    " + field + ": {}\n"
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("Parse accepted future field %q", field)
		}
	}
}

func TestValidateRejectsInvalidTimeoutScheduleAndConcurrency(t *testing.T) {
	m := validManifest()
	m.Defaults.Timeout = manifest.Duration(-time.Minute)
	m.Functions["hello"] = manifest.Function{
		Runtime:     manifest.RuntimeCommand,
		Command:     []string{"echo"},
		Timeout:     manifest.Duration(-time.Second),
		Schedule:    &manifest.ScheduleConfig{Cron: "* * * *", Timezone: "No/Such_Zone"},
		Concurrency: manifest.ConcurrencyConfig{Max: 2},
	}
	err := Validate(m)
	if err == nil {
		t.Fatal("Validate() succeeded, want semantic errors")
	}
	for _, fragment := range []string{"defaults.timeout", "timeout", "schedule.cron", "schedule.timezone", "concurrency.max"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not contain %q", err, fragment)
		}
	}
}
