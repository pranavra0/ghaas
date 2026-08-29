package state

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"ghaas/internal/invocation"
)

type recordKey struct {
	function string
	id       invocation.InvocationID
}

// Memory is a mutex-protected, process-local Store. Every value crossing the
// API boundary is copied, including optional pointer fields.
type Memory struct {
	mu      sync.RWMutex
	records map[recordKey]invocation.Invocation
}

// NewMemory returns an empty in-memory state store.
func NewMemory() *Memory {
	return &Memory{records: make(map[recordKey]invocation.Invocation)}
}

// NewMemoryStore is an explicit alias for NewMemory.
func NewMemoryStore() *Memory { return NewMemory() }

func (m *Memory) Get(ctx context.Context, function string, id invocation.InvocationID) (*invocation.Invocation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if _, err := canonicalInvocationID(function, id); err != nil {
		return nil, err
	}
	m.mu.RLock()
	v, ok := m.records[recordKey{function: function, id: id}]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	if v.Function != function || v.ID != id {
		return nil, fmt.Errorf("%w: stored record identity mismatch", invocation.ErrInvalidInvocation)
	}
	if err := validateRecord(v); err != nil {
		return nil, err
	}
	copy := clone(v)
	return &copy, nil
}

func (m *Memory) Create(ctx context.Context, v invocation.Invocation) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateRecord(v); err != nil {
		return err
	}
	key := recordKey{function: v.Function, id: v.ID}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records == nil {
		m.records = make(map[recordKey]invocation.Invocation)
	}
	if _, exists := m.records[key]; exists {
		return ErrConflict
	}
	m.records[key] = clone(v)
	return nil
}

func (m *Memory) CompareAndSwap(ctx context.Context, previous, next invocation.Invocation) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := validateRecord(previous); err != nil {
		return err
	}
	if err := validateRecord(next); err != nil {
		return err
	}
	if err := validateCAS(previous, next); err != nil {
		return err
	}
	key := recordKey{function: previous.Function, id: previous.ID}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, exists := m.records[key]
	if !exists {
		return ErrNotFound
	}
	if current.Function != previous.Function || current.ID != previous.ID {
		return fmt.Errorf("%w: stored record identity mismatch", invocation.ErrInvalidInvocation)
	}
	if err := validateRecord(current); err != nil {
		return err
	}
	if !reflect.DeepEqual(current, previous) {
		return ErrConflict
	}
	m.records[key] = clone(next)
	return nil
}

func (m *Memory) List(ctx context.Context, function string, limit int) ([]invocation.Invocation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if function != "" {
		if err := validateFunction(function); err != nil {
			return nil, err
		}
	}
	m.mu.RLock()
	items := make([]invocation.Invocation, 0, len(m.records))
	for key, v := range m.records {
		if err := validateRecord(v); err != nil {
			m.mu.RUnlock()
			return nil, err
		}
		if key.function != v.Function || key.id != v.ID {
			m.mu.RUnlock()
			return nil, fmt.Errorf("%w: stored record identity mismatch", invocation.ErrInvalidInvocation)
		}
		if key.function == function || function == "" {
			items = append(items, clone(v))
		}
	}
	m.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ID < items[j].ID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func validateRecord(v invocation.Invocation) error {
	if v.SchemaVersion != 1 {
		return fmt.Errorf("%w: schema version must be 1", invocation.ErrInvalidInvocation)
	}
	if err := v.Validate(); err != nil {
		return err
	}
	if _, err := canonicalInvocationID(v.Function, v.ID); err != nil {
		return err
	}
	// Terminal and completed-failure records must carry a complete result/
	// history. Pending and running records retain compatibility with older
	// callers that populated metadata incrementally before issuing CAS.
	if v.Status == invocation.StatusFailed || v.Status.IsTerminal() {
		if err := v.ValidateStrict(); err != nil {
			return err
		}
	}
	return nil
}

func clone(v invocation.Invocation) invocation.Invocation {
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

var _ Store = (*Memory)(nil)

// MemoryStore is an alternate name for the v0.1 memory backend.
type MemoryStore = Memory
