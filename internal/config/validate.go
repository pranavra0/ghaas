package config

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"ghaas/pkg/manifest"
)

var (
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	envPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	clockPattern = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`)
	cronAtom     = regexp.MustCompile(`^(?:\*|[0-9]+|[A-Za-z]+)(?:-(?:\*|[0-9]+|[A-Za-z]+))?$`)
)

var reservedEnvironmentKeys = map[string]struct{}{
	"GHAAS_FUNCTION":        {},
	"GHAAS_INVOCATION_ID":   {},
	"GHAAS_ATTEMPT":         {},
	"GHAAS_TRIGGER":         {},
	"GHAAS_WORKFLOW_RUN_ID": {},
}

// ValidationError contains all semantic errors in stable order. Problems are
// collected rather than returned at the first failure so a user can fix a
// manifest in one pass.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 0 {
		return "invalid manifest"
	}
	return "invalid manifest: " + strings.Join(e.Problems, "; ")
}

// Validate applies the manifest's semantic constraints. It does not access
// GitHub or inspect the host environment beyond validating IANA timezone names.
func Validate(m manifest.Manifest) error {
	var problems []string
	if m.Version != 1 {
		problems = append(problems, fmt.Sprintf("version must be 1 (got %d)", m.Version))
	}
	if m.Defaults.Timeout != 0 && time.Duration(m.Defaults.Timeout) <= 0 {
		problems = append(problems, "defaults.timeout must be positive")
	}
	if len(m.Functions) == 0 {
		problems = append(problems, "functions must contain at least one function")
	}

	functionNames := make([]string, 0, len(m.Functions))
	for name := range m.Functions {
		functionNames = append(functionNames, name)
	}
	sort.Strings(functionNames)
	for _, name := range functionNames {
		fn := m.Functions[name]
		prefix := fmt.Sprintf("function %q", name)
		if !namePattern.MatchString(name) {
			problems = append(problems, prefix+": name must contain only letters, digits, '_' or '-' and start with a letter or digit")
		}
		problems = append(problems, validateFunction(prefix, fn)...)
		timeout := fn.Timeout
		if timeout == 0 {
			timeout = m.Defaults.Timeout
		}
		if timeout > manifest.Duration(6*time.Hour) {
			problems = append(problems, prefix+": timeout cannot exceed GitHub Actions' 360-minute job limit")
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

func validateFunction(prefix string, fn manifest.Function) []string {
	var problems []string
	if fn.Runtime != manifest.RuntimeCommand {
		if fn.Runtime == "" {
			problems = append(problems, prefix+": runtime is required")
		} else {
			problems = append(problems, fmt.Sprintf("%s: unsupported runtime %q (only %q is supported)", prefix, fn.Runtime, manifest.RuntimeCommand))
		}
	}
	if len(fn.Command) == 0 {
		problems = append(problems, prefix+": command must contain at least one argument")
	} else {
		for i, arg := range fn.Command {
			if strings.TrimSpace(arg) == "" {
				problems = append(problems, fmt.Sprintf("%s: command argument %d must not be empty", prefix, i))
			}
			if strings.IndexByte(arg, 0) >= 0 {
				problems = append(problems, fmt.Sprintf("%s: command argument %d must not contain NUL", prefix, i))
			}
		}
	}
	if fn.Timeout != 0 && time.Duration(fn.Timeout) <= 0 {
		problems = append(problems, prefix+": timeout must be positive")
	}
	if fn.Schedule != nil {
		problems = append(problems, validateSchedule(prefix, *fn.Schedule)...)
	}
	if fn.Concurrency.Max < 0 {
		problems = append(problems, fmt.Sprintf("%s: concurrency.max must be positive (got %d)", prefix, fn.Concurrency.Max))
	} else if fn.Concurrency.Max != 0 && fn.Concurrency.Max != 1 {
		// GitHub Actions concurrency groups provide one active execution. Keep
		// the v0.1 contract explicit instead of silently ignoring larger values.
		problems = append(problems, fmt.Sprintf("%s: concurrency.max must be 1 (got %d)", prefix, fn.Concurrency.Max))
	}
	if fn.State != nil {
		problems = append(problems, validateState(prefix, *fn.State)...)
	}
	if fn.Retry != nil {
		problems = append(problems, validateRetry(prefix, *fn.Retry)...)
	}
	if fn.SLO != nil {
		problems = append(problems, validateSLO(prefix, *fn.SLO)...)
		if fn.Schedule == nil && fn.SLO.HasScheduleDelay() {
			problems = append(problems, prefix+": slo.schedule_delay requires a schedule")
		}
	}
	problems = append(problems, validateEnvironment(prefix, fn.Environment, fn.Secrets)...)
	return problems
}

func validateSchedule(prefix string, schedule manifest.ScheduleConfig) []string {
	var problems []string
	hasCron := strings.TrimSpace(schedule.Cron) != ""
	hasTarget := strings.TrimSpace(schedule.Target) != ""
	if !hasCron && !hasTarget {
		problems = append(problems, prefix+": schedule.cron or schedule.target is required")
	} else if hasCron && hasTarget {
		problems = append(problems, prefix+": schedule.cron and schedule.target are mutually exclusive")
	} else if hasCron {
		if err := validateCron(schedule.Cron); err != nil {
			problems = append(problems, prefix+": schedule.cron "+err.Error())
		}
	} else {
		clock := scheduleTargetClock(schedule.Target)
		if clock == "" || !clockPattern.MatchString(clock) {
			problems = append(problems, prefix+": schedule.target must include a valid HH:MM time")
		}
	}
	tz := strings.TrimSpace(schedule.Timezone)
	if tz == "" {
		problems = append(problems, prefix+": schedule.timezone is required")
	} else if tz == "Local" {
		problems = append(problems, fmt.Sprintf("%s: schedule.timezone %q is not a valid IANA timezone", prefix, schedule.Timezone))
	} else if _, err := time.LoadLocation(schedule.Timezone); err != nil {
		problems = append(problems, fmt.Sprintf("%s: schedule.timezone %q is not a valid IANA timezone", prefix, schedule.Timezone))
	}
	if schedule.Retry != nil {
		if schedule.Retry.Every <= 0 {
			problems = append(problems, prefix+": schedule.retry.every must be positive")
		}
		if schedule.Retry.Every > 0 && time.Duration(schedule.Retry.Every) < 5*time.Minute {
			problems = append(problems, prefix+": schedule.retry.every must be at least 5m for GitHub Actions scheduling")
		}
		if strings.TrimSpace(schedule.Retry.Until) == "" {
			problems = append(problems, prefix+": schedule.retry.until is required")
		} else if !clockPattern.MatchString(schedule.Retry.Until) {
			problems = append(problems, prefix+": schedule.retry.until must use HH:MM in the 24-hour clock")
		}
	}

	nestedWindows := 0
	if schedule.ExecutionWindow != nil {
		nestedWindows++
	}
	if schedule.Window != nil {
		nestedWindows++
	}
	flatWindow := schedule.WindowStart != "" || schedule.WindowEnd != ""
	if nestedWindows > 0 && flatWindow {
		problems = append(problems, prefix+": schedule execution window must use one spelling (execution_window, window, or window_start/window_end)")
	} else if nestedWindows > 1 {
		problems = append(problems, prefix+": schedule.execution_window and schedule.window must not both be specified")
	} else if flatWindow {
		problems = append(problems, validateExecutionWindow(prefix, "schedule.window_start/window_end", manifest.ExecutionWindow{Start: schedule.WindowStart, End: schedule.WindowEnd})...)
	} else if schedule.ExecutionWindow != nil {
		problems = append(problems, validateExecutionWindow(prefix, "schedule.execution_window", *schedule.ExecutionWindow)...)
	} else if schedule.Window != nil {
		problems = append(problems, validateExecutionWindow(prefix, "schedule.window", *schedule.Window)...)
	}
	return problems
}
func scheduleTargetClock(target string) string {
	for _, token := range strings.Fields(target) {
		if strings.Contains(token, ":") {
			return token
		}
	}
	return ""
}

func validateExecutionWindow(prefix, path string, window manifest.ExecutionWindow) []string {
	var problems []string
	if strings.TrimSpace(window.Start) == "" {
		problems = append(problems, fmt.Sprintf("%s: %s.start is required", prefix, path))
	} else if !clockPattern.MatchString(window.Start) {
		problems = append(problems, fmt.Sprintf("%s: %s.start %q must use HH:MM in the 24-hour clock", prefix, path, window.Start))
	}
	if strings.TrimSpace(window.End) == "" {
		problems = append(problems, fmt.Sprintf("%s: %s.end is required", prefix, path))
	} else if !clockPattern.MatchString(window.End) {
		problems = append(problems, fmt.Sprintf("%s: %s.end %q must use HH:MM in the 24-hour clock", prefix, path, window.End))
	}
	if clockPattern.MatchString(window.Start) && clockPattern.MatchString(window.End) {
		start := clockMinutes(window.Start)
		end := clockMinutes(window.End)
		if end <= start {
			problems = append(problems, fmt.Sprintf("%s: %s.end must be after start", prefix, path))
		}
	}
	return problems
}

func clockMinutes(value string) int {
	return int(value[0]-'0')*600 + int(value[1]-'0')*60 + int(value[3]-'0')*10 + int(value[4]-'0')
}

func validateState(prefix string, state manifest.StateConfig) []string {
	var problems []string
	backend := state.Backend
	if backend == "" {
		// Keep the wording useful to old callers that used an empty reserved
		// state block, while explaining the actual required field.
		return []string{prefix + ": state is not supported without a backend (state.backend is required)"}
	}
	if strings.TrimSpace(backend) != backend {
		problems = append(problems, fmt.Sprintf("%s: state.backend %q must not contain surrounding whitespace", prefix, backend))
	}
	if state.Branch != "" && state.Ref != "" {
		problems = append(problems, prefix+": state.branch and state.ref must not both be specified")
	}
	switch backend {
	case manifest.StateBackendMemory:
		if state.Branch != "" || state.Ref != "" {
			problems = append(problems, prefix+": state branch/ref is only valid with the branch backend")
		}
	case manifest.StateBackendBranch:
		branch := state.Branch
		if branch == "" {
			branch = state.Ref
		}
		if branch != "" {
			if err := validateGitRef(branch); err != nil {
				problems = append(problems, prefix+": state branch "+err.Error())
			}
		}
	default:
		problems = append(problems, fmt.Sprintf("%s: unsupported state backend %q (supported backends are %q and %q)", prefix, state.Backend, manifest.StateBackendMemory, manifest.StateBackendBranch))
	}
	return problems
}

func validateGitRef(value string) error {
	if strings.TrimSpace(value) != value || value == "" {
		return fmt.Errorf("must not be empty or contain surrounding whitespace")
	}
	if strings.Contains(value, "${{") || strings.Contains(value, "}}") {
		return fmt.Errorf("%q is not a literal Git ref", value)
	}
	if strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.Contains(value, "//") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return fmt.Errorf("%q is not a valid Git ref", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") ||
			strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") || part == "@" {
			return fmt.Errorf("%q is not a valid Git ref", value)
		}
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f || r == '~' || r == '^' || r == ':' || r == '\\' || r == '?' || r == '*' || r == '[' {
			return fmt.Errorf("%q is not a valid Git ref", value)
		}
	}
	return nil
}

func validateRetry(prefix string, retry manifest.RetryConfig) []string {
	var problems []string
	if retry.MaxAttempts <= 0 {
		problems = append(problems, prefix+": retry is not supported without a positive max_attempts")
	}
	if retry.Backoff != 0 && time.Duration(retry.Backoff) <= 0 {
		problems = append(problems, prefix+": retry.backoff must be positive")
	}
	return problems
}

func validateSLO(prefix string, slo manifest.SLOConfig) []string {
	var problems []string
	hasRate := slo.HasSuccessRate()
	hasDelay := slo.HasScheduleDelay()
	if hasRate && (math.IsNaN(slo.SuccessRate) || math.IsInf(slo.SuccessRate, 0) || slo.SuccessRate <= 0 || slo.SuccessRate > 100) {
		problems = append(problems, fmt.Sprintf("%s: slo.success_rate must be greater than 0 and at most 100 (got %v)", prefix, slo.SuccessRate))
	}
	if hasDelay && time.Duration(slo.ScheduleDelay) <= 0 {
		problems = append(problems, prefix+": slo.schedule_delay must be positive")
	}
	if !hasRate && !hasDelay {
		problems = append(problems, prefix+": slo must configure success_rate and/or schedule_delay")
	}
	return problems
}

func validateEnvironment(prefix string, env map[string]string, secrets []string) []string {
	var problems []string
	envNames := make([]string, 0, len(env))
	for name := range env {
		envNames = append(envNames, name)
	}
	sort.Strings(envNames)
	for _, name := range envNames {
		if !envPattern.MatchString(name) {
			problems = append(problems, fmt.Sprintf("%s: env key %q is not a valid environment identifier", prefix, name))
		}
		if _, reserved := reservedEnvironmentKeys[name]; reserved {
			problems = append(problems, fmt.Sprintf("%s: env key %q is reserved", prefix, name))
		}
		if strings.IndexByte(env[name], 0) >= 0 {
			problems = append(problems, fmt.Sprintf("%s: env value for %q must not contain NUL", prefix, name))
		}
	}

	secretNames := append([]string(nil), secrets...)
	sort.Strings(secretNames)
	seenSecrets := make(map[string]struct{}, len(secretNames))
	for _, name := range secretNames {
		if !envPattern.MatchString(name) {
			problems = append(problems, fmt.Sprintf("%s: secret name %q is not a valid environment identifier", prefix, name))
		}
		if _, reserved := reservedEnvironmentKeys[name]; reserved {
			problems = append(problems, fmt.Sprintf("%s: secret name %q is reserved", prefix, name))
		}
		if _, seen := seenSecrets[name]; seen {
			problems = append(problems, fmt.Sprintf("%s: secret name %q is duplicated", prefix, name))
		} else {
			seenSecrets[name] = struct{}{}
		}
		if _, inEnv := env[name]; inEnv {
			problems = append(problems, fmt.Sprintf("%s: secret name %q collides with env key", prefix, name))
		}
	}
	return problems
}

// validateCron checks the five POSIX cron fields and their basic ranges. It
// accepts lists, ranges, steps, and the conventional month/day names.
func validateCron(value string) error {
	fields := strings.Fields(value)
	if len(fields) != 5 {
		return fmt.Errorf("must contain exactly 5 fields")
	}
	ranges := [][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	for i, field := range fields {
		if err := validateCronField(field, ranges[i][0], ranges[i][1], i); err != nil {
			return fmt.Errorf("field %d: %s", i+1, err)
		}
	}
	return nil
}

func validateCronField(field string, min, max, index int) error {
	for _, listPart := range strings.Split(field, ",") {
		if listPart == "" {
			return fmt.Errorf("empty list item")
		}
		parts := strings.Split(listPart, "/")
		if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
			return fmt.Errorf("malformed step")
		}
		if len(parts) == 2 {
			step, err := strconv.Atoi(parts[1])
			if err != nil || step <= 0 {
				return fmt.Errorf("step must be a positive integer")
			}
			if parts[0] != "*" && !strings.Contains(parts[0], "-") {
				return fmt.Errorf("step requires '*' or a range")
			}
		}
		if !cronAtom.MatchString(parts[0]) {
			return fmt.Errorf("malformed item %q", listPart)
		}
		if strings.Contains(parts[0], "-") {
			bounds := strings.Split(parts[0], "-")
			if len(bounds) != 2 || bounds[0] == "" || bounds[1] == "" {
				return fmt.Errorf("malformed range")
			}
			lower, err := cronBoundValue(bounds[0], min, max, index)
			if err != nil {
				return err
			}
			upper, err := cronBoundValue(bounds[1], min, max, index)
			if err != nil {
				return err
			}
			if lower > upper {
				return fmt.Errorf("range bounds are reversed")
			}
		} else if parts[0] != "*" {
			if _, err := cronBoundValue(parts[0], min, max, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func cronBoundValue(value string, min, max, index int) (int, error) {
	upper := strings.ToUpper(value)
	var ordered []string
	if index == 3 {
		ordered = []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}
	} else if index == 4 {
		ordered = []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}
	}
	if len(ordered) != 0 {
		offset := 0
		if index == 3 {
			offset = 1
		}
		for n, name := range ordered {
			if name == upper {
				return n + offset, nil
			}
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", value)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("value %d outside range %d-%d", n, min, max)
	}
	return n, nil
}
