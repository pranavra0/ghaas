package compiler

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultGhaasInstall = `go install ./cmd/ghaas && echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"`

type workflow struct{ root yaml.Node }

func buildWorkflow(name string, function any, options Options) (workflow, error) {
	v := indirect(reflect.ValueOf(function))
	if !v.IsValid() {
		return workflow{}, fmt.Errorf("function %q is nil", name)
	}

	install := options.GhaasInstall
	if install == "" {
		install = options.InstallCommand
	}
	if install == "" {
		install = options.Install
	}
	if install == "" {
		install = options.GhaasInstallCommand
	}
	if install == "" {
		install = options.InstallURL
	}
	if install == "" && options.GhaasVersion != "" {
		install = "go install ghaas/cmd/ghaas@" + options.GhaasVersion
	}
	if install == "" {
		install = defaultGhaasInstall
	}

	root := yaml.Node{Kind: yaml.MappingNode}
	addPair(&root, scalar("name"), scalar("ghaas: "+name))
	addPair(&root, scalar("run-name"), scalarPreserving("ghaas: "+name+"/${{ inputs.ghaas_invocation_id || github.run_id }}"))

	// Keep all trigger configuration in a YAML mapping. In particular, the
	// timezone is a field of the schedule entry, not a YAML comment: comments
	// are not part of the workflow's semantics and are easily lost by tools
	// which load and re-emit the generated document.
	on := yaml.Node{Kind: yaml.MappingNode}
	schedule := indirect(field(v, "Schedule"))
	if schedule.IsValid() {
		cron := stringField(schedule, "Cron")
		if cron == "" && stringField(schedule, "Target") != "" {
			// A target/window schedule needs frequent opportunities. The
			// runtime still deduplicates the logical tick; use the configured
			// retry cadence when it is representable as whole minutes.
			cron = scheduleOpportunityCron(schedule)
		}
		if cron != "" {
			schedules := yaml.Node{Kind: yaml.SequenceNode}
			entry := yaml.Node{Kind: yaml.MappingNode}
			addPair(&entry, scalar("cron"), scalarQuoted(cron))
			if timezone := stringField(schedule, "Timezone"); timezone != "" {
				addPair(&entry, scalar("timezone"), scalar(timezone))
			}
			schedules.Content = append(schedules.Content, &entry)
			addPair(&on, scalar("schedule"), &schedules)
		}
	}

	// Manual dispatch is deliberately present even for scheduled functions.
	// The input is optional so scheduled runs can derive their logical ID in
	// the runtime, while callers can supply a stable ID for manual runs.
	dispatch := yaml.Node{Kind: yaml.MappingNode}
	inputs := yaml.Node{Kind: yaml.MappingNode}
	input := yaml.Node{Kind: yaml.MappingNode}
	addPair(&input, scalar("description"), scalar("ghaas logical invocation UUID"))
	addPair(&input, scalar("required"), scalarBool(false))
	addPair(&input, scalar("type"), scalar("string"))
	addPair(&inputs, scalar("ghaas_invocation_id"), &input)
	addPair(&dispatch, scalar("inputs"), &inputs)
	addPair(&on, scalar("workflow_dispatch"), &dispatch)
	addPair(&root, scalar("on"), &on)

	// Checkout requires repository contents read access. Branch-backed state
	// commits need write access, while all other state backends should remain
	// read-only. Exhausted-invocation issue creation is independently granted
	// only when configured.
	permissions := yaml.Node{Kind: yaml.MappingNode}
	contentsPermission := "read"
	if stateBackend(v) == "branch" {
		contentsPermission = "write"
	}
	addPair(&permissions, scalar("contents"), scalar(contentsPermission))
	if onExhaustedIssue(v) {
		addPair(&permissions, scalar("issues"), scalar("write"))
	}
	addPair(&root, scalar("permissions"), &permissions)

	concurrency := yaml.Node{Kind: yaml.MappingNode}
	addPair(&concurrency, scalar("group"), scalar("ghaas-"+name))
	addPair(&concurrency, scalar("cancel-in-progress"), scalarBool(false))
	addPair(&root, scalar("concurrency"), &concurrency)

	job := yaml.Node{Kind: yaml.MappingNode}
	addPair(&job, scalar("runs-on"), scalar("ubuntu-latest"))
	minutes, err := timeoutMinutes(v, options.DefaultTimeoutValue())
	if err != nil {
		return workflow{}, fmt.Errorf("function %q timeout: %w", name, err)
	}
	addPair(&job, scalar("timeout-minutes"), scalarInt(minutes))

	steps := yaml.Node{Kind: yaml.SequenceNode}
	checkout := yaml.Node{Kind: yaml.MappingNode}
	addPair(&checkout, scalar("uses"), scalar("actions/checkout@v4"))
	steps.Content = append(steps.Content, &checkout)

	installStep := yaml.Node{Kind: yaml.MappingNode}
	addPair(&installStep, scalar("name"), scalar("Install ghaas"))
	// Options.GhaasInstall is an explicit, trusted installer command. The
	// function's command is never interpolated into this shell command.
	addPair(&installStep, scalar("run"), scalar(install))
	steps.Content = append(steps.Content, &installStep)

	invoke := yaml.Node{Kind: yaml.MappingNode}
	addPair(&invoke, scalar("name"), scalar("Invoke function"))
	env := environment(v)

	// User-provided environment is data in YAML, not a template fragment.
	// Runtime metadata is assigned afterwards so a manifest cannot shadow the
	// values used for invocation identity and tracing.
	env["GHAAS_FUNCTION"] = name
	env["GHAAS_INVOCATION_ID"] = "${{ inputs.ghaas_invocation_id }}"
	env["GHAAS_ATTEMPT"] = "1"
	env["GHAAS_TRIGGER"] = "${{ github.event_name }}"
	env["GHAAS_WORKFLOW_RUN_ID"] = "${{ github.run_id }}"
	for key, value := range workflowMetadata(v) {
		// Derived metadata is authoritative. A user environment entry must
		// never replace invocation identity or manifest-derived runtime data.
		env[key] = value
	}

	secretNames := secretValues(v)
	sort.Strings(secretNames)
	for _, key := range secretNames {
		// A declared secret always resolves from GitHub Secrets, even if a
		// user environment map happens to contain the same key.
		env[key] = "${{ secrets." + key + " }}"
	}
	envNode := yaml.Node{Kind: yaml.MappingNode}
	envKeys := make([]string, 0, len(env))
	for key := range env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		addPair(&envNode, scalar(key), scalarPreserving(env[key]))
	}
	addPair(&invoke, scalar("env"), &envNode)

	// The function name is validated before this builder is called. The
	// manifest command itself is intentionally not emitted into a shell
	// expression; runtime invoke loads it from the checked-out manifest and
	// executes its argv directly.
	addPair(&invoke, scalar("run"), scalar("ghaas runtime invoke "+name))
	steps.Content = append(steps.Content, &invoke)
	addPair(&job, scalar("steps"), &steps)

	jobs := yaml.Node{Kind: yaml.MappingNode}
	addPair(&jobs, scalar("invoke"), &job)
	addPair(&root, scalar("jobs"), &jobs)
	return workflow{root: root}, nil
}

func renderWorkflow(w workflow) ([]byte, error) {
	content, err := yaml.Marshal(&w.root)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Code generated by ghaas. DO NOT EDIT.\n"), content...), nil
}

func addPair(mapping *yaml.Node, key, value *yaml.Node) {
	mapping.Content = append(mapping.Content, key, value)
}
func scalar(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
func scalarInt(value int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)}
}
func scalarBool(value bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(value)}
}
func scalarQuoted(value string) *yaml.Node {
	n := scalar(value)
	n.Style = yaml.DoubleQuotedStyle
	return n
}
func scalarPreserving(value string) *yaml.Node {
	// Expressions are plain data, never shell fragments. Double-quoting only
	// affects YAML syntax and leaves the expression value byte-for-byte intact.
	if strings.HasPrefix(value, "${{") && strings.HasSuffix(value, "}}") {
		return scalarQuoted(value)
	}
	return scalar(value)
}

func stringField(v reflect.Value, name string) string {
	return stringValue(field(v, name))
}
func stringValue(v reflect.Value) string {
	v = indirect(v)
	if !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, v.Type().Bits())
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	}
	if v.CanInterface() {
		if s, ok := v.Interface().(fmt.Stringer); ok {
			return s.String()
		}
	}
	return ""
}

// firstField returns the first present field from names. Reflection keeps the
// compiler compatible with the public manifest types as optional configuration
// structs evolve, while retaining the same behavior for callers using small
// test structs.
func firstField(v reflect.Value, names ...string) reflect.Value {
	v = indirect(v)
	for _, name := range names {
		if candidate := field(v, name); candidate.IsValid() {
			return candidate
		}
	}
	return reflect.Value{}
}

func intValue(v reflect.Value) (int, bool) {
	v = indirect(v)
	if !v.IsValid() {
		return 0, false
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(v.Uint()), true
	case reflect.String:
		if v.String() == "" {
			return 0, false
		}
		n, err := strconv.Atoi(v.String())
		return n, err == nil
	default:
		return 0, false
	}
}

func boolValue(v reflect.Value) (bool, bool) {
	v = indirect(v)
	if !v.IsValid() {
		return false, false
	}
	if v.Kind() == reflect.Bool {
		return v.Bool(), true
	}
	if v.Kind() == reflect.String {
		b, err := strconv.ParseBool(v.String())
		return b, err == nil
	}
	return false, false
}

func durationString(v reflect.Value) string {
	d, err := durationValue(v)
	if err != nil || d <= 0 {
		return ""
	}
	return d.String()
}
func onExhaustedIssue(v reflect.Value) bool {
	if exhausted := firstField(v, "OnExhausted"); exhausted.IsValid() {
		if issue, ok := boolValue(exhausted); ok && issue {
			return true
		}
		if boolFieldAny(exhausted, "Issue", "CreateIssue") {
			return true
		}
	}
	if retry := firstField(v, "Retry"); retry.IsValid() {
		if exhausted := firstField(retry, "OnExhausted"); exhausted.IsValid() {
			if issue, ok := boolValue(exhausted); ok && issue {
				return true
			}
			return boolFieldAny(exhausted, "Issue", "CreateIssue")
		}
	}
	return false
}

func stateBackend(v reflect.Value) string {
	state := indirect(field(v, "State"))
	if !state.IsValid() {
		return ""
	}
	backend := stringFieldAny(state, "Backend", "Type")
	if backend == "" {
		backend = stringValue(state)
	}
	return strings.ToLower(strings.TrimSpace(backend))
}

func scheduleOpportunityCron(schedule reflect.Value) string {
	retry := indirect(firstField(schedule, "Retry"))
	if retry.IsValid() {
		every := durationFromReflect(firstField(retry, "Every"))
		if every > 0 && every%time.Minute == 0 {
			minutes := int(every / time.Minute)
			if minutes >= 5 && minutes < 60 {
				return fmt.Sprintf("*/%d * * * *", minutes)
			}
			if minutes >= 60 && minutes%60 == 0 && minutes/60 <= 23 {
				return fmt.Sprintf("0 */%d * * *", minutes/60)
			}
		}
	}
	// GitHub Actions throttles schedules more frequent than every five
	// minutes; this is the safest opportunity cadence for a target window.
	return "*/5 * * * *"
}

// workflowMetadata carries configuration which the thin workflow cannot
// implement itself to the runtime as ordinary environment data.
func workflowMetadata(v reflect.Value) map[string]string {
	metadata := make(map[string]string)
	if schedule := indirect(field(v, "Schedule")); schedule.IsValid() {
		addMetadataString(metadata, "GHAAS_SCHEDULE_TIMEZONE", stringField(schedule, "Timezone"))
		addMetadataString(metadata, "GHAAS_SCHEDULE_TARGET", stringFieldAny(schedule, "Target"))
		addScheduleWindowMetadata(metadata, schedule)
		if scheduleRetry := indirect(firstField(schedule, "Retry")); scheduleRetry.IsValid() {
			addMetadataString(metadata, "GHAAS_SCHEDULE_RETRY_EVERY", durationOrString(scheduleRetry, "Every", "Backoff"))
			addMetadataString(metadata, "GHAAS_SCHEDULE_RETRY_UNTIL", stringFieldAny(scheduleRetry, "Until", "End"))
		}
	}
	if retry := indirect(field(v, "Retry")); retry.IsValid() {
		if max, ok := intValue(firstField(retry, "MaxAttempts", "Attempts", "Max")); ok && max > 0 {
			metadata["GHAAS_RETRY_MAX_ATTEMPTS"] = strconv.Itoa(max)
		}
		addMetadataString(metadata, "GHAAS_RETRY_BACKOFF",
			durationOrString(retry, "Backoff", "Every"))
	}

	if concurrency := indirect(field(v, "Concurrency")); concurrency.IsValid() {
		if max, ok := intValue(firstField(concurrency, "Max")); ok && max > 0 {
			metadata["GHAAS_CONCURRENCY_MAX"] = strconv.Itoa(max)
		}
	}

	if state := indirect(field(v, "State")); state.IsValid() {
		backend := stringFieldAny(state, "Backend", "Type")
		if backend == "" {
			backend = stringValue(state)
		}
		backend = strings.TrimSpace(backend)
		addMetadataString(metadata, "GHAAS_STATE_BACKEND", backend)
		branch := stringFieldAny(state, "Branch", "BranchName", "Ref")
		if branch == "" && strings.EqualFold(backend, "branch") {
			branch = "ghaas-state"
		}
		addMetadataString(metadata, "GHAAS_STATE_BRANCH", branch)
	}

	// on_exhausted is a policy hint for the runtime/dead-letter integration;
	// keeping it in metadata means enabling it does not turn the workflow into
	// a shell implementation of retry semantics.
	if onExhaustedIssue(v) {
		metadata["GHAAS_ON_EXHAUSTED_ISSUE"] = "true"
	}

	if slo := indirect(field(v, "SLO")); slo.IsValid() {
		addMetadataString(metadata, "GHAAS_SLO_SUCCESS_RATE",
			stringFieldAny(slo, "SuccessRate"))
		addMetadataString(metadata, "GHAAS_SLO_SCHEDULE_DELAY",
			durationOrString(slo, "ScheduleDelay", "Delay"))
	}
	return metadata
}

func addScheduleWindowMetadata(metadata map[string]string, schedule reflect.Value) {
	var window reflect.Value
	for _, name := range []string{"ExecutionWindow", "Window"} {
		candidate := indirect(field(schedule, name))
		if candidate.IsValid() {
			window = candidate
			break
		}
	}

	if window.IsValid() {
		// Some compatible callers model a window as a scalar. Keep that value
		// intact; structured windows also expose their boundaries separately.
		if text := stringValue(window); text != "" {
			addMetadataString(metadata, "GHAAS_SCHEDULE_WINDOW", text)
			return
		}
		start := stringFieldAny(window, "Start", "From")
		end := stringFieldAny(window, "End", "Until", "To")
		addMetadataString(metadata, "GHAAS_SCHEDULE_WINDOW_START", start)
		addMetadataString(metadata, "GHAAS_SCHEDULE_WINDOW_END", end)
		if start != "" && end != "" {
			addMetadataString(metadata, "GHAAS_SCHEDULE_WINDOW", start+"-"+end)
		}
		return
	}

	// ScheduleConfig also supports the flattened window_start/window_end
	// spelling. Read those fields from the schedule itself rather than from a
	// missing nested window value.
	start := stringFieldAny(schedule, "WindowStart")
	end := stringFieldAny(schedule, "WindowEnd")
	addMetadataString(metadata, "GHAAS_SCHEDULE_WINDOW_START", start)
	addMetadataString(metadata, "GHAAS_SCHEDULE_WINDOW_END", end)
	if start != "" && end != "" {
		addMetadataString(metadata, "GHAAS_SCHEDULE_WINDOW", start+"-"+end)
	}
}

func addMetadataString(metadata map[string]string, key, value string) {
	if value != "" {
		metadata[key] = value
	}
}

func stringFieldAny(v reflect.Value, names ...string) string {
	return stringValue(firstField(v, names...))
}
func boolFieldAny(v reflect.Value, names ...string) bool {
	v = indirect(v)
	for _, name := range names {
		if value, ok := boolValue(field(v, name)); ok {
			return value
		}
	}
	return false
}

func durationOrString(v reflect.Value, names ...string) string {
	value := firstField(v, names...)
	if text := durationString(value); text != "" {
		return text
	}
	return stringValue(value)
}

func timeoutMinutes(v reflect.Value, fallback time.Duration) (int, error) {
	value := indirect(field(v, "Timeout"))
	if !value.IsValid() {
		return durationMinutes(fallback), nil
	}
	duration, err := durationValue(value)
	if err != nil {
		return 0, err
	}
	if duration <= 0 {
		return durationMinutes(fallback), nil
	}
	return durationMinutes(duration), nil
}

func durationMinutes(duration time.Duration) int {
	if duration <= 0 {
		duration = 15 * time.Minute
	}
	minutes := int(duration / time.Minute)
	if duration%time.Minute != 0 {
		minutes++
	}
	if minutes < 1 {
		minutes = 1
	}
	return minutes
}

func durationValue(value reflect.Value) (time.Duration, error) {
	value = indirect(value)
	if !value.IsValid() {
		return 0, nil
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return time.Duration(value.Int()), nil
	case reflect.String:
		if value.String() == "" {
			return 0, nil
		}
		return time.ParseDuration(value.String())
	case reflect.Struct:
		return durationValue(field(value, "Duration"))
	default:
		return 0, fmt.Errorf("unsupported duration type %s", value.Type())
	}
}
func durationFromReflect(v reflect.Value) time.Duration {
	d, _ := durationValue(v)
	return d
}

func environment(v reflect.Value) map[string]string {
	for _, name := range []string{"Environment", "Env"} {
		m := indirect(field(v, name))
		if !m.IsValid() || m.Kind() != reflect.Map {
			continue
		}
		out := make(map[string]string, m.Len())
		for _, key := range m.MapKeys() {
			out[stringValue(key)] = stringValue(m.MapIndex(key))
		}
		return out
	}
	return map[string]string{}
}
func secretValues(v reflect.Value) []string {
	values := indirect(field(v, "Secrets"))
	if !values.IsValid() || (values.Kind() != reflect.Slice && values.Kind() != reflect.Array) {
		return nil
	}
	out := make([]string, 0, values.Len())
	for i := 0; i < values.Len(); i++ {
		if name := stringValue(values.Index(i)); name != "" {
			out = append(out, name)
		}
	}
	return out
}
