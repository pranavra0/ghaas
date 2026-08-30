// Package manifest defines the ghaas v0.1 manifest format and strict YAML parser.
package manifest

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is the top-level ghaas configuration.
type Manifest struct {
	Version   int                 `yaml:"version"`
	Defaults  Defaults            `yaml:"defaults,omitempty"`
	Functions map[string]Function `yaml:"functions"`
}

// Defaults contains values inherited by functions.
type Defaults struct {
	Timeout Duration `yaml:"timeout,omitempty"`
}

// Function describes one command-backed function.
type Function struct {
	Runtime     RuntimeConfig     `yaml:"runtime"`
	Command     []string          `yaml:"command"`
	Schedule    *ScheduleConfig   `yaml:"schedule,omitempty"`
	Timeout     Duration          `yaml:"timeout,omitempty"`
	Environment map[string]string `yaml:"env,omitempty"`
	Secrets     []string          `yaml:"secrets,omitempty"`
	Concurrency ConcurrencyConfig `yaml:"concurrency,omitempty"`
	Retry       *RetryConfig      `yaml:"retry,omitempty"`
}

// ContainsGitHubExpression reports whether a literal value includes GitHub
// Actions expression markers. Manifest environment values are deliberately
// literal; expressions are emitted only for declared secret names and
// workflow-owned metadata.
func ContainsGitHubExpression(value string) bool {
	return strings.Contains(value, "${{") || strings.Contains(value, "}}")
}

// IsReservedEnvironmentKey reports whether a manifest environment name is
// owned by ghaas or by the GitHub Actions control plane.
func IsReservedEnvironmentKey(name string) bool {
	return strings.HasPrefix(name, "GHAAS_") || name == "GITHUB_TOKEN"
}

// RetryConfig controls durable retries for a function. An omitted
// max_attempts defaults to one attempt; an omitted backoff defaults to zero.
type RetryConfig struct {
	MaxAttempts int      `yaml:"max_attempts,omitempty"`
	Backoff     Duration `yaml:"backoff,omitempty"`
}

// EffectiveMaxAttempts returns the configured retry limit, defaulting to one.
// The defaults argument is accepted for symmetry with other effective
// function settings and is reserved for future manifest-level retry defaults.
func (f Function) EffectiveMaxAttempts(_ Defaults) int {
	if f.Retry == nil || f.Retry.MaxAttempts <= 0 {
		return 1
	}
	return f.Retry.MaxAttempts
}

// EffectiveBackoff returns the configured retry delay, defaulting to zero.
// The defaults argument is accepted for symmetry with other effective
// function settings and is reserved for future manifest-level retry defaults.
func (f Function) EffectiveBackoff(_ Defaults) Duration {
	if f.Retry == nil {
		return 0
	}
	return f.Retry.Backoff
}

// RuntimeConfig is the execution runtime. v0.1 supports command.
type RuntimeConfig string

const RuntimeCommand RuntimeConfig = "command"

// UnmarshalYAML ensures runtime is represented by a scalar string, rather than
// accepting mappings or sequences that happen to decode to a string.
func (r *RuntimeConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("runtime must be a scalar string")
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return err
	}
	*r = RuntimeConfig(value)
	return nil
}

// ScheduleConfig configures a scheduled trigger. Cron uses the standard five
// field cron form. Timezone is an IANA timezone name.
type ScheduleConfig struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
}

// ConcurrencyConfig configures the maximum number of in-flight executions.
// v0.1 supports max 1 (or an omitted zero value).
type ConcurrencyConfig struct {
	Max int `yaml:"max,omitempty"`
}

// Duration is a time.Duration encoded as a YAML duration string (for example,
// "15m" or "2h30m").
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a positive duration string using time.ParseDuration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a string")
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("invalid duration %q: duration must be positive", value)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML emits the canonical duration representation.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// EffectiveTimeout returns the function timeout, inheriting defaults when the
// function does not specify one. A zero result means no timeout was configured.
func (f Function) EffectiveTimeout(defaults Defaults) Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return defaults.Timeout
}

// Parse decodes one strict YAML manifest from data. Unknown fields and extra
// YAML documents are rejected. Semantic validation is provided by internal/config.
func Parse(data []byte) (Manifest, error) {
	return Decode(bytes.NewReader(data))
}

// Decode decodes one strict YAML manifest from r. Unknown fields and extra
// YAML documents are rejected.
func Decode(r io.Reader) (Manifest, error) {
	var m Manifest
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Manifest{}, fmt.Errorf("decode manifest: multiple YAML documents are not supported")
		}
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	return m, nil
}
