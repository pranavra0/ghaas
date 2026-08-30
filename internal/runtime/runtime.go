// Package runtime executes a manifest function in the current process environment.
// It is the small entrypoint used by generated workflows. State orchestration is
// optional for local development and injected for production.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pranavra0/ghaas/internal/invocation"
	"github.com/pranavra0/ghaas/internal/state"
	"github.com/pranavra0/ghaas/pkg/manifest"
)

const (
	DefaultTimeout    = 15 * time.Minute
	stateWriteTimeout = 10 * time.Second
)

// Loader reads and validates a manifest. The main command supplies config.Load;
// keeping it injectable makes runtime invocation deterministic in tests.
type Loader func(string) (manifest.Manifest, error)

// Store is the runtime-facing subset of the durable state contract. Keeping
// this seam structural permits deterministic state-machine fakes in tests while
// the production implementation remains internal/state.Store.
type Store interface {
	Ensure(context.Context, string, string, ...int) (state.Invocation, error)
	Acquire(context.Context, string, string, string) (state.Invocation, state.Lease, error)
	Renew(context.Context, state.Lease) (state.Lease, error)
	BindProvider(context.Context, state.Lease, int64, int, string) (state.Invocation, error)
	Complete(context.Context, state.Lease, bool, ...state.Result) (state.Invocation, error)
}

// Options controls one runtime invocation.
type Options struct {
	ManifestPath string
	LoadManifest Loader
	Runner       invocation.Runner
	Environment  []string
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer

	// Store enables durable orchestration. A nil Store keeps the local/dev
	// direct-execution behavior used by development and unit tests.
	Store Store
	// Owner identifies this worker for lease ownership. If empty, a stable
	// provider-based owner is derived from the environment.
	Owner string
	// Clock and Sleep are injectable to make state transitions deterministic.
	Clock func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// Invoke loads function from the manifest, merges its environment over the
// ambient environment, and executes argv directly. The returned code is the
// child exit status. A non-zero child status is not itself an error.
func Invoke(ctx context.Context, function string, options Options) (int, error) {
	if ctx == nil {
		return 1, errors.New("nil context")
	}
	function = strings.TrimSpace(function)
	if function == "" {
		return 1, errors.New("function name is required")
	}
	path := options.ManifestPath
	if path == "" {
		path = "ghaas.yaml"
	}
	if options.LoadManifest == nil {
		return 1, errors.New("manifest loader is not configured")
	}
	m, err := options.LoadManifest(path)
	if err != nil {
		return 1, err
	}
	fn, ok := m.Functions[function]
	if !ok {
		return 1, fmt.Errorf("function %q not found", function)
	}
	if len(fn.Command) == 0 || strings.TrimSpace(fn.Command[0]) == "" {
		return 1, fmt.Errorf("function %q command is required", function)
	}

	ambient := options.Environment
	if ambient == nil {
		ambient = options.Runner.Env
	}
	if ambient == nil {
		ambient = os.Environ()
	}
	id, err := invocationIDFor(function, ambient, options.Store != nil)
	if err != nil {
		return 1, err
	}

	if options.Store != nil {
		return invokeDurable(ctx, function, id, m, fn, ambient, options)
	}
	return invokeDirect(ctx, function, id, m, fn, ambient, options)
}

func invokeDirect(ctx context.Context, function, id string, m manifest.Manifest, fn manifest.Function, ambient []string, options Options) (int, error) {
	timeout := time.Duration(fn.EffectiveTimeout(m.Defaults))
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	metadata := metadataEnvironment(ambient, function, id, 1)
	code, runErr := runCommand(runContext, fn.Command, fn.Environment, metadata, options)
	if runErr == nil {
		return code, nil
	}
	if runContext.Err() != nil {
		return 1, classifyContextError(function, runContext.Err())
	}
	if code >= 0 {
		return code, nil
	}
	return 1, fmt.Errorf("function %q: %w", function, runErr)
}

func invokeDurable(ctx context.Context, function, id string, m manifest.Manifest, fn manifest.Function, ambient []string, options Options) (int, error) {
	maxAttempts := fn.EffectiveMaxAttempts(m.Defaults)
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	inv, err := options.Store.Ensure(ctx, function, id, maxAttempts)
	if err != nil {
		return 1, err
	}
	if strings.EqualFold(string(inv.Status), "succeeded") {
		return 0, nil
	}
	owner := options.Owner
	if owner == "" {
		owner = workerOwner(ambient)
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = contextSleep
	}
	for {
		current, lease, err := options.Store.Acquire(ctx, function, id, owner)
		if err != nil {
			if errors.Is(err, state.ErrLeaseHeld) {
				// Do not poll or reclaim an active lease here. Its holder may
				// still be running the command, and a bounded wait cannot
				// distinguish that case from a dead worker. Reacquiring after
				// expiry could launch a second external effect while the first
				// command is still running; fencing only protects durable
				// state writes. Leave recovery to a later invocation after the
				// lease expires rather than risk duplicate command execution.
				return 1, err
			}
			return 1, err
		}
		if strings.EqualFold(string(current.Status), "succeeded") {
			return 0, nil
		}
		if runID, runAttempt := providerRun(ambient); runID > 0 &&
			(current.Provider == nil || current.Provider.RunID != runID ||
				current.Provider.RunAttempt != runAttempt || current.Provider.Reason != providerReason(ambient)) {
			bindCtx, bindCancel := newStateWriteContext(ctx)
			current, err = options.Store.BindProvider(bindCtx, lease, runID, runAttempt, providerReason(ambient))
			bindCancel()
			if err != nil {
				return 1, err
			}
		}

		renewal := startLeaseRenewal(ctx, options.Store, lease, options.Clock)
		timeout := time.Duration(fn.EffectiveTimeout(m.Defaults))
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		runContext, cancel := context.WithTimeout(ctx, timeout)
		metadata := metadataEnvironment(ambient, function, id, current.Attempts)
		code, runErr := runCommand(runContext, fn.Command, fn.Environment, metadata, options)
		contextErr := runContext.Err()
		cancel()
		renewedLease, renewErr := renewal.Stop()
		lease = renewedLease

		success := runErr == nil && contextErr == nil && code == 0
		result := state.Result{ExitCode: code}
		if contextErr != nil {
			result.Error = contextErr.Error()
		} else if runErr != nil {
			result.Error = runErr.Error()
		}

		// Completion is a fenced write and must get a bounded context that is
		// independent of the command's parent. A canceled command still owns a
		// usable lease until its expiry, and leaving that lease running makes the
		// durable invocation unrecoverable until another worker reclaims it.
		if leaseUsable(lease, options.Clock) {
			completeCtx, completeCancel := newStateWriteContext(ctx)
			completed, completeErr := options.Store.Complete(completeCtx, lease, success, result)
			completeCancel()
			if completeErr != nil {
				// The provider may have applied a write before returning an
				// error. Preserve that unknown-effect outcome: never retry the
				// completion (or the command) from here.
				return 1, completeErr
			}
			if success {
				return 0, nil
			}
			if strings.EqualFold(string(completed.Status), "failed") {
				if err := sleep(ctx, time.Duration(fn.EffectiveBackoff(m.Defaults))); err != nil {
					return 1, classifyContextError(function, err)
				}
				continue
			}
			if contextErr != nil {
				return 1, classifyContextError(function, contextErr)
			}
			if runErr != nil && code < 0 {
				return 1, fmt.Errorf("function %q: %w", function, runErr)
			}
			return code, nil
		}

		// A renewal failure is actionable only when the fenced lease can no
		// longer be used. Do not report it for a successfully completed command
		// if its completion was durably recorded above.
		if renewErr != nil {
			return 1, fmt.Errorf("function %q lease renewal: %w", function, renewErr)
		}
		if contextErr != nil {
			return 1, classifyContextError(function, contextErr)
		}
		if runErr != nil && code < 0 {
			return 1, fmt.Errorf("function %q: %w", function, runErr)
		}
		return code, nil
	}
}

func runCommand(ctx context.Context, command []string, functionEnv, metadata map[string]string, options Options) (int, error) {
	runner := options.Runner
	if options.Stdin != nil {
		runner.Stdin = options.Stdin
	}
	if options.Stdout != nil {
		runner.Stdout = options.Stdout
	}
	if options.Stderr != nil {
		runner.Stderr = options.Stderr
	}
	base := options.Environment
	if base == nil {
		base = runner.Env
	}
	if base == nil {
		base = os.Environ()
	}
	runner.Env = stripStateCredentials(base)
	values := merge(functionEnv, metadata)
	for key := range values {
		if isStateCredential(key) {
			delete(values, key)
		}
	}
	return runner.RunWithEnvironment(ctx, command, values)
}

func isStateCredential(key string) bool {
	return key == "GITHUB_TOKEN" || key == "GH_TOKEN" || key == "GHAAS_STATE_TOKEN" ||
		strings.HasPrefix(key, "GHAAS_STATE_")
}

func stripStateCredentials(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if !isStateCredential(key) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func newStateWriteContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), stateWriteTimeout)
}
func leaseUsable(lease state.Lease, now func() time.Time) bool {
	if now == nil {
		now = time.Now
	}
	return lease.ExpiresAt.After(now())
}

func classifyContextError(function string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("function %q timed out: %w", function, err)
	}
	return fmt.Errorf("function %q canceled: %w", function, err)
}

func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type leaseRenewal struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	lease  state.Lease
	err    error
	done   chan struct{}
}

func startLeaseRenewal(ctx context.Context, store Store, lease state.Lease, now func() time.Time) *leaseRenewal {
	if now == nil {
		now = time.Now
	}
	renewCtx, cancel := context.WithCancel(ctx)
	r := &leaseRenewal{cancel: cancel, lease: lease, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		current := lease
		for {
			delay := current.ExpiresAt.Sub(now()) / 2
			if delay <= 0 {
				delay = 10 * time.Millisecond
			} else if delay > time.Minute {
				delay = time.Minute
			}
			timer := time.NewTimer(delay)
			select {
			case <-renewCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			next, err := store.Renew(renewCtx, current)
			if err != nil {
				if renewCtx.Err() == nil {
					r.mu.Lock()
					r.err = err
					r.mu.Unlock()
				}
				return
			}
			current = next
			r.mu.Lock()
			r.lease = next
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *leaseRenewal) Stop() (state.Lease, error) {
	r.cancel()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease, r.err
}

func workerOwner(ambient []string) string {
	if owner := strings.TrimSpace(environmentValue(ambient, "GHAAS_OWNER")); owner != "" {
		return owner
	}
	if runID := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ID")); runID != "" {
		return "github-run/" + runID
	}
	return "ghaas-runtime"
}

func invocationID(function string, ambient []string) (string, error) {
	return invocationIDFor(function, ambient, false)
}

func invocationIDFor(function string, ambient []string, durable bool) (string, error) {
	event := strings.TrimSpace(environmentValue(ambient, "GITHUB_EVENT_NAME"))
	if event == "schedule" {
		runID := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ID"))
		id, err := strconv.ParseInt(runID, 10, 64)
		if err != nil || id <= 0 {
			return "", fmt.Errorf("invalid GITHUB_RUN_ID %q", runID)
		}
		return function + "/" + runID, nil
	}
	value := strings.TrimSpace(environmentValue(ambient, "GHAAS_INVOCATION_ID"))
	if value != "" {
		if strings.ContainsAny(value, "\r\n") {
			return "", errors.New("invalid GHAAS_INVOCATION_ID")
		}
		prefix := function + "/"
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
		}
		if value == "" || strings.Contains(value, "/") {
			return "", fmt.Errorf("invalid GHAAS_INVOCATION_ID %q", value)
		}
		return prefix + value, nil
	}
	if durable {
		return "", errors.New("GHAAS_INVOCATION_ID is required for manual invocation")
	}
	if value := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ID")); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return "", fmt.Errorf("invalid GITHUB_RUN_ID %q", value)
		}
		return function + "/" + value, nil
	}
	return function + "/local", nil
}

func metadataEnvironment(ambient []string, function, id string, attempts ...int) map[string]string {
	logicalAttempt := 1
	if len(attempts) > 0 {
		logicalAttempt = attempts[0]
	}
	trigger := strings.TrimSpace(environmentValue(ambient, "GITHUB_EVENT_NAME"))
	switch trigger {
	case "workflow_dispatch":
		trigger = "manual"
	case "schedule":
		trigger = "schedule"
	case "":
		trigger = "manual"
	}
	if logicalAttempt <= 0 {
		logicalAttempt = 1
	}
	runAttempt := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ATTEMPT"))
	if runAttempt == "" {
		runAttempt = "1"
	}
	runID := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ID"))
	reason := strings.TrimSpace(environmentValue(ambient, "GHAAS_ATTEMPT_REASON"))
	if reason == "" {
		reason = strings.TrimSpace(environmentValue(ambient, "GITHUB_EVENT_NAME"))
	}
	return map[string]string{
		"GHAAS_FUNCTION":             function,
		"GHAAS_INVOCATION_ID":        id,
		"GHAAS_ATTEMPT":              strconv.Itoa(logicalAttempt),
		"GHAAS_TRIGGER":              trigger,
		"GHAAS_WORKFLOW_RUN_ID":      runID,
		"GHAAS_WORKFLOW_RUN_ATTEMPT": runAttempt,
		"GHAAS_ATTEMPT_REASON":       reason,
	}
}

func providerRun(ambient []string) (int64, int) {
	runID, err := strconv.ParseInt(strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ID")), 10, 64)
	if err != nil || runID <= 0 {
		return 0, 0
	}
	runAttempt, err := strconv.Atoi(strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ATTEMPT")))
	if err != nil || runAttempt <= 0 {
		runAttempt = 1
	}
	return runID, runAttempt
}

func providerReason(ambient []string) string {
	if reason := strings.TrimSpace(environmentValue(ambient, "GHAAS_ATTEMPT_REASON")); reason != "" {
		return reason
	}
	return strings.TrimSpace(environmentValue(ambient, "GITHUB_EVENT_NAME"))
}

func environmentValue(environment []string, key string) string {
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

func merge(manifestEnv, metadata map[string]string) map[string]string {
	values := make(map[string]string, len(manifestEnv)+len(metadata))
	for key, value := range manifestEnv {
		values[key] = value
	}
	for key, value := range metadata {
		values[key] = value
	}
	return values
}
