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
		Retry:       &manifest.RetryConfig{MaxAttempts: 3, Backoff: manifest.Duration(5 * time.Second)},
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

func TestValidateRejectsGitHubExpressionsInLiteralEnvironment(t *testing.T) {
	for _, value := range []string{
		"${{ secrets.NOT_DECLARED }}",
		"prefix ${{ github.token }} suffix",
		"malformed ${{",
		"malformed }}",
	} {
		m := validManifest()
		m.Functions["hello"] = manifest.Function{
			Runtime:     manifest.RuntimeCommand,
			Command:     []string{"echo"},
			Environment: map[string]string{"VALUE": value},
		}
		err := Validate(m)
		if err == nil || !strings.Contains(err.Error(), "GitHub expression syntax") {
			t.Fatalf("Validate accepted literal env expression %q: %v", value, err)
		}
	}
}

func TestValidateRejectsGitHubTokenShadowing(t *testing.T) {
	m := validManifest()
	m.Functions["hello"] = manifest.Function{
		Runtime:     manifest.RuntimeCommand,
		Command:     []string{"echo"},
		Environment: map[string]string{"GITHUB_TOKEN": "shadow"},
		Secrets:     []string{"GITHUB_TOKEN"},
	}
	err := Validate(m)
	if err == nil {
		t.Fatal("Validate() succeeded, want control-plane credential errors")
	}
	message := err.Error()
	if !strings.Contains(message, `env key "GITHUB_TOKEN" is reserved`) {
		t.Errorf("missing env reservation error: %q", message)
	}
	if !strings.Contains(message, `secret name "GITHUB_TOKEN" is reserved`) {
		t.Errorf("missing secret reservation error: %q", message)
	}
}

func TestValidateRejectsFutureFieldsAtDecode(t *testing.T) {
	for _, field := range []string{"state", "target", "execution_window", "on_exhausted", "slo"} {
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
		Retry:       &manifest.RetryConfig{MaxAttempts: -1, Backoff: manifest.Duration(-time.Second)},
	}
	err := Validate(m)
	if err == nil {
		t.Fatal("Validate() succeeded, want semantic errors")
	}
	for _, fragment := range []string{"defaults.timeout", "timeout", "schedule.cron", "schedule.timezone", "concurrency.max", "retry.max_attempts", "retry.backoff"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not contain %q", err, fragment)
		}
	}
}
