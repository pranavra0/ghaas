// Package state provides invocation state storage implementations.
package state

import (
	"context"

	"ghaas/internal/invocation"
)

var (
	ErrNotFound = invocation.ErrNotFound
	ErrConflict = invocation.ErrConflict
)
// Aliases keep backend-facing code concise while preserving the single
// invocation record definition.
type Invocation = invocation.Invocation
type InvocationID = invocation.InvocationID

// Store abstracts invocation persistence. Durable backends, leases, retries,
// and branch-backed state are intentionally outside the v0.1 implementation.
type Store interface {
	Get(context.Context, string, invocation.InvocationID) (*invocation.Invocation, error)
	Create(context.Context, invocation.Invocation) error
	CompareAndSwap(context.Context, invocation.Invocation, invocation.Invocation) error
	List(context.Context, string, int) ([]invocation.Invocation, error)
}
