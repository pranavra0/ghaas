package compiler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pranavra0/ghaas/pkg/manifest"
	"gopkg.in/yaml.v3"
)

type workflow struct{ root yaml.Node }

const (
	workflowInvocationInput = "ghaas_invocation_id"
	workflowRunNameFormat   = "ghaas: %s/${{ inputs.ghaas_invocation_id || github.run_id }}"
)

const maxWorkflowTimeout = 6 * time.Hour

func buildWorkflow(name string, function manifest.Function, options Options) (workflow, error) {
	if function.Concurrency.Max < 0 || function.Concurrency.Max > 1 {
		return workflow{}, fmt.Errorf("function %q concurrency.max must be 1", name)
	}
	if function.Retry != nil {
		if function.Retry.MaxAttempts < 0 {
			return workflow{}, fmt.Errorf("function %q retry.max_attempts must be positive when specified", name)
		}
		if function.Retry.Backoff < 0 {
			return workflow{}, fmt.Errorf("function %q retry.backoff must be positive when specified", name)
		}
	}
	if err := validateEnvironment(function); err != nil {
		return workflow{}, fmt.Errorf("function %q: %w", name, err)
	}

	root := yaml.Node{Kind: yaml.MappingNode}
	addPair(&root, scalar("name"), scalar("ghaas: "+name))
	addPair(&root, scalar("run-name"), scalarPreserving(fmt.Sprintf(workflowRunNameFormat, name)))

	on := yaml.Node{Kind: yaml.MappingNode}
	if schedule := function.Schedule; schedule != nil && strings.TrimSpace(schedule.Cron) != "" {
		schedules := yaml.Node{Kind: yaml.SequenceNode}
		entry := yaml.Node{Kind: yaml.MappingNode}
		addPair(&entry, scalar("cron"), scalarQuoted(schedule.Cron))
		if timezone := strings.TrimSpace(schedule.Timezone); timezone != "" {
			addPair(&entry, scalar("timezone"), scalar(timezone))
		}
		schedules.Content = append(schedules.Content, &entry)
		addPair(&on, scalar("schedule"), &schedules)
	}

	// The dispatch input is part of the v0.1 identity contract: CLI-created
	// UUIDs are carried into the workflow title and runtime environment. A
	// scheduled run omits it and derives its ID in the runtime.
	dispatch := yaml.Node{Kind: yaml.MappingNode}
	inputs := yaml.Node{Kind: yaml.MappingNode}
	input := yaml.Node{Kind: yaml.MappingNode}
	addPair(&input, scalar("description"), scalar("ghaas logical invocation UUID"))
	addPair(&input, scalar("required"), scalarBool(true))
	addPair(&input, scalar("type"), scalar("string"))
	addPair(&inputs, scalar(workflowInvocationInput), &input)
	addPair(&dispatch, scalar("inputs"), &inputs)
	addPair(&on, scalar("workflow_dispatch"), &dispatch)
	addPair(&root, scalar("on"), &on)

	permissions := yaml.Node{Kind: yaml.MappingNode}
	addPair(&permissions, scalar("contents"), scalar("write"))
	addPair(&root, scalar("permissions"), &permissions)

	concurrency := yaml.Node{Kind: yaml.MappingNode}
	addPair(&concurrency, scalar("group"), scalar("ghaas-"+name))
	addPair(&concurrency, scalar("cancel-in-progress"), scalarBool(false))
	addPair(&root, scalar("concurrency"), &concurrency)

	job := yaml.Node{Kind: yaml.MappingNode}
	addPair(&job, scalar("runs-on"), scalar("ubuntu-latest"))
	totalTimeout, err := retryTimeout(function, options.DefaultTimeoutValue())
	if err != nil {
		return workflow{}, fmt.Errorf("function %q timeout: %w", name, err)
	}
	minutes, err := timeoutMinutes(manifest.Duration(totalTimeout), 0)
	if err != nil {
		return workflow{}, fmt.Errorf("function %q timeout: %w", name, err)
	}
	addPair(&job, scalar("timeout-minutes"), scalarInt(minutes))

	steps := yaml.Node{Kind: yaml.SequenceNode}
	checkout := yaml.Node{Kind: yaml.MappingNode}
	addPair(&checkout, scalar("uses"), scalar("actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683"))
	checkoutWith := yaml.Node{Kind: yaml.MappingNode}
	addPair(&checkoutWith, scalar("persist-credentials"), scalarBool(false))
	addPair(&checkout, scalar("with"), &checkoutWith)
	steps.Content = append(steps.Content, &checkout)

	install, err := options.installer()
	if err != nil {
		return workflow{}, fmt.Errorf("function %q installer: %w", name, err)
	}
	installStep := yaml.Node{Kind: yaml.MappingNode}
	addPair(&installStep, scalar("name"), scalar("Install ghaas"))
	addPair(&installStep, scalar("run"), scalar(install))
	steps.Content = append(steps.Content, &installStep)

	invoke := yaml.Node{Kind: yaml.MappingNode}
	addPair(&invoke, scalar("name"), scalar("Invoke function"))
	env := make(map[string]string, len(function.Environment)+8)
	for key, value := range function.Environment {
		env[key] = value
	}
	// GHAAS_* values are owned by the workflow and are added after manifest
	// values so a manifest cannot shadow identity or tracing metadata.
	env["GHAAS_FUNCTION"] = name
	env["GHAAS_INVOCATION_ID"] = "${{ inputs.ghaas_invocation_id }}"
	env["GHAAS_ATTEMPT"] = "1"
	env["GHAAS_TRIGGER"] = "${{ github.event_name }}"
	env["GHAAS_WORKFLOW_RUN_ID"] = "${{ github.run_id }}"
	env["GHAAS_WORKFLOW_RUN_ATTEMPT"] = "${{ github.run_attempt }}"
	env["GHAAS_ATTEMPT_REASON"] = "${{ github.event_name }}"
	env["GITHUB_TOKEN"] = "${{ github.token }}"
	for _, key := range sortedSecretNames(function.Secrets) {
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
	addPair(&invoke, scalar("run"), scalar("$RUNNER_TEMP/ghaas-bin/ghaas runtime invoke "+name))
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

func retryTimeout(function manifest.Function, fallback time.Duration) (time.Duration, error) {
	duration := time.Duration(function.Timeout)
	if duration == 0 {
		duration = fallback
	}
	if duration <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	maxAttempts := function.EffectiveMaxAttempts(manifest.Defaults{})
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	backoff := time.Duration(function.EffectiveBackoff(manifest.Defaults{}))
	const maxDuration = time.Duration(1<<63 - 1)
	attempts := int64(maxAttempts)
	if int64(duration) > int64(maxDuration)/attempts {
		return 0, fmt.Errorf("retry total timeout is too large")
	}
	total := duration * time.Duration(attempts)
	if attempts > 1 && backoff > 0 {
		retries := attempts - 1
		if int64(backoff) > (int64(maxDuration)-int64(total))/retries {
			return 0, fmt.Errorf("retry total timeout is too large")
		}
		total += backoff * time.Duration(retries)
	}
	if total > maxWorkflowTimeout {
		return 0, fmt.Errorf("retry total timeout cannot exceed GitHub Actions' 360-minute job limit")
	}
	return total, nil
}

func timeoutMinutes(value manifest.Duration, fallback time.Duration) (int, error) {
	duration := time.Duration(value)
	if duration == 0 {
		duration = fallback
	}
	if duration < 0 {
		return 0, fmt.Errorf("must be positive")
	}
	if duration > maxWorkflowTimeout {
		return 0, fmt.Errorf("cannot exceed GitHub Actions' 360-minute job limit")
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

func validateEnvironment(function manifest.Function) error {
	for name, value := range function.Environment {
		if manifest.IsReservedEnvironmentKey(name) {
			return fmt.Errorf("env key %q is reserved", name)
		}
		if manifest.ContainsGitHubExpression(value) {
			return fmt.Errorf("env value for %q contains GitHub expression syntax", name)
		}
	}
	seen := make(map[string]struct{}, len(function.Secrets))
	for _, name := range function.Secrets {
		if manifest.IsReservedEnvironmentKey(name) {
			return fmt.Errorf("secret name %q is reserved", name)
		}
		if _, exists := function.Environment[name]; exists {
			return fmt.Errorf("secret name %q collides with env key", name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("secret name %q is duplicated", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func sortedSecretNames(secrets []string) []string {
	names := append([]string(nil), secrets...)
	sort.Strings(names)
	return names
}
