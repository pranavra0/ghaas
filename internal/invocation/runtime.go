package invocation

import (
	"context"
	"errors"
	"fmt"
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

// Runtime coordinates one command execution and its v0.1 state transitions.
// It deliberately has no leases or retry policy; those belong to a later
// durable-state implementation.
type Runtime struct {
	Store  StateStore
	Runner CommandRunner
	Now    func() time.Time
}

// Invoke creates (when needed), claims, executes, and records one logical
// invocation. A terminal record is returned unchanged; an already-running
// record returns ErrInvocationBusy. A command failure is recorded as failed
// and returned as the error alongside its record; callers can inspect the
// exit code in the record's Result.
func (r Runtime) Invoke(ctx context.Context, function string, id InvocationID, command []string) (Invocation, error) {
	if r.Store == nil {
		return Invocation{}, ErrRuntimeStoreRequired
	}
	if r.Runner == nil {
		return Invocation{}, ErrRuntimeRunnerRequired
	}
	if function == "" || id == "" {
		return Invocation{}, fmt.Errorf("%w: function and ID are required", ErrInvalidInvocation)
	}
	if len(command) == 0 {
		return Invocation{}, fmt.Errorf("%w: command is required", ErrInvalidInvocation)
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
		current = &Invocation{SchemaVersion: 1, Function: function, ID: id, Status: StatusPending, CreatedAt: now}
		if err := r.Store.Create(ctx, *current); err != nil {
			// A concurrent creator is harmless: use the record it won.
			if !errors.Is(err, ErrConflict) {
				return Invocation{}, err
			}
			current, err = r.Store.Get(ctx, function, id)
			if err != nil {
				return Invocation{}, err
			}
		}
	}
	if current == nil {
		return Invocation{}, fmt.Errorf("%w: store returned nil record", ErrInvalidInvocation)
	}
	if current.Status.IsTerminal() {
		return cloneInvocation(*current), nil
	}
	if current.Status != StatusPending {
		return cloneInvocation(*current), ErrInvocationBusy
	}

	running := cloneInvocation(*current)
	if err := running.Transition(StatusRunning); err != nil {
		return Invocation{}, err
	}
	running.Attempts++
	started := r.clock()
	running.StartedAt = &started
	if err := r.Store.CompareAndSwap(ctx, *current, running); err != nil {
		return Invocation{}, err
	}

	code, runErr := r.Runner.Run(ctx, append([]string(nil), command...))
	completed := r.clock()
	finished := cloneInvocation(running)
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
	if err := r.Store.CompareAndSwap(context.WithoutCancel(ctx), running, finished); err != nil {
		return Invocation{}, err
	}
	if runErr == nil && code != 0 {
		runErr = fmt.Errorf("command exited with status %d", code)
	}
	return finished, runErr
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
