// Package manifest defines the ghaas manifest format and strict YAML parser.
package manifest

import (
	"bytes"
	"fmt"
	"io"
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
	State       *StateConfig      `yaml:"state,omitempty"`
	Retry       *RetryConfig      `yaml:"retry,omitempty"`
}

// RuntimeConfig is the name of the execution runtime. v0.1 supports command.
type RuntimeConfig string

// Runtime is a concise alias for RuntimeConfig.
type Runtime = RuntimeConfig

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

// ScheduleConfig configures a scheduled trigger.
type ScheduleConfig struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
}

// ConcurrencyConfig configures the maximum number of in-flight executions.
type ConcurrencyConfig struct {
	Max int `yaml:"max,omitempty"`
}

// StateConfig and RetryConfig are reserved for a later manifest version. They
// remain represented so strict decoding can produce useful errors for unknown
// fields; semantic validation rejects their use in v0.1.
type StateConfig struct{}
type RetryConfig struct{}

// Duration is a time.Duration encoded as a YAML duration string (for example,
// "15m" or "2h30m").
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML parses a duration string using time.ParseDuration.
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
	if parsed == 0 {
		return fmt.Errorf("invalid duration %q: duration must be positive", value)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML emits the canonical duration representation.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Parse decodes one strict YAML manifest from data. Unknown fields and extra
// YAML documents are rejected.
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
