// Package state provides invocation state storage implementations.
package state

import (
	"context"
	"fmt"
	"strings"

	"ghaas/internal/invocation"
)

var (
	ErrNotFound           = invocation.ErrNotFound
	ErrConflict           = invocation.ErrConflict
	ErrStateNotFound      = invocation.ErrNotFound
	ErrStateConflict      = invocation.ErrConflict
	ErrInvalidTransition  = invocation.ErrInvalidTransition
	ErrInvalidInvocation  = invocation.ErrInvalidInvocation
	ErrLeaseHeld          = invocation.ErrLeaseHeld
	ErrInvocationComplete = invocation.ErrInvocationComplete
)

// Aliases keep backend-facing code concise while preserving the single
// invocation record definition.
type Invocation = invocation.Invocation
type InvocationID = invocation.InvocationID

// Store abstracts invocation persistence. Implementations must validate
// records and use compare-and-swap for every state transition.
type Store interface {
	Get(context.Context, string, invocation.InvocationID) (*invocation.Invocation, error)

	Create(context.Context, invocation.Invocation) error
	CompareAndSwap(context.Context, invocation.Invocation, invocation.Invocation) error
	List(context.Context, string, int) ([]invocation.Invocation, error)
}

func validateCAS(previous, next invocation.Invocation) error {
	if previous.Function != next.Function || previous.ID != next.ID {
		return ErrConflict
	}
	if previous.Status != next.Status && !invocation.CanTransition(previous.Status, next.Status) {
		return fmt.Errorf("%w: %s -> %s", invocation.ErrInvalidTransition, previous.Status, next.Status)
	}
	return nil
}

// ValidateRecord checks the storage-level schema and lifecycle invariants.
func ValidateRecord(value invocation.Invocation) error {
	return validateRecord(value)
}

// Validate is a concise compatibility spelling for ValidateRecord.
func Validate(value invocation.Invocation) error {
	return ValidateRecord(value)
}

// validateFunction checks the path-safe function component used by invocation
// IDs. Keep this in state so all backends enforce the same identity rules.
func validateFunction(function string) error {
	if _, err := invocation.NewScheduledID(function, "_"); err != nil {
		return fmt.Errorf("%w: invalid function: %v", invocation.ErrInvalidInvocation, err)
	}
	return nil
}

// canonicalInvocationID validates ownership and rejects non-canonical IDs.
// Manual IDs use the canonical lower-case UUID spelling; scheduled IDs retain
// the schedule key exactly as supplied.
func canonicalInvocationID(function string, id invocation.InvocationID) (invocation.InvocationID, error) {
	if err := validateFunction(function); err != nil {
		return "", err
	}
	parts := strings.Split(string(id), "/")
	if len(parts) != 2 || parts[0] != function {
		return "", fmt.Errorf("%w: invocation ID does not belong to function", invocation.ErrInvalidInvocation)
	}
	if err := invocation.ValidateInvocationID(id); err != nil {
		return "", fmt.Errorf("%w: malformed invocation ID: %v", invocation.ErrInvalidInvocation, err)
	}
	canonical, err := invocation.NewScheduledID(function, parts[1])
	if err != nil {
		return "", fmt.Errorf("%w: malformed invocation ID: %v", invocation.ErrInvalidInvocation, err)
	}
	if err := invocation.ValidateUUID(parts[1]); err == nil {
		manual, manualErr := invocation.NewManualID(function, parts[1])
		if manualErr != nil {
			return "", fmt.Errorf("%w: malformed invocation ID: %v", invocation.ErrInvalidInvocation, manualErr)
		}
		canonical = manual
	}
	if id != canonical {
		return "", fmt.Errorf("%w: invocation ID is not canonical", invocation.ErrInvalidInvocation)
	}
	return canonical, nil
}
