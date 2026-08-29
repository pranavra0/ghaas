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

// Defaults contains values inherited by functions. A zero timeout means that
// the loader/compiler default is used.
type Defaults struct {
	Timeout Duration `yaml:"timeout,omitempty"`
}

// Function describes one command-backed function.
type Function struct {
	Runtime     RuntimeConfig      `yaml:"runtime"`
	Command     []string           `yaml:"command"`
	Schedule    *ScheduleConfig    `yaml:"schedule,omitempty"`
	Timeout     Duration           `yaml:"timeout,omitempty"`
	Environment map[string]string  `yaml:"env,omitempty"`
	Secrets     []string           `yaml:"secrets,omitempty"`
	Concurrency ConcurrencyConfig  `yaml:"concurrency,omitempty"`
	State       *StateConfig       `yaml:"state,omitempty"`
	Retry       *RetryConfig       `yaml:"retry,omitempty"`
	OnExhausted *OnExhaustedConfig `yaml:"on_exhausted,omitempty"`
	SLO         *SLOConfig         `yaml:"slo,omitempty"`
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

// ScheduleConfig configures a scheduled trigger. Cron uses the standard five
// field cron form. Timezone is an IANA timezone name.
type ScheduleConfig struct {
	Cron            string               `yaml:"cron"`
	Timezone        string               `yaml:"timezone"`
	Target          string               `yaml:"target,omitempty"`
	Retry           *ScheduleRetryConfig `yaml:"retry,omitempty"`
	ExecutionWindow *ExecutionWindow     `yaml:"execution_window,omitempty"`
	// Window is accepted as the concise spelling of execution_window. It is
	// retained as a separate field for source compatibility with early clients;
	// validation rejects configurations that specify both spellings.
	Window *ExecutionWindow `yaml:"window,omitempty"`
	// WindowStart and WindowEnd are compatibility spellings for manifests that
	// flatten the execution window under schedule. They must be provided as a
	// pair and are validated in the same way as execution_window.
	WindowStart string `yaml:"window_start,omitempty"`
	WindowEnd   string `yaml:"window_end,omitempty"`
}

// ScheduleRetryConfig describes a v0.2 scheduling retry window. It is
// intentionally distinct from RetryConfig: this controls runner opportunities
// before execution, not retries after a command failure.
type ScheduleRetryConfig struct {
	Every Duration `yaml:"every"`
	Until string   `yaml:"until"`
}

// EffectiveExecutionWindow returns the configured execution window, accepting
// either the nested execution_window/window spelling or the flattened
// window_start/window_end spelling. If multiple spellings are present,
// semantic validation reports the configuration error.
func (s ScheduleConfig) EffectiveExecutionWindow() *ExecutionWindow {
	if s.ExecutionWindow != nil {
		return s.ExecutionWindow
	}
	if s.Window != nil {
		return s.Window
	}
	if s.WindowStart != "" || s.WindowEnd != "" {
		return &ExecutionWindow{Start: s.WindowStart, End: s.WindowEnd}
	}
	return nil
}

// StateBackend is the backend identifier. It is an alias for string so callers
// using the original string-based StateConfig API remain source-compatible.
type StateBackend = string

// State backends supported by the manifest model.
const (
	StateBackendMemory StateBackend = "memory"
	StateBackendBranch StateBackend = "branch"
	DefaultStateBranch StateBackend = "ghaas-state"
)

// EffectiveBranch returns the configured branch/ref for a branch backend. The
// default is the conventional ghaas-state branch.
func (s StateConfig) EffectiveBranch() string {
	if s.Branch != "" {
		return s.Branch
	}
	if s.Ref != "" {
		return s.Ref
	}
	return DefaultStateBranch
}

// ExecutionWindow limits when a scheduled logical invocation may be retried.
// Start and End are local wall-clock times in the schedule timezone, formatted
// as HH:MM. An end earlier than start denotes an invalid (rather than wrapping)
// window; use an explicit schedule on the following day instead.
type ExecutionWindow struct {
	Start string `yaml:"start"`
	End   string `yaml:"end"`
}

// ConcurrencyConfig configures the maximum number of in-flight executions.
type ConcurrencyConfig struct {
	Max int `yaml:"max,omitempty"`
}

// StateConfig selects the invocation state backend.
type StateConfig struct {
	Backend string `yaml:"backend"`
	// Branch names the branch used by the branch backend. Ref is an accepted
	// explicit ref spelling for callers that use Git terminology; validation
	// rejects configurations that provide both values.
	Branch string `yaml:"branch,omitempty"`
	Ref    string `yaml:"ref,omitempty"`
}

// RetryConfig controls execution retries. MaxAttempts includes the initial
// execution attempt. Backoff is the delay between attempts.
type RetryConfig struct {
	MaxAttempts int      `yaml:"max_attempts"`
	Backoff     Duration `yaml:"backoff,omitempty"`
}

// OnExhaustedConfig controls optional dead-letter handling after retries are
// exhausted.
type OnExhaustedConfig struct {
	Issue bool `yaml:"issue,omitempty"`
}

// SLOConfig declares optional reliability targets. SuccessRate is a percentage
// in the inclusive range (0, 100]. ScheduleDelay is the maximum allowed delay
// for a scheduled invocation to begin.
type SLOConfig struct {
	SuccessRate   float64  `yaml:"success_rate,omitempty"`
	ScheduleDelay Duration `yaml:"schedule_delay,omitempty"`

	successRateSet   bool
	scheduleDelaySet bool
}

// UnmarshalYAML tracks field presence so an explicitly configured zero target
// is rejected while omitted optional targets remain distinguishable. The
// explicit mapping also keeps strict unknown-field behavior inside this custom
// unmarshaler.
func (s *SLOConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("slo must be a mapping")
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return fmt.Errorf("slo field name must be a string")
		}
		name := key.Value
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("slo field %q is duplicated", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "success_rate":
			var rate float64
			if err := value.Decode(&rate); err != nil {
				return fmt.Errorf("slo.success_rate: %w", err)
			}
			s.SuccessRate = rate
			s.successRateSet = true
		case "schedule_delay":
			var delay Duration
			if err := value.Decode(&delay); err != nil {
				return fmt.Errorf("slo.schedule_delay: %w", err)
			}
			s.ScheduleDelay = delay
			s.scheduleDelaySet = true
		default:
			return fmt.Errorf("slo contains unknown field %q", name)
		}
	}
	return nil
}

// HasSuccessRate reports whether a success-rate target was configured. It
// also treats non-zero programmatic values as configured for compatibility
// with callers constructing SLOConfig literals.
func (s SLOConfig) HasSuccessRate() bool {
	return s.successRateSet || s.SuccessRate != 0
}

// HasScheduleDelay reports whether a schedule-delay target was configured.
func (s SLOConfig) HasScheduleDelay() bool {
	return s.scheduleDelaySet || s.ScheduleDelay != 0
}

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

// EffectiveTimeout returns the function timeout, inheriting defaults when the
// function does not specify one. A zero result means that no manifest timeout
// was configured and the caller should apply its own runtime default.
func (f Function) EffectiveTimeout(defaults Defaults) Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return defaults.Timeout
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
