package config

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"ghaas/pkg/manifest"
)

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	envPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	cronAtom    = regexp.MustCompile(`^(\*|[0-9]+|[A-Za-z]+)(-(\*|[0-9]+|[A-Za-z]+))?$`)
)

var reservedEnvironmentKeys = map[string]struct{}{
	"GHAAS_FUNCTION":        {},
	"GHAAS_INVOCATION_ID":   {},
	"GHAAS_ATTEMPT":         {},
	"GHAAS_TRIGGER":         {},
	"GHAAS_WORKFLOW_RUN_ID": {},
}

// ValidationError contains all semantic errors in stable order.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 0 {
		return "invalid manifest"
	}
	return "invalid manifest: " + strings.Join(e.Problems, "; ")
}

// Validate applies v0.1 semantic constraints. It does not access GitHub or
// inspect the host environment beyond validating IANA timezone names.
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
		}
	}
	if fn.Timeout != 0 && time.Duration(fn.Timeout) <= 0 {
		problems = append(problems, prefix+": timeout must be positive")
	}
	if fn.Schedule != nil {
		problems = append(problems, validateSchedule(prefix, *fn.Schedule)...)
	}
	if fn.Concurrency.Max != 0 && fn.Concurrency.Max != 1 {
		problems = append(problems, fmt.Sprintf("%s: concurrency.max must be 1 (got %d)", prefix, fn.Concurrency.Max))
	}
	if fn.State != nil {
		problems = append(problems, prefix+": state is not supported in manifest version 1")
	}
	if fn.Retry != nil {
		problems = append(problems, prefix+": retry is not supported in manifest version 1")
	}
	problems = append(problems, validateEnvironment(prefix, fn.Environment, fn.Secrets)...)
	return problems
}

func validateSchedule(prefix string, schedule manifest.ScheduleConfig) []string {
	var problems []string
	if strings.TrimSpace(schedule.Cron) == "" {
		problems = append(problems, prefix+": schedule.cron is required")
	} else if err := validateCron(schedule.Cron); err != nil {
		problems = append(problems, prefix+": schedule.cron "+err.Error())
	}
	if tz := strings.TrimSpace(schedule.Timezone); tz != "" {
		if tz == "Local" {
			problems = append(problems, fmt.Sprintf("%s: schedule.timezone %q is not a valid IANA timezone", prefix, schedule.Timezone))
		} else if _, err := time.LoadLocation(schedule.Timezone); err != nil {
			problems = append(problems, fmt.Sprintf("%s: schedule.timezone %q is not a valid IANA timezone", prefix, schedule.Timezone))
		}
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
	if names := cronNames[index]; len(names) != 0 {
		upper := strings.ToUpper(value)
		ordered := []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}
		offset := 0
		if index == 4 {
			ordered = []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}
		} else {
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

var cronNames = [5]map[string]struct{}{
	{}, {}, {}, {
		"JAN": {}, "FEB": {}, "MAR": {}, "APR": {}, "MAY": {}, "JUN": {},
		"JUL": {}, "AUG": {}, "SEP": {}, "OCT": {}, "NOV": {}, "DEC": {},
	}, {
		"SUN": {}, "MON": {}, "TUE": {}, "WED": {}, "THU": {}, "FRI": {}, "SAT": {},
	},
}
