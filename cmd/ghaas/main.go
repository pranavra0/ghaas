package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"ghaas/internal/cli"
	"ghaas/internal/compiler"
	"ghaas/internal/config"
	"ghaas/internal/github"
	"ghaas/internal/invocation"
	"ghaas/internal/state"
	"ghaas/pkg/manifest"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "v0.1.0"

// processState is shared by the CLI and runtime when they are embedded in
// one process (for example, integration tests). The default is a local
// branch-shaped store so independent CLI invocations retain logical state.
var (
	processState     invocation.StateStore
	processStateRoot string
	processStateMu   sync.Mutex

	// exhaustedIssues suppresses duplicate issue creation when a workflow
	// retries the same exhausted invocation in one process. A successful
	// notification is retained; failed notifications are deliberately
	// retryable on a later run.
	exhaustedIssueMu sync.Mutex
	exhaustedIssues  = make(map[string]struct{})
)

const (
	defaultStateBranch    = string(manifest.DefaultStateBranch)
	exhaustedIssueTimeout = 15 * time.Second
	// The runtime is built from the checked-out source so generated workflows
	// are self-contained and do not execute mutable remote code.
	workflowGhaasInstallCmd = `go install ./cmd/ghaas && echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"`
)

// sharedState opens the durable local state store for one branch/ref. The
// branch is part of the on-disk namespace and is passed to BranchStore so two
// refs cannot collide. Errors are returned to callers rather than silently
// downgrading execution to a process-local memory store.
func sharedState(branch string) (invocation.StateStore, error) {
	if strings.TrimSpace(branch) == "" {
		branch = defaultStateBranch
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determine state directory: %w", err)
	}
	root := filepath.Join(cwd, ".ghaas-state")
	key := root + "\x00" + branch

	processStateMu.Lock()
	defer processStateMu.Unlock()
	if processState != nil && processStateRoot == key {
		return processState, nil
	}

	// BranchStore incorporates the branch/ref in its namespace. Keep the
	// storage root stable so callers can inspect the conventional
	// `.ghaas-state/<branch>/functions` layout while refs remain isolated.
	store, openErr := state.OpenBranchStore(root, branch)
	if openErr != nil {
		return nil, fmt.Errorf("open state branch %q: %w", branch, openErr)
	}
	processState = store
	processStateRoot = key
	return processState, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := execute(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghaas:", err)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func execute(ctx context.Context, args []string) (int, error) {
	if ctx == nil {
		return 1, fmt.Errorf("nil context")
	}
	if len(args) >= 2 && args[0] == "runtime" && args[1] == "invoke" {
		return runtimeInvoke(ctx, args[2:])
	}
	if err := cli.Run(ctx, args, services()); err != nil {
		return 1, err
	}
	return 0, nil
}

func runtimeInvoke(ctx context.Context, args []string) (int, error) {
	if len(args) != 1 || args[0] == "" {
		return 1, fmt.Errorf("usage: ghaas runtime invoke FUNCTION")
	}
	if ctx == nil {
		return 1, fmt.Errorf("nil context")
	}
	function := args[0]
	m, err := config.Load("ghaas.yaml")
	if err != nil {
		return 1, err
	}
	fn, ok := m.Functions[function]
	if !ok {
		return 1, fmt.Errorf("function %q not found", function)
	}
	if configured := strings.TrimSpace(os.Getenv("GHAAS_FUNCTION")); configured != "" && configured != function {
		return 1, fmt.Errorf("GHAAS_FUNCTION %q does not match function %q", configured, function)
	}
	timeout := 15 * time.Minute
	if fn.Timeout > 0 {
		timeout = time.Duration(fn.Timeout)
	} else if m.Defaults.Timeout > 0 {
		timeout = time.Duration(m.Defaults.Timeout)
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id, err := runtimeInvocationIDFor(function, fn.Schedule)
	if err != nil {
		return 1, err
	}
	event := strings.TrimSpace(os.Getenv("GITHUB_EVENT_NAME"))
	trigger := event
	switch event {
	case "", "workflow_dispatch":
		trigger = "manual"
	case "schedule":
		trigger = "schedule"
	}
	if trigger == "schedule" && fn.Schedule != nil && !scheduleExecutionAllowed(fn.Schedule, time.Now()) {
		fmt.Fprintf(os.Stdout, "skipped %s: outside execution window\n", function)
		return 0, nil
	}
	var workflowRunID int64
	if raw := strings.TrimSpace(os.Getenv("GITHUB_RUN_ID")); raw != "" {
		workflowRunID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || workflowRunID < 0 {
			return 1, fmt.Errorf("invalid GITHUB_RUN_ID %q", raw)
		}
	}
	store, err := runtimeStateStore(fn)
	if err != nil {
		return 1, err
	}
	wasExhausted := false
	if existing, getErr := store.Get(ctx, function, id); getErr == nil && existing != nil {
		wasExhausted = existing.Status == invocation.StatusExhausted
	} else if getErr != nil && !errors.Is(getErr, invocation.ErrNotFound) {
		return 1, fmt.Errorf("read invocation state: %w", getErr)
	}
	env := withEnvironment(os.Environ(), fn.Environment)
	runtime := invocation.Runtime{
		Store:            store,
		Runner:           invocation.Runner{Env: env},
		Trigger:          trigger,
		WorkflowRunID:    workflowRunID,
		LeaseTTL:         timeout,
		RetryMaxAttempts: retryMaxAttempts(fn),
		RetryBackoff:     retryBackoff(fn),
	}
	record, runErr := runtime.Invoke(runContext, function, id, fn.Command)
	if !wasExhausted && record.Status == invocation.StatusExhausted && fn.OnExhausted != nil && fn.OnExhausted.Issue {
		notifyExhausted(ctx, function, record)
	}
	if runErr != nil {
		if record.Result != nil && record.Result.ExitCode > 0 {
			return record.Result.ExitCode, nil
		}
		return 1, runErr
	}
	return 0, nil
}

func runtimeStateStore(fn manifest.Function) (invocation.StateStore, error) {
	if fn.State == nil {
		return sharedState(defaultStateBranch)
	}
	switch fn.State.Backend {
	case manifest.StateBackendMemory:
		return state.NewMemory(), nil
	case manifest.StateBackendBranch:
		return sharedState(fn.State.EffectiveBranch())
	default:
		return nil, fmt.Errorf("unsupported state backend %q", fn.State.Backend)
	}
}

func notifyExhausted(ctx context.Context, function string, record invocation.Invocation) {
	if record.ID == "" {
		return
	}
	if function == "" {
		function = record.Function
	}
	// Hold the lock through the bounded request. This is intentionally simple:
	// concurrent retries for one process cannot race into duplicate issues, and
	// at most one request is in flight at a time.
	exhaustedIssueMu.Lock()
	defer exhaustedIssueMu.Unlock()
	key := string(record.ID)
	if _, alreadySent := exhaustedIssues[key]; alreadySent {
		return
	}
	client, err := github.NewClientFromEnv()
	if err != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	notifyContext, cancel := context.WithTimeout(ctx, exhaustedIssueTimeout)
	defer cancel()
	title := fmt.Sprintf("[ghaas] %s exhausted retries", record.ID)
	lastWorkflow := "none"
	if record.WorkflowRunID > 0 {
		lastWorkflow = strconv.FormatInt(record.WorkflowRunID, 10)
	}
	lastExitCode := "unknown"
	if record.Result != nil {
		lastExitCode = strconv.Itoa(record.Result.ExitCode)
	}
	body := fmt.Sprintf(
		"Function: %s\nInvocation: %s\nAttempts: %d\nLast workflow: %s\nLast exit code: %s",
		function, record.ID, record.Attempts, lastWorkflow, lastExitCode,
	)
	if _, err := client.CreateIssue(notifyContext, title, body); err == nil {
		exhaustedIssues[key] = struct{}{}
	}
}

func retryMaxAttempts(fn manifest.Function) int {
	if fn.Retry == nil {
		return 0
	}
	return fn.Retry.MaxAttempts
}

func retryBackoff(fn manifest.Function) time.Duration {
	if fn.Retry == nil {
		return 0
	}
	return time.Duration(fn.Retry.Backoff)
}

func runtimeInvocationID(function string) (invocation.InvocationID, error) {
	return runtimeInvocationIDFor(function, nil)
}

func runtimeInvocationIDFor(function string, schedule *manifest.ScheduleConfig) (invocation.InvocationID, error) {
	if value := strings.TrimSpace(os.Getenv("GHAAS_INVOCATION_ID")); value != "" {
		id := invocation.InvocationID(value)
		if err := invocation.ValidateInvocationID(id); err != nil {
			return "", fmt.Errorf("invalid GHAAS_INVOCATION_ID: %w", err)
		}
		if !strings.HasPrefix(value, function+"/") {
			return "", fmt.Errorf("GHAAS_INVOCATION_ID %q does not belong to function %q", value, function)
		}
		return id, nil
	}
	if strings.TrimSpace(os.Getenv("GITHUB_EVENT_NAME")) == "schedule" {
		now := time.Now().UTC()
		cron := ""
		if schedule != nil {
			cron = schedule.Cron
			if schedule.Timezone != "" {
				location, err := time.LoadLocation(schedule.Timezone)
				if err != nil {
					return "", fmt.Errorf("load schedule timezone %q: %w", schedule.Timezone, err)
				}
				now = now.In(location)
			}
			if strings.TrimSpace(schedule.Target) != "" {
				return invocation.NewScheduledID(function, targetScheduleKey(now, schedule.Target))
			}
		}
		return invocation.NewScheduledID(function, scheduleKey(now, cron))
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate invocation ID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	uuid := hex.EncodeToString(raw[:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" +
		hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" + hex.EncodeToString(raw[10:])
	return invocation.NewManualID(function, uuid)
}

// scheduleKey derives a stable key for the cron occurrence represented by now.
// For the common single-occurrence forms it preserves the human-readable keys
// from the manifest contract. Schedules with lists/steps use the full local
// minute, preventing distinct ticks from colliding.
func scheduleKey(now time.Time, cron string) string {
	parts := strings.Fields(cron)
	if len(parts) != 5 {
		return now.Truncate(time.Minute).Format("2006-01-02T15:04-07:00")
	}
	minute, hour, dom, month, dow := parts[0], parts[1], parts[2], parts[3], parts[4]
	tick := now.Truncate(time.Minute)
	if isSingleCronField(minute) && isSingleCronField(hour) && dom == "*" && month == "*" {
		if isSingleWeekday(dow) {
			year, week := tick.ISOWeek()
			return fmt.Sprintf("%04d-W%02d", year, week)
		}
		if dow == "*" {
			return tick.Format("2006-01-02")
		}
	}
	if isSingleCronField(minute) && hour == "*" && dom == "*" && month == "*" && dow == "*" {
		return tick.Format("2006-01-02T15")
	}
	if minute == "*" && hour == "*" && dom == "*" && month == "*" && dow == "*" {
		return tick.Format("2006-01-02T15:04-07:00")
	}
	return tick.Format("2006-01-02T15:04-07:00")
}

func isSingleCronField(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func isSingleWeekday(value string) bool {
	if isSingleCronField(value) {
		n := 0
		if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
			return false
		}
		return n >= 0 && n <= 7
	}
	switch strings.ToUpper(value) {
	case "SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT":
		return true
	default:
		return false
	}
}
func targetScheduleKey(now time.Time, target string) string {
	if _, ok := targetWeekday(target); ok {
		year, week := now.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	}
	return now.Format("2006-01-02")
}

func targetWeekday(target string) (time.Weekday, bool) {
	for _, token := range strings.Fields(target) {
		switch strings.ToUpper(strings.Trim(token, ",.")) {
		case "SUN", "SUNDAY":
			return time.Sunday, true
		case "MON", "MONDAY":
			return time.Monday, true
		case "TUE", "TUESDAY":
			return time.Tuesday, true
		case "WED", "WEDNESDAY":
			return time.Wednesday, true
		case "THU", "THURSDAY":
			return time.Thursday, true
		case "FRI", "FRIDAY":
			return time.Friday, true
		case "SAT", "SATURDAY":
			return time.Saturday, true
		}
	}
	return time.Sunday, false
}

func scheduleExecutionAllowed(schedule *manifest.ScheduleConfig, now time.Time) bool {
	if schedule == nil {
		return true
	}
	window := schedule.EffectiveExecutionWindow()
	start, end := "", ""
	if window != nil {
		start, end = window.Start, window.End
	}
	if schedule.Retry != nil && end == "" {
		end = schedule.Retry.Until
		if start == "" {
			start = targetClock(schedule.Target)
		}
	}
	if schedule.Target != "" && window == nil && schedule.Retry == nil {
		location, err := time.LoadLocation(schedule.Timezone)
		if err != nil {
			return false
		}
		local := now.In(location)
		if weekday, ok := targetWeekday(schedule.Target); ok && local.Weekday() != weekday {
			return false
		}
		targetMinute, ok := parseClock(targetClock(schedule.Target))
		return ok && local.Hour()*60+local.Minute() == targetMinute
	}
	if start == "" && end != "" {
		start = "00:00"
	}
	if start == "" && end == "" {
		return true
	}
	startMinute, startOK := parseClock(start)
	endMinute, endOK := parseClock(end)
	if !startOK || !endOK {
		return false
	}
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		return false
	}
	local := now.In(location)
	if weekday, ok := targetWeekday(schedule.Target); ok && local.Weekday() != weekday {
		return false
	}
	minute := local.Hour()*60 + local.Minute()
	return minute >= startMinute && minute < endMinute
}

func targetClock(target string) string {
	for _, token := range strings.Fields(target) {
		if len(token) >= 5 && token[2] == ':' {
			return token[:5]
		}
	}
	return ""
}

func parseClock(value string) (int, bool) {
	if len(value) != 5 || value[2] != ':' {
		return 0, false
	}
	hour, hourErr := strconv.Atoi(value[:2])
	minute, minuteErr := strconv.Atoi(value[3:])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func newUUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate invocation ID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return hex.EncodeToString(raw[:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" +
		hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" + hex.EncodeToString(raw[10:]), nil
}

func withEnvironment(base []string, values map[string]string) []string {
	result := append([]string(nil), base...)
	if len(values) == 0 {
		return result
	}
	seen := make(map[string]struct{}, len(values))
	for _, entry := range result {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if _, exists := seen[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func services() cli.Services {
	return cli.Services{
		ManifestPath: "ghaas.yaml",
		LoadManifest: func(path string) (any, error) { return config.Load(path) },
		Validate: func(value any) error {
			m, ok := value.(manifest.Manifest)
			if !ok {
				return fmt.Errorf("unexpected manifest type %T", value)
			}
			return config.Validate(m)
		},
		FunctionNames: func(value any) []string {
			m, ok := value.(manifest.Manifest)
			if !ok {
				return nil
			}
			names := make([]string, 0, len(m.Functions))
			for name := range m.Functions {
				names = append(names, name)
			}
			return names
		},
		Compile: func(value any, function string) (cli.Compiled, error) {
			m, ok := value.(manifest.Manifest)
			if !ok {
				return cli.Compiled{}, fmt.Errorf("unexpected manifest type %T", value)
			}
			fn, ok := m.Functions[function]
			if !ok {
				return cli.Compiled{}, fmt.Errorf("function %q not found", function)
			}
			defaultTimeout := 15 * time.Minute
			if m.Defaults.Timeout > 0 {
				defaultTimeout = time.Duration(m.Defaults.Timeout)
			}
			// `go install` places binaries in GOPATH/bin, which is not
			// guaranteed to be on a hosted runner's PATH. Export it through
			// GITHUB_PATH before the generated invocation step runs.
			workflowFunction := fn
			// Normalize the compatibility `ref` spelling to `branch` for
			// compiler adapters that inspect only the first field. The
			// runtime still receives the original manifest and selects the
			// same effective branch.
			if workflowFunction.State != nil && workflowFunction.State.Backend == manifest.StateBackendBranch {
				stateConfig := *workflowFunction.State
				stateConfig.Branch = stateConfig.EffectiveBranch()
				stateConfig.Ref = ""
				workflowFunction.State = &stateConfig
			}
			workflowFunction.Environment = make(map[string]string, len(fn.Environment)+1)
			for key, value := range fn.Environment {
				workflowFunction.Environment[key] = value
			}
			// NewClientFromEnv intentionally reads GITHUB_TOKEN. Actions does
			// not export that value automatically, so wire the ephemeral token
			// only for the optional dead-letter issue path.
			if fn.OnExhausted != nil && fn.OnExhausted.Issue {
				if _, configured := workflowFunction.Environment["GITHUB_TOKEN"]; !configured {
					workflowFunction.Environment["GITHUB_TOKEN"] = "${{ secrets.GITHUB_TOKEN }}"
				}
			}
			artifact, err := compiler.Compile(function, workflowFunction, compiler.Options{
				GhaasInstall:   workflowGhaasInstallCmd,
				DefaultTimeout: defaultTimeout,
			})
			if err != nil {
				return cli.Compiled{}, err
			}
			return cli.Compiled{Filename: filepath.Base(artifact.Path), Content: string(artifact.Content)}, nil
		},
		NewGitHub: func() (github.GitHub, error) { return github.NewClientFromEnv() },
		State:     nil,
		NewState:  func() (invocation.StateStore, error) { return sharedState(defaultStateBranch) },
		StateFor: func(value any, function string) (invocation.StateStore, error) {
			m, ok := value.(manifest.Manifest)
			if !ok {
				return nil, fmt.Errorf("unexpected manifest type %T", value)
			}
			fn, ok := m.Functions[function]
			if !ok {
				return nil, fmt.Errorf("function %q not found", function)
			}
			return runtimeStateStore(fn)
		},
		NewInvocationID: func(function string) (invocation.InvocationID, error) {
			raw, err := newUUID()
			if err != nil {
				return "", err
			}
			return invocation.NewManualID(function, raw)
		},
		WorkflowFor: func(function string) string { return filepath.Base(compiler.WorkflowPath(function)) },
		Version:     version,
	}
}
