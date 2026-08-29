package invocation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

var (
	ErrRuntimeStoreRequired  = errors.New("invocation store is required")
	ErrRuntimeRunnerRequired = errors.New("invocation runner is required")
	ErrInvocationBusy        = errors.New("invocation is already running")
)

// StateStore is the persistence seam used by Runtime. internal/state.Memory
// implements it; keeping the interface here avoids coupling the execution
// package to one backend.
type StateStore interface {
	Get(context.Context, string, InvocationID) (*Invocation, error)
	Create(context.Context, Invocation) error
	CompareAndSwap(context.Context, Invocation, Invocation) error
	List(context.Context, string, int) ([]Invocation, error)
}

// EnvironmentRunner is implemented by the production runner and allows
// Runtime to inject invocation metadata without weakening CommandRunner's
// small compatibility seam.
type EnvironmentRunner interface {
	RunWithEnvironment(context.Context, []string, map[string]string) (int, error)
}

// Runtime coordinates logical invocation creation, lease acquisition,
// execution, and retry transitions.
type Runtime struct {
	Store  StateStore
	Runner CommandRunner
	// Executor is an alternate spelling accepted by adapters that call the
	// process seam an executor. Runner takes precedence when both are set.
	Executor CommandRunner
	Now      func() time.Time

	// Trigger and WorkflowRunID are persisted as attempt metadata.
	Trigger          string
	WorkflowRunID    int64
	LeaseOwner       string
	Owner            string
	LeaseTTL         time.Duration
	LeaseDuration    time.Duration
	RetryMaxAttempts int
	RetryBackoff     time.Duration
	Retry            RetryPolicy
	// Environment is merged into the standard GHAAS_* metadata. It is never
	// persisted in invocation state.
	Environment map[string]string
	// Logger receives non-secret lifecycle metadata. A nil logger uses slog's
	// process default and remains safe for embedders that do not configure one.
	Logger *slog.Logger
	// Sleep is injectable for deterministic retry tests.
	Sleep func(context.Context, time.Duration) error
}

// Invoke creates (when needed), claims, executes, and records one logical
// invocation. Terminal records are returned unchanged. A live lease or a
// running record without a recoverable lease returns ErrInvocationBusy.
func (r Runtime) Invoke(ctx context.Context, function string, id InvocationID, command []string) (Invocation, error) {
	if r.Store == nil {
		return Invocation{}, ErrRuntimeStoreRequired
	}
	runner := r.runner()
	if runner == nil {
		return Invocation{}, ErrRuntimeRunnerRequired
	}
	if err := validateInvocationIdentity(function, id); err != nil {
		return Invocation{}, fmt.Errorf("%w: %v", ErrInvalidInvocation, err)
	}
	if len(command) == 0 || command[0] == "" {
		return Invocation{}, fmt.Errorf("%w: command is required", ErrInvalidInvocation)
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxAttempts := r.RetryMaxAttempts
	if maxAttempts == 0 {
		maxAttempts = r.Retry.MaxAttempts
	}
	backoff := r.RetryBackoff
	if backoff == 0 {
		backoff = r.Retry.Backoff
	}
	leaseTTL := r.LeaseTTL
	if leaseTTL == 0 {
		leaseTTL = r.LeaseDuration
	}
	if maxAttempts < 0 || backoff < 0 || leaseTTL < 0 {
		return Invocation{}, fmt.Errorf("%w: retry/lease durations cannot be negative", ErrInvalidInvocation)
	}
	if ctx == nil {
		return Invocation{}, errors.New("nil context")
	}

	now := r.clock()
	current, err := r.Store.Get(ctx, function, id)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return Invocation{}, err
		}
		created := Invocation{
			SchemaVersion: 1, Function: function, ID: id,
			Status: StatusPending, CreatedAt: now.UTC(), Trigger: r.Trigger,
			WorkflowRunID: r.WorkflowRunID,
		}
		if err := created.ValidateStrict(); err != nil {
			return Invocation{}, err
		}
		if err := r.Store.Create(ctx, created); err != nil {
			if !errors.Is(err, ErrConflict) {
				return Invocation{}, err
			}
			current, err = r.Store.Get(ctx, function, id)
			if err != nil {
				return Invocation{}, err
			}
		} else {
			current = &created
		}
	}
	if current == nil {
		return Invocation{}, fmt.Errorf("%w: store returned nil record", ErrInvalidInvocation)
	}
	if err := validateRuntimeRecord(*current); err != nil {
		return Invocation{}, err
	}

	for {
		if current.Status.IsTerminal() {
			return cloneInvocation(*current), nil
		}
		if err := contextError(ctx); err != nil {
			return cloneInvocation(*current), err
		}
		if current.Status == StatusFailed {
			if maxAttempts <= 0 {
				// A failure with no retry policy is a durable outcome, not an
				// active invocation. Preserve it and report the original error.
				return cloneInvocation(*current), priorResultError(*current)
			}
			if current.Attempts >= maxAttempts {
				exhausted, exhaustErr := Exhaust(*current)
				if exhaustErr != nil {
					return Invocation{}, exhaustErr
				}
				if exhaustErr = r.Store.CompareAndSwap(context.WithoutCancel(ctx), *current, exhausted); exhaustErr != nil {
					if errors.Is(exhaustErr, ErrConflict) {
						current, exhaustErr = r.reload(context.WithoutCancel(ctx), function, id)
						if exhaustErr == nil {
							continue
						}
					}
					return Invocation{}, exhaustErr
				}
				return exhausted, priorResultError(exhausted)
			}
			pending, retryErr := Retry(*current)
			if retryErr != nil {
				return Invocation{}, retryErr
			}
			if retryErr = r.Store.CompareAndSwap(context.WithoutCancel(ctx), *current, pending); retryErr != nil {
				if errors.Is(retryErr, ErrConflict) {
					current, retryErr = r.reload(context.WithoutCancel(ctx), function, id)
					if retryErr == nil {
						continue
					}
				}
				return Invocation{}, retryErr
			}
			if retryErr = r.wait(ctx, backoff); retryErr != nil {
				return pending, retryErr
			}
			current = &pending
			continue
		}

		previous := cloneInvocation(*current)
		var running Invocation
		if current.Status == StatusPending {
			if leaseTTL > 0 {
				running, err = AcquireLease(previous, r.clock(), r.owner(), leaseTTL)
			} else {
				running, err = Start(previous, r.clock(), r.WorkflowRunID)
			}
		} else if current.Status == StatusRunning {
			if current.Lease == nil || current.LeaseActive(r.clock()) {
				return previous, ErrInvocationBusy
			}
			if maxAttempts > 0 && current.Attempts >= maxAttempts {
				failed, failErr := Fail(previous, r.clock(), InvocationResult{
					ExitCode: -1, Error: "invocation lease expired before completion",
				})
				if failErr != nil {
					return Invocation{}, failErr
				}
				if failErr = r.Store.CompareAndSwap(context.WithoutCancel(ctx), previous, failed); failErr != nil {
					if errors.Is(failErr, ErrConflict) {
						current, failErr = r.reload(context.WithoutCancel(ctx), function, id)
						if failErr == nil {
							continue
						}
					}
					return Invocation{}, failErr
				}
				exhausted, exhaustErr := Exhaust(failed)
				if exhaustErr != nil {
					return Invocation{}, exhaustErr
				}
				if exhaustErr = r.Store.CompareAndSwap(context.WithoutCancel(ctx), failed, exhausted); exhaustErr != nil {
					if errors.Is(exhaustErr, ErrConflict) {
						current, exhaustErr = r.reload(context.WithoutCancel(ctx), function, id)
						if exhaustErr == nil {
							continue
						}
					}
					return Invocation{}, exhaustErr
				}
				return exhausted, priorResultError(exhausted)
			}
			// A stale lease is recovered in place via CAS. Keep a usable
			// finite lease even when the recovering runner did not configure
			// one explicitly.
			ttl := leaseTTL
			if ttl <= 0 {
				ttl = time.Hour
			}
			running, err = AcquireLease(previous, r.clock(), r.owner(), ttl)
		}
		if err != nil {
			if errors.Is(err, ErrLeaseHeld) {
				return previous, ErrInvocationBusy
			}
			return Invocation{}, err
		}
		running.Trigger = r.Trigger
		if running.WorkflowRunID == 0 {
			running.WorkflowRunID = r.WorkflowRunID
		}
		if err := r.Store.CompareAndSwap(ctx, previous, running); err != nil {
			if errors.Is(err, ErrConflict) {
				current, err = r.reload(ctx, function, id)
				if err == nil {
					continue
				}
			}
			return Invocation{}, err
		}
		logger.Info("invocation started",
			"function", function,
			"invocation_id", id,
			"attempt", running.Attempts,
			"trigger", running.Trigger,
			"workflow_run_id", running.WorkflowRunID,
		)

		code, runErr := r.run(ctx, command, running)
		finished := cloneInvocation(running)
		completed := r.clock().UTC()
		finished.CompletedAt = &completed
		finished.Result = &InvocationResult{ExitCode: code}
		if runErr != nil {
			finished.Result.Error = runErr.Error()
		}
		if code == 0 && runErr == nil {
			if err := finished.Transition(StatusSucceeded); err != nil {
				return Invocation{}, err
			}
		} else {
			if err := finished.Transition(StatusFailed); err != nil {
				return Invocation{}, err
			}
		}
		finished.Lease = nil
		if err := validateRuntimeRecord(finished); err != nil {
			return Invocation{}, err
		}
		if err := r.Store.CompareAndSwap(context.WithoutCancel(ctx), running, finished); err != nil {
			if errors.Is(err, ErrConflict) {
				var reloadErr error
				current, reloadErr = r.reload(context.WithoutCancel(ctx), function, id)
				if reloadErr == nil {
					continue
				}
				err = reloadErr
			}
			return Invocation{}, err
		}
		logger.Info("invocation finished",
			"function", function,
			"invocation_id", id,
			"attempt", finished.Attempts,
			"status", finished.Status,
			"exit_code", finished.Result.ExitCode,
			"workflow_run_id", finished.WorkflowRunID,
		)
		if finished.Status == StatusSucceeded {
			return finished, nil
		}

		if maxAttempts > 0 {
			if running.Attempts < maxAttempts {
				pending, retryErr := Retry(finished)
				if retryErr != nil {
					return Invocation{}, retryErr
				}
				if retryErr = r.Store.CompareAndSwap(context.WithoutCancel(ctx), finished, pending); retryErr != nil {
					if errors.Is(retryErr, ErrConflict) {
						current, retryErr = r.reload(context.WithoutCancel(ctx), function, id)
						if retryErr == nil {
							continue
						}
					}
					return Invocation{}, retryErr
				}
				if retryErr = r.wait(ctx, backoff); retryErr != nil {
					return pending, retryErr
				}
				current = &pending
				continue
			}
			exhausted, exhaustErr := Exhaust(finished)
			if exhaustErr != nil {
				return Invocation{}, exhaustErr
			}
			if exhaustErr = r.Store.CompareAndSwap(context.WithoutCancel(ctx), finished, exhausted); exhaustErr != nil {
				if errors.Is(exhaustErr, ErrConflict) {
					current, exhaustErr = r.reload(context.WithoutCancel(ctx), function, id)
					if exhaustErr == nil {
						continue
					}
				}
				return Invocation{}, exhaustErr
			}
			if runErr == nil {
				runErr = fmt.Errorf("command exited with status %d", code)
			}
			return exhausted, runErr
		}
		if runErr == nil {
			runErr = fmt.Errorf("command exited with status %d", code)
		}
		return finished, runErr
	}
}

// InvokeCommand is a descriptive alias for Invoke.
func (r Runtime) InvokeCommand(ctx context.Context, function string, id InvocationID, command []string) (Invocation, error) {
	return r.Invoke(ctx, function, id, command)
}

func (r Runtime) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}
func (r Runtime) owner() string {
	if r.LeaseOwner != "" {
		return r.LeaseOwner
	}
	if r.Owner != "" {
		return r.Owner
	}
	if r.WorkflowRunID > 0 {
		return fmt.Sprintf("workflow-run-%d", r.WorkflowRunID)
	}
	return "ghaas-runtime"
}

// reload fetches and validates the winner after a compare-and-swap conflict.
// A conflict is expected during duplicate workflow delivery; malformed state
// must not be retried as if it were a transient race.
func (r Runtime) reload(ctx context.Context, function string, id InvocationID) (*Invocation, error) {
	current, err := r.Store.Get(ctx, function, id)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, fmt.Errorf("%w: store returned nil record", ErrInvalidInvocation)
	}
	if err := validateRuntimeRecord(*current); err != nil {
		return nil, err
	}
	return current, nil
}
func validateRuntimeRecord(i Invocation) error {
	if i.SchemaVersion == 1 {
		return i.ValidateStrict()
	}
	return i.Validate()
}

func (r Runtime) wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return contextError(ctx)
	}
	if r.Sleep != nil {
		return r.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r Runtime) run(ctx context.Context, command []string, current Invocation) (int, error) {
	args := append([]string(nil), command...)
	runner := r.runner()
	if envRunner, ok := runner.(EnvironmentRunner); ok {
		env := InvocationEnvironment(current)
		for k, v := range r.Environment {
			env[k] = v
		}
		return envRunner.RunWithEnvironment(ctx, args, env)
	}
	return runner.Run(ctx, args)
}

func (r Runtime) runner() CommandRunner {
	if r.Runner != nil {
		return r.Runner
	}
	return r.Executor
}

func priorResultError(i Invocation) error {
	if i.Result == nil {
		return ErrInvocationComplete
	}
	if i.Result.Error != "" {
		return errors.New(i.Result.Error)
	}
	if i.Result.ExitCode != 0 {
		return fmt.Errorf("command exited with status %d", i.Result.ExitCode)
	}
	return ErrInvocationComplete
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
