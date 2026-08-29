// Package invocation contains logical invocation identity and lifecycle helpers.
package invocation

import (
	"errors"
	"fmt"
	"time"
)

// InvocationID identifies one logical execution of a function.
// It is deliberately a string so scheduled IDs remain human-readable.
type InvocationID string

// InvocationStatus is the lifecycle state of a logical invocation.
type InvocationStatus string

const (
	StatusPending   InvocationStatus = "pending"
	StatusRunning   InvocationStatus = "running"
	StatusSucceeded InvocationStatus = "succeeded"
	StatusFailed    InvocationStatus = "failed"
	StatusExhausted InvocationStatus = "exhausted"

	// Short aliases make the state machine convenient to use without weakening
	// the serialized values above.
	Pending   = StatusPending
	Running   = StatusRunning
	Succeeded = StatusSucceeded
	Failed    = StatusFailed
	Exhausted = StatusExhausted
)

// Status is a concise alias for InvocationStatus.
type Status = InvocationStatus

const (
	InvocationPending   = StatusPending
	InvocationRunning   = StatusRunning
	InvocationSucceeded = StatusSucceeded
	InvocationFailed    = StatusFailed
	InvocationExhausted = StatusExhausted
)

var (
	ErrInvalidTransition = errors.New("invalid invocation state transition")
	ErrInvalidInvocation = errors.New("invalid invocation")
	// ErrNotFound and ErrConflict are shared by state backends and orchestration
	// seams so callers can use errors.Is without importing a concrete backend.
	ErrNotFound      = errors.New("state not found")
	ErrConflict      = errors.New("state conflict")
	ErrStateNotFound = ErrNotFound
	ErrStateConflict = ErrConflict
)

// InvocationResult records the process result without retaining command output.
type InvocationResult struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// Invocation is the state record for a logical invocation.
type Invocation struct {
	SchemaVersion int              `json:"schema_version"`
	Function      string           `json:"function"`
	ID            InvocationID     `json:"invocation_id"`
	Status        InvocationStatus `json:"status"`
	Attempts      int              `json:"attempts"`

	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	WorkflowRunID int64             `json:"workflow_run_id,omitempty"`
	Result        *InvocationResult `json:"result,omitempty"`
}

// Record is an alternate name for the state record used by callers that do not
// need to distinguish it from an invocation. It is an alias, not a second type.
type Record = Invocation

// IsTerminal reports whether no further execution is legal for the record.
func (s InvocationStatus) IsTerminal() bool {
	return s == StatusSucceeded || s == StatusExhausted
}

// Valid reports whether s is one of the v0.1 lifecycle statuses.
func (s InvocationStatus) Valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusSucceeded, StatusFailed, StatusExhausted:
		return true
	default:
		return false
	}
}

// CanTransition reports whether moving from the record's current status to
// next is legal. Retry policy is intentionally not part of this pure helper:
// failed may be returned to pending by an orchestrator that elects to retry.
func (i Invocation) CanTransition(next InvocationStatus) bool {
	switch i.Status {
	case StatusPending:
		return next == StatusRunning
	case StatusRunning:
		return next == StatusSucceeded || next == StatusFailed
	case StatusFailed:
		return next == StatusPending || next == StatusExhausted
	default:
		return false
	}
}

// CanTransition reports whether the state edge is legal without constructing a record.
func CanTransition(from, to InvocationStatus) bool {
	return (Invocation{Status: from}).CanTransition(to)
}

// ValidateTransition returns ErrInvalidTransition for an illegal edge.
func ValidateTransition(from, to InvocationStatus) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

// Transition applies one legal state transition.
func (i *Invocation) Transition(next InvocationStatus) error {
	if i == nil {
		return fmt.Errorf("%w: nil record", ErrInvalidInvocation)
	}
	if !next.Valid() || !i.Status.Valid() || !i.CanTransition(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, i.Status, next)
	}
	i.Status = next
	return nil
}

// Start transitions a pending invocation to running and records its attempt.
func Start(i Invocation, now time.Time, workflowRunID int64) (Invocation, error) {
	i = cloneInvocation(i)
	if err := i.Transition(StatusRunning); err != nil {
		return Invocation{}, err
	}
	i.Attempts++
	now = now.UTC()
	i.StartedAt = &now
	i.WorkflowRunID = workflowRunID
	return i, nil
}

// Succeed records a successful process result and completes a running invocation.
func Succeed(i Invocation, now time.Time, result InvocationResult) (Invocation, error) {
	i = cloneInvocation(i)
	if result.ExitCode != 0 {
		return Invocation{}, fmt.Errorf("%w: successful result has exit code %d", ErrInvalidInvocation, result.ExitCode)
	}
	if err := i.Transition(StatusSucceeded); err != nil {
		return Invocation{}, err
	}
	now = now.UTC()
	i.CompletedAt = &now
	i.Result = &result
	return i, nil
}

// Fail records a failed process result and completes a running invocation.
func Fail(i Invocation, now time.Time, result InvocationResult) (Invocation, error) {
	i = cloneInvocation(i)
	if err := i.Transition(StatusFailed); err != nil {
		return Invocation{}, err
	}
	now = now.UTC()
	i.CompletedAt = &now
	i.Result = &result
	return i, nil
}

// Retry returns a failed invocation to pending for a later attempt.
func Retry(i Invocation) (Invocation, error) {
	i = cloneInvocation(i)
	if err := i.Transition(StatusPending); err != nil {
		return Invocation{}, err
	}
	i.StartedAt = nil
	i.CompletedAt = nil
	return i, nil
}

func cloneInvocation(v Invocation) Invocation {
	if v.StartedAt != nil {
		t := *v.StartedAt
		v.StartedAt = &t
	}
	if v.CompletedAt != nil {
		t := *v.CompletedAt
		v.CompletedAt = &t
	}
	if v.Result != nil {
		r := *v.Result
		v.Result = &r
	}
	return v
}

// Validate checks invariants needed by state backends. Zero SchemaVersion is
// accepted for compatibility with callers constructing records by hand; newly
// created records should use SchemaVersion 1.
func (i Invocation) Validate() error {
	if i.SchemaVersion != 0 && i.SchemaVersion != 1 {
		return fmt.Errorf("%w: unsupported schema version %d", ErrInvalidInvocation, i.SchemaVersion)
	}
	if i.Function == "" {
		return fmt.Errorf("%w: function is required", ErrInvalidInvocation)
	}
	if i.ID == "" {
		return fmt.Errorf("%w: invocation ID is required", ErrInvalidInvocation)
	}
	if !i.Status.Valid() {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidInvocation, i.Status)
	}
	if i.Attempts < 0 {
		return fmt.Errorf("%w: attempts cannot be negative", ErrInvalidInvocation)
	}
	return nil
}
