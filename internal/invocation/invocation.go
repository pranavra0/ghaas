// Package invocation contains logical invocation identity and lifecycle helpers.
package invocation

import (
	"errors"
	"fmt"
	"strings"
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

// Trigger identifies the event which caused an invocation attempt.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerUnknown  = ""
)

// Lease prevents two runners from executing the same logical invocation at
// once. A lease is advisory and can be recovered after ExpiresAt.
type Lease struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InvocationLease is retained as a descriptive spelling for Lease.
type InvocationLease = Lease
type LeaseInfo = Lease

var (
	ErrInvalidTransition = errors.New("invalid invocation state transition")
	ErrInvalidInvocation = errors.New("invalid invocation")
	// ErrNotFound and ErrConflict are shared by state backends and orchestration
	// seams so callers can use errors.Is without importing a concrete backend.
	ErrNotFound           = errors.New("state not found")
	ErrConflict           = errors.New("state conflict")
	ErrStateNotFound      = ErrNotFound
	ErrStateConflict      = ErrConflict
	ErrLeaseHeld          = errors.New("invocation lease is held")
	ErrInvocationComplete = errors.New("invocation is complete")
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

	WorkflowRunID int64 `json:"workflow_run_id,omitempty"`
	// Trigger and Lease are metadata used to make duplicate and recovery
	// decisions. They are omitted from old records when not available.
	Trigger string            `json:"trigger,omitempty"`
	Lease   *Lease            `json:"lease,omitempty"`
	Result  *InvocationResult `json:"result,omitempty"`
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
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	if err := i.Transition(StatusRunning); err != nil {
		return Invocation{}, err
	}
	i.Attempts++
	now = now.UTC()
	i.StartedAt = &now
	i.CompletedAt = nil
	i.WorkflowRunID = workflowRunID
	i.Lease = nil
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	return i, nil
}

// Succeed records a successful process result and completes a running invocation.
func Succeed(i Invocation, now time.Time, result InvocationResult) (Invocation, error) {
	i = cloneInvocation(i)
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	if result.ExitCode != 0 {
		return Invocation{}, fmt.Errorf("%w: successful result has exit code %d", ErrInvalidInvocation, result.ExitCode)
	}
	if err := i.Transition(StatusSucceeded); err != nil {
		return Invocation{}, err
	}
	now = now.UTC()
	i.CompletedAt = &now
	i.Result = &result
	i.Lease = nil
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	return i, nil
}

// Fail records a failed process result and completes a running invocation.
func Fail(i Invocation, now time.Time, result InvocationResult) (Invocation, error) {
	i = cloneInvocation(i)
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	if err := i.Transition(StatusFailed); err != nil {
		return Invocation{}, err
	}
	now = now.UTC()
	i.CompletedAt = &now
	i.Result = &result
	i.Lease = nil
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	return i, nil
}

// Exhaust marks a failed invocation as permanently exhausted.
func Exhaust(i Invocation) (Invocation, error) {
	i = cloneInvocation(i)
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	if err := i.Transition(StatusExhausted); err != nil {
		return Invocation{}, err
	}
	i.Lease = nil
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	return i, nil
}

// Retry returns a failed invocation to pending for a later attempt.
func Retry(i Invocation) (Invocation, error) {
	i = cloneInvocation(i)
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	if err := i.Transition(StatusPending); err != nil {
		return Invocation{}, err
	}
	i.StartedAt = nil
	i.CompletedAt = nil
	i.Lease = nil
	i.Result = nil
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	return i, nil
}

// RetryPolicy controls execution retries. MaxAttempts includes the first
// execution (a value of one therefore means no retry).
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

// RetryDecision is the state-machine decision after a failed attempt.
type RetryDecision string

const (
	RetryAgain             RetryDecision = "retry"
	RetryExhaust           RetryDecision = "exhausted"
	RetryDecisionRetry                   = RetryAgain
	RetryDecisionExhausted               = RetryExhaust
)

// ShouldRetry reports whether another attempt is allowed by policy.
func ShouldRetry(i Invocation, maxAttempts int) bool {
	return i.Status == StatusFailed && maxAttempts > 0 && i.Attempts < maxAttempts
}

// ShouldRetry is the method form of the package helper.
func (i Invocation) ShouldRetry(maxAttempts int) bool { return ShouldRetry(i, maxAttempts) }

// DecideRetry chooses retry or exhaustion for a failed invocation. Policies
// with no configured maximum retain the historical failed state.
func DecideRetry(i Invocation, maxAttempts int) RetryDecision {
	if ShouldRetry(i, maxAttempts) {
		return RetryAgain
	}
	return RetryExhaust
}

// DecideRetry is the method form of the package helper.
func (i Invocation) DecideRetry(maxAttempts int) RetryDecision {
	return DecideRetry(i, maxAttempts)
}

// RetryDecisionFor is an explicit alias useful to orchestrators.
func RetryDecisionFor(i Invocation, maxAttempts int) RetryDecision {
	return DecideRetry(i, maxAttempts)
}

// AcquireLease claims an invocation for owner. Active leases are never
// reacquired, including by their current owner; callers must treat the
// returned ErrLeaseHeld as a duplicate runner. An expired running lease can be
// recovered and starts a new attempt. A zero TTL is rejected so accidental
// immortal leases are avoided.
func AcquireLease(i Invocation, now time.Time, owner string, ttl time.Duration) (Invocation, error) {
	i = cloneInvocation(i)
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	if i.Status.IsTerminal() {
		return Invocation{}, ErrInvocationComplete
	}
	if owner == "" {
		return Invocation{}, fmt.Errorf("%w: lease owner is required", ErrInvalidInvocation)
	}
	if ttl <= 0 {
		return Invocation{}, fmt.Errorf("%w: lease duration must be positive", ErrInvalidInvocation)
	}
	now = now.UTC()
	if i.Lease != nil && i.LeaseActive(now) {
		return Invocation{}, ErrLeaseHeld
	}
	if i.Status == StatusRunning {
		// A stale runner is recovered in place. Its old attempt is no longer
		// considered live and this claim represents the next attempt.
		i.Attempts++
	} else {
		if err := i.Transition(StatusRunning); err != nil {
			return Invocation{}, err
		}
		i.Attempts++
	}
	i.StartedAt = &now
	i.CompletedAt = nil
	i.Result = nil
	expires := now.Add(ttl)
	i.Lease = &Lease{Owner: owner, ExpiresAt: expires}
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	return i, nil
}

// LeaseActive reports whether an invocation has a lease which has not expired.
func (i Invocation) LeaseActive(now time.Time) bool {
	return i.Lease != nil && i.Lease.Owner != "" && i.Lease.ExpiresAt.After(now.UTC())
}

// LeaseExpired reports whether a lease exists and is no longer active.
func (i Invocation) LeaseExpired(now time.Time) bool {
	return i.Lease != nil && !i.LeaseActive(now)
}

// HasActiveLease is an explicit alias for LeaseActive.
func (i Invocation) HasActiveLease(now time.Time) bool { return i.LeaseActive(now) }

// ReleaseLease drops a lease held by owner. It is safe to release an expired
// lease; releasing another active owner's lease is rejected.
func ReleaseLease(i Invocation, owner string) (Invocation, error) {
	return ReleaseLeaseAt(i, owner, time.Now().UTC())
}

// ReleaseLeaseAt is the deterministic form of ReleaseLease.
func ReleaseLeaseAt(i Invocation, owner string, now time.Time) (Invocation, error) {
	i = cloneInvocation(i)
	if err := validateRecordBoundary(i); err != nil {
		return Invocation{}, err
	}
	if i.Lease == nil {
		return i, nil
	}
	if i.Lease.Owner != owner && i.Lease.ExpiresAt.After(now.UTC()) {
		return Invocation{}, ErrLeaseHeld
	}
	i.Lease = nil
	return i, nil
}

// TryAcquireLease is an alias for AcquireLease.
func TryAcquireLease(i Invocation, now time.Time, owner string, ttl time.Duration) (Invocation, error) {
	return AcquireLease(i, now, owner, ttl)
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
	if v.Lease != nil {
		l := *v.Lease
		v.Lease = &l
	}
	if v.Result != nil {
		r := *v.Result
		v.Result = &r
	}
	return v
}

// Validate checks record identity and schema-independent invariants. Schema
// zero remains accepted for callers that construct legacy records by hand;
// use ValidateStrict when enforcing the complete schema version one lifecycle.
func (i Invocation) Validate() error {
	if i.SchemaVersion != 0 && i.SchemaVersion != 1 {
		return fmt.Errorf("%w: unsupported schema version %d", ErrInvalidInvocation, i.SchemaVersion)
	}
	if err := validateInvocationIdentity(i.Function, i.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInvocation, err)
	}
	if !i.Status.Valid() {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidInvocation, i.Status)
	}
	if i.Attempts < 0 {
		return fmt.Errorf("%w: attempts cannot be negative", ErrInvalidInvocation)
	}
	if i.WorkflowRunID < 0 {
		return fmt.Errorf("%w: workflow_run_id cannot be negative", ErrInvalidInvocation)
	}
	if i.SchemaVersion == 0 {
		return nil
	}
	if i.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at is required", ErrInvalidInvocation)
	}
	if i.StartedAt != nil && i.StartedAt.Before(i.CreatedAt) {
		return fmt.Errorf("%w: started_at precedes created_at", ErrInvalidInvocation)
	}
	if i.CompletedAt != nil {
		if i.CompletedAt.Before(i.CreatedAt) {
			return fmt.Errorf("%w: completed_at precedes created_at", ErrInvalidInvocation)
		}
		if i.StartedAt != nil && i.CompletedAt.Before(*i.StartedAt) {
			return fmt.Errorf("%w: completed_at precedes started_at", ErrInvalidInvocation)
		}
	}
	if i.Lease != nil {
		if i.Status != StatusRunning || strings.TrimSpace(i.Lease.Owner) == "" || i.Lease.ExpiresAt.IsZero() {
			return fmt.Errorf("%w: invalid lease metadata", ErrInvalidInvocation)
		}
		if i.StartedAt != nil && !i.Lease.ExpiresAt.After(*i.StartedAt) {
			return fmt.Errorf("%w: lease expires before attempt starts", ErrInvalidInvocation)
		}
	}
	if i.Result != nil && i.Status == StatusSucceeded && i.Result.ExitCode != 0 {
		return fmt.Errorf("%w: succeeded result has non-zero exit code", ErrInvalidInvocation)
	}
	return nil
}

func (i Invocation) validateLifecycle() error {
	switch i.Status {
	case StatusPending:
		if i.StartedAt != nil || i.CompletedAt != nil || i.Result != nil || i.Lease != nil {
			return fmt.Errorf("%w: pending record has execution metadata", ErrInvalidInvocation)
		}
	case StatusRunning:
		if i.Attempts < 1 || i.StartedAt == nil || i.CompletedAt != nil || i.Result != nil {
			return fmt.Errorf("%w: running record has invalid metadata", ErrInvalidInvocation)
		}
	case StatusSucceeded, StatusExhausted:
		if i.Attempts < 1 || i.StartedAt == nil || i.CompletedAt == nil || i.Result == nil || i.Lease != nil {
			return fmt.Errorf("%w: terminal record has incomplete metadata", ErrInvalidInvocation)
		}
	case StatusFailed:
		if i.Attempts < 1 || i.StartedAt == nil || i.CompletedAt == nil || i.Result == nil || i.Lease != nil {
			return fmt.Errorf("%w: failed record has incomplete metadata", ErrInvalidInvocation)
		}
	}
	return nil
}

func validateRecordBoundary(i Invocation) error {
	if i.SchemaVersion == 1 {
		return i.ValidateStrict()
	}
	return i.Validate()
}

// ValidateStrict applies the complete record invariants and requires the
// current schema version.
func (i Invocation) ValidateStrict() error {
	if i.SchemaVersion != 1 {
		return fmt.Errorf("%w: schema version must be 1", ErrInvalidInvocation)
	}
	if err := i.Validate(); err != nil {
		return err
	}
	return i.validateLifecycle()
}
