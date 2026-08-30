// Package state implements the production durable invocation state store.
//
// State is one bounded, schema-versioned JSON document in GitHub's Git Data
// API. This package never writes the checkout and has no process-local durable
// backend.
package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/pranavra0/ghaas/internal/github"
)

const (
	SchemaVersion  = 1
	StateRef       = "refs/heads/ghaas-state-v1"
	StatePath      = ".ghaas/state/v1.json"
	MaxStateBytes  = 1 << 20
	MaxInvocations = 10000
	maxCASRetries  = 8
	// Keep enough room for active and retryable records while bounding the
	// terminal history retained by the aggregate.
	defaultTerminalRetention = MaxInvocations / 2
)

// GitData is the GitHub Git Database subset used by the store. UpdateRef is a
// compare-and-swap operation in the provider implementation and is never a
// force update.
type GitData = github.GitData
type Ref = github.GitRef
type Commit = github.GitCommit
type Tree = github.GitTree
type Blob = github.GitBlob
type TreeEntry = github.TreeEntry

// InvocationID is an alias so callers can document logical IDs without
// changing the string-based API used by workflow/runtime boundaries.
type InvocationID = string
type InvocationStatus = Status

var (
	ErrNotFound           = errors.New("state not found")
	ErrConflict           = errors.New("state conflict")
	ErrLeaseHeld          = errors.New("state lease held")
	ErrStaleLease         = errors.New("state stale lease")
	ErrInvalid            = errors.New("state invalid")
	ErrAlreadyComplete    = errors.New("state invocation complete")
	ErrExhausted          = errors.New("state retries exhausted")
	ErrLeaseExpired       = ErrStaleLease
	ErrLeaseLost          = ErrStaleLease
	ErrRetryExhausted     = ErrExhausted
	ErrInvocationComplete = ErrAlreadyComplete
	ErrStateConflict      = ErrConflict
	ErrInvalidState       = ErrInvalid
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusExhausted Status = "exhausted"
	Pending                = StatusPending
	Running                = StatusRunning
	Succeeded              = StatusSucceeded
	Failed                 = StatusFailed
	Exhausted              = StatusExhausted
)

type Provider struct {
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	Reason     string `json:"reason,omitempty"`
}

// Function and ID are process-local context for a returned lease and are not
// duplicated in the aggregate wire representation.
type Lease struct {
	Function  string    `json:"-"`
	ID        string    `json:"-"`
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	Fence     uint64    `json:"fence"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Result struct {
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Invocation struct {
	SchemaVersion int        `json:"schema_version"`
	Function      string     `json:"function"`
	ID            string     `json:"invocation_id"`
	Status        Status     `json:"status"`
	Attempts      int        `json:"attempts"`
	MaxAttempts   int        `json:"max_attempts"`
	Fence         uint64     `json:"fence,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	Provider      *Provider  `json:"provider,omitempty"`
	Lease         *Lease     `json:"lease,omitempty"`
	Result        *Result    `json:"result,omitempty"`
}

type Option func(*options)
type options struct {
	now               func() time.Time
	leaseDuration     time.Duration
	maxCAS            int
	terminalRetention int
}

func WithClock(now func() time.Time) Option {
	return func(o *options) {
		if now != nil {
			o.now = now
		}
	}
}
func WithLeaseDuration(duration time.Duration) Option {
	return func(o *options) {
		if duration > 0 {
			o.leaseDuration = duration
		}
	}
}
func WithMaxCASRetries(max int) Option {
	return func(o *options) {
		if max > 0 {
			o.maxCAS = max
		}
	}
}

// WithTerminalRetention limits the number of completed (succeeded or
// exhausted) records retained in the aggregate. Oldest terminal records are
// compacted first; pending, running, and retryable failed records are never
// compacted.
func WithTerminalRetention(max int) Option {
	return func(o *options) {
		if max > 0 {
			o.terminalRetention = max
		}
	}
}

type Clock interface{ Now() time.Time }

func WithClockSource(clock Clock) Option {
	return func(o *options) {
		if clock != nil {
			o.now = clock.Now
		}
	}
}

type Store struct {
	git               GitData
	now               func() time.Time
	leaseDuration     time.Duration
	maxCAS            int
	terminalRetention int
}

func New(git GitData, opts ...Option) *Store {
	o := options{
		now:               time.Now,
		leaseDuration:     5 * time.Minute,
		maxCAS:            maxCASRetries,
		terminalRetention: defaultTerminalRetention,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &Store{
		git:               git,
		now:               o.now,
		leaseDuration:     o.leaseDuration,
		maxCAS:            o.maxCAS,
		terminalRetention: o.terminalRetention,
	}
}
func NewStore(git GitData, opts ...Option) *Store { return New(git, opts...) }

// Ensure creates a pending invocation if absent. Existing records are returned
// unchanged, so repeated manual dispatch attempts never create a new ID.
func (s *Store) Ensure(ctx context.Context, function, id string, maxAttempts ...int) (Invocation, error) {
	if err := validKey(function, id); err != nil {
		return Invocation{}, err
	}
	max := 1
	if len(maxAttempts) > 0 {
		max = maxAttempts[0]
	}
	if max < 1 {
		return Invocation{}, fmt.Errorf("%w: max attempts must be positive", ErrInvalid)
	}
	return s.mutate(ctx, function, id, func(doc *document) (Invocation, bool, error) {
		if existing, ok := doc.find(function, id); ok {
			return existing, false, nil
		}
		now := s.now().UTC()
		in := Invocation{SchemaVersion: SchemaVersion, Function: function, ID: id, Status: StatusPending, MaxAttempts: max, CreatedAt: now}
		doc.Invocations = append(doc.Invocations, in)
		return in, true, nil
	})
}

func (s *Store) Get(ctx context.Context, function, id string) (Invocation, error) {
	if err := validKey(function, id); err != nil {
		return Invocation{}, err
	}
	doc, _, err := s.read(ctx)
	if err != nil {
		return Invocation{}, err
	}
	in, ok := doc.find(function, id)
	if !ok {
		return Invocation{}, fmt.Errorf("%w: %s/%s", ErrNotFound, function, id)
	}
	return in, nil
}

// List returns up to limit durable invocations for function, newest first by
// creation time, with invocation ID as the deterministic tie-break. Sorting is
// applied before truncation so a bounded query still returns the true latest
// records even when the aggregate contains older records first.
func (s *Store) List(ctx context.Context, function string, limit int) ([]Invocation, error) {
	if err := validFunction(function); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, fmt.Errorf("%w: list limit must be positive", ErrInvalid)
	}
	doc, _, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	invocations := make([]Invocation, 0, min(limit, len(doc.Invocations)))
	for _, inv := range doc.Invocations {
		if inv.Function == function {
			invocations = append(invocations, inv)
		}
	}
	sort.Slice(invocations, func(i, j int) bool {
		a, b := invocations[i], invocations[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	if len(invocations) > limit {
		invocations = invocations[:limit]
	}
	return invocations, nil
}

func validFunction(function string) error {
	if strings.TrimSpace(function) == "" || len(function) > 256 || strings.ContainsAny(function, "\r\n") {
		return fmt.Errorf("%w: invalid function", ErrInvalid)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Acquire claims pending work or a retryable failed invocation. Attempts and
// the fencing counter are monotonic. An expired lease can be recovered.
func (s *Store) Acquire(ctx context.Context, function, id, owner string) (Invocation, Lease, error) {
	if err := validKey(function, id); err != nil {
		return Invocation{}, Lease{}, err
	}
	if strings.TrimSpace(owner) == "" {
		return Invocation{}, Lease{}, fmt.Errorf("%w: lease owner is required", ErrInvalid)
	}
	var lease Lease
	var exhausted bool
	in, err := s.mutate(ctx, function, id, func(doc *document) (Invocation, bool, error) {
		idx, ok := doc.index(function, id)
		if !ok {
			return Invocation{}, false, fmt.Errorf("%w: %s/%s", ErrNotFound, function, id)
		}
		cur := doc.Invocations[idx]
		if cur.Status == StatusSucceeded || cur.Status == StatusExhausted {
			return cur, false, fmt.Errorf("%w: %s/%s", ErrAlreadyComplete, function, id)
		}
		now := s.now().UTC()
		if cur.Lease != nil && cur.Lease.ExpiresAt.After(now) {
			return cur, false, fmt.Errorf("%w: %s/%s", ErrLeaseHeld, function, id)
		}
		if cur.Attempts >= cur.MaxAttempts {
			// A worker can die after claiming the final attempt. Persist the
			// terminal transition while reclaiming that expired lease instead
			// of leaving status=running forever.
			if cur.Status == StatusRunning && cur.Lease != nil {
				cur.Status = StatusExhausted
				cur.CompletedAt = timePtr(now)
				cur.Lease = nil
				doc.Invocations[idx] = cur
				exhausted = true
				return cur, true, nil
			}
			return cur, false, fmt.Errorf("%w: %s/%s", ErrExhausted, function, id)
		}
		token, err := newToken()
		if err != nil {
			return cur, false, err
		}
		fence := cur.Fence + 1
		if cur.Lease != nil && cur.Lease.Fence >= fence {
			fence = cur.Lease.Fence + 1
		}
		lease = Lease{Function: function, ID: id, Owner: owner, Token: token, Fence: fence, ExpiresAt: now.Add(s.leaseDuration)}
		cur.Fence = fence
		cur.Status = StatusRunning
		cur.Attempts++
		cur.StartedAt = timePtr(now)
		cur.CompletedAt = nil
		cur.Lease = &lease
		doc.Invocations[idx] = cur
		return cur, true, nil
	})
	if err == nil && exhausted {
		err = fmt.Errorf("%w: %s/%s", ErrExhausted, function, id)
	}
	return in, lease, err
}
func (s *Store) Renew(ctx context.Context, lease Lease) (Lease, error) {
	if err := validLeaseCall(lease); err != nil {
		return Lease{}, err
	}
	var renewed Lease
	_, err := s.mutateWithGuard(ctx, lease.Function, lease.ID, func(doc *document) (Invocation, bool, error) {
		idx, ok := doc.index(lease.Function, lease.ID)
		if !ok {
			return Invocation{}, false, fmt.Errorf("%w: %s/%s", ErrNotFound, lease.Function, lease.ID)
		}
		cur := doc.Invocations[idx]
		now := s.now().UTC()
		if err := checkLease(cur, lease, now); err != nil {
			return cur, false, err
		}
		renewed = lease
		renewed.ExpiresAt = now.Add(s.leaseDuration)
		cur.Lease = &renewed
		doc.Invocations[idx] = cur
		return cur, true, nil
	}, s.leaseExpiryGuard(func() Lease { return renewed }))
	return renewed, err
}

func (s *Store) BindProvider(ctx context.Context, lease Lease, runID int64, runAttempt int, reason string) (Invocation, error) {
	if err := validLeaseCall(lease); err != nil {
		return Invocation{}, err
	}
	if runID <= 0 || runAttempt <= 0 {
		return Invocation{}, fmt.Errorf("%w: provider run id and attempt must be positive", ErrInvalid)
	}

	var result Invocation
	var mutateErr error
	result, mutateErr = s.mutateWithGuard(ctx, lease.Function, lease.ID, func(doc *document) (Invocation, bool, error) {
		idx, ok := doc.index(lease.Function, lease.ID)
		if !ok {
			return Invocation{}, false, fmt.Errorf("%w: %s/%s", ErrNotFound, lease.Function, lease.ID)
		}

		cur := doc.Invocations[idx]
		if err := checkLease(cur, lease, s.now().UTC()); err != nil {
			return cur, false, err
		}

		provider := &Provider{
			RunID:      runID,
			RunAttempt: runAttempt,
			Reason:     reason,
		}
		if cur.Provider != nil && *cur.Provider == *provider {
			// Binding the same provider reference is idempotent. In
			// particular, retries of the runner's startup path must not
			// create another state commit.
			return cur, false, nil
		}

		// A provider rerun (or a new provider run for the same logical
		// invocation) may be discovered while this fenced lease is active.
		// Replace only the current reference: Attempts and Fence remain the
		// durable logical-attempt history and are never reset by rebinding.
		cur.Provider = provider
		doc.Invocations[idx] = cur
		return cur, true, nil
	}, s.leaseExpiryGuard(func() Lease { return lease }))
	return result, mutateErr
}

// Complete records the durable outcome and invalidates the lease. A failed
// invocation remains retryable until Attempts reaches MaxAttempts, at which
// point it is exhausted. Retries retain the same invocation ID.
func (s *Store) Complete(ctx context.Context, lease Lease, succeeded bool, result ...Result) (Invocation, error) {
	if err := validLeaseCall(lease); err != nil {
		return Invocation{}, err
	}
	return s.mutateWithGuard(ctx, lease.Function, lease.ID, func(doc *document) (Invocation, bool, error) {
		idx, ok := doc.index(lease.Function, lease.ID)
		if !ok {
			return Invocation{}, false, fmt.Errorf("%w: %s/%s", ErrNotFound, lease.Function, lease.ID)
		}
		cur := doc.Invocations[idx]
		now := s.now().UTC()
		if err := checkLease(cur, lease, now); err != nil {
			return cur, false, err
		}
		cur.CompletedAt = timePtr(now)
		cur.Lease = nil
		if len(result) > 0 {
			copyResult := result[0]
			cur.Result = &copyResult
		} else {
			cur.Result = nil
		}
		if succeeded {
			cur.Status = StatusSucceeded
			cur.LastError = ""
		} else {
			if cur.Result != nil && cur.Result.Error != "" {
				cur.LastError = cur.Result.Error
			} else {
				cur.LastError = "failed"
			}
			if cur.Attempts >= cur.MaxAttempts {
				cur.Status = StatusExhausted
			} else {
				cur.Status = StatusFailed
			}
		}
		doc.Invocations[idx] = cur
		return cur, true, nil
	}, s.leaseExpiryGuard(func() Lease { return lease }))
}

func validKey(function, id string) error {
	if strings.TrimSpace(function) == "" || strings.TrimSpace(id) == "" || len(function) > 256 || len(id) > 512 {
		return fmt.Errorf("%w: invalid function or invocation id", ErrInvalid)
	}
	if strings.ContainsAny(function, "\r\n") || strings.ContainsAny(id, "\r\n") {
		return fmt.Errorf("%w: invalid function or invocation id", ErrInvalid)
	}
	return nil
}

func validLease(lease Lease) error {
	if strings.TrimSpace(lease.Owner) == "" || strings.TrimSpace(lease.Token) == "" || lease.Fence == 0 || lease.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: incomplete lease", ErrInvalid)
	}
	return nil
}
func validLeaseCall(lease Lease) error {
	if err := validLease(lease); err != nil {
		return err
	}
	if strings.TrimSpace(lease.Function) == "" || strings.TrimSpace(lease.ID) == "" {
		return fmt.Errorf("%w: lease invocation is required", ErrInvalid)
	}
	return nil
}
func checkLease(in Invocation, lease Lease, now time.Time) error {
	if in.Lease == nil {
		return fmt.Errorf("%w: no active lease", ErrStaleLease)
	}
	if in.Lease.Owner != lease.Owner || in.Lease.Token != lease.Token || in.Lease.Fence != lease.Fence {
		return fmt.Errorf("%w: lease fence mismatch", ErrStaleLease)
	}
	if !in.Lease.ExpiresAt.After(now) || !lease.ExpiresAt.Equal(in.Lease.ExpiresAt) {
		return fmt.Errorf("%w: lease expired", ErrStaleLease)
	}
	return nil
}
func newToken() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("state lease token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
func timePtr(t time.Time) *time.Time { return &t }

type document struct {
	SchemaVersion int          `json:"schema_version"`
	Invocations   []Invocation `json:"invocations"`
}

func (d *document) index(function, id string) (int, bool) {
	for i := range d.Invocations {
		if d.Invocations[i].Function == function && d.Invocations[i].ID == id {
			return i, true
		}
	}
	return 0, false
}
func (d *document) find(function, id string) (Invocation, bool) {
	i, ok := d.index(function, id)
	if !ok {
		return Invocation{}, false
	}
	return d.Invocations[i], true
}

func terminal(in Invocation) bool {
	return in.Status == StatusSucceeded || in.Status == StatusExhausted
}

func terminalTime(in Invocation) time.Time {
	if in.CompletedAt != nil {
		return in.CompletedAt.UTC()
	}
	return in.CreatedAt.UTC()
}

// compactTerminals retains the newest terminal records. The ordering is
// explicit so concurrent rebases produce the same retained set regardless of
// the aggregate's prior physical ordering.
func compactTerminals(doc *document, retain int) {
	if retain < 0 {
		return
	}
	terminalIndexes := make([]int, 0)
	for i, in := range doc.Invocations {
		if terminal(in) {
			terminalIndexes = append(terminalIndexes, i)
		}
	}
	if len(terminalIndexes) <= retain {
		return
	}
	sort.Slice(terminalIndexes, func(i, j int) bool {
		a := doc.Invocations[terminalIndexes[i]]
		b := doc.Invocations[terminalIndexes[j]]
		if ta, tb := terminalTime(a), terminalTime(b); !ta.Equal(tb) {
			return ta.Before(tb)
		}
		if a.Function != b.Function {
			return a.Function < b.Function
		}
		return a.ID < b.ID
	})
	remove := make(map[int]struct{}, len(terminalIndexes)-retain)
	for _, idx := range terminalIndexes[:len(terminalIndexes)-retain] {
		remove[idx] = struct{}{}
	}
	kept := doc.Invocations[:0]
	for i, in := range doc.Invocations {
		if _, drop := remove[i]; !drop {
			kept = append(kept, in)
		}
	}
	doc.Invocations = kept
}

func decode(data []byte) (document, error) {
	if len(data) > MaxStateBytes {
		return document{}, fmt.Errorf("%w: state blob exceeds %d bytes", ErrInvalid, MaxStateBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var doc document
	if err := decoder.Decode(&doc); err != nil {
		return document{}, fmt.Errorf("%w: decode state: %v", ErrInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return document{}, fmt.Errorf("%w: trailing state data", ErrInvalid)
	}
	if doc.SchemaVersion != SchemaVersion {
		return document{}, fmt.Errorf("%w: unsupported schema version %d", ErrInvalid, doc.SchemaVersion)
	}
	if len(doc.Invocations) > MaxInvocations {
		return document{}, fmt.Errorf("%w: too many invocations", ErrInvalid)
	}
	if err := validateDocument(doc); err != nil {
		return document{}, err
	}
	return doc, nil
}

func validateDocument(doc document) error {
	seen := make(map[string]struct{}, len(doc.Invocations))
	for i := range doc.Invocations {
		in := doc.Invocations[i]
		if err := validKey(in.Function, in.ID); err != nil {
			return err
		}
		key := in.Function + "\x00" + in.ID
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: duplicate invocation", ErrInvalid)
		}
		seen[key] = struct{}{}
		if in.SchemaVersion != SchemaVersion || in.Attempts < 0 || in.MaxAttempts < 1 || in.Attempts > in.MaxAttempts {
			return fmt.Errorf("%w: invalid invocation schema or attempts", ErrInvalid)
		}
		switch in.Status {
		case StatusPending, StatusRunning, StatusSucceeded, StatusFailed, StatusExhausted:
		default:
			return fmt.Errorf("%w: invalid status", ErrInvalid)
		}
		if in.Status == StatusRunning && in.Lease == nil {
			return fmt.Errorf("%w: running invocation has no lease", ErrInvalid)
		}
		if in.Status != StatusRunning && in.Lease != nil {
			return fmt.Errorf("%w: non-running invocation has a lease", ErrInvalid)
		}
		if in.Status == StatusExhausted && in.Attempts < in.MaxAttempts {
			return fmt.Errorf("%w: exhausted invocation has retries remaining", ErrInvalid)
		}
		if in.Status == StatusFailed && in.Attempts >= in.MaxAttempts {
			return fmt.Errorf("%w: failed invocation has no retries remaining", ErrInvalid)
		}
		if in.Lease != nil {
			if err := validLease(*in.Lease); err != nil {
				return err
			}
		}
		if in.Provider != nil && (in.Provider.RunID <= 0 || in.Provider.RunAttempt <= 0) {
			return fmt.Errorf("%w: invalid provider binding", ErrInvalid)
		}
	}
	return nil
}

func encode(doc document) ([]byte, error) {
	if len(doc.Invocations) > MaxInvocations {
		return nil, fmt.Errorf("%w: too many invocations", ErrInvalid)
	}
	sort.Slice(doc.Invocations, func(i, j int) bool {
		a, b := doc.Invocations[i], doc.Invocations[j]
		if a.Function == b.Function {
			return a.ID < b.ID
		}
		return a.Function < b.Function
	})
	if err := validateDocument(doc); err != nil {
		return nil, err
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("state encode: %w", err)
	}
	if len(data) > MaxStateBytes {
		return nil, fmt.Errorf("%w: state blob exceeds %d bytes", ErrInvalid, MaxStateBytes)
	}
	return data, nil
}

func mapGitError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, github.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if errors.Is(err, github.ErrConflict) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	var apiErr *github.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

func (s *Store) read(ctx context.Context) (document, string, error) {
	if s == nil || s.git == nil {
		return document{}, "", fmt.Errorf("%w: nil git data", ErrInvalid)
	}
	ref, err := s.git.GetRef(ctx, StateRef)
	if err != nil {
		err = mapGitError(err)
		if errors.Is(err, ErrNotFound) {
			return document{SchemaVersion: SchemaVersion}, "", nil
		}
		return document{}, "", err
	}
	if strings.TrimSpace(ref.SHA) == "" {
		return document{}, "", fmt.Errorf("%w: empty state ref", ErrInvalid)
	}
	commit, err := s.git.GetCommit(ctx, ref.SHA)
	if err != nil {
		return document{}, "", mapGitError(err)
	}
	tree, err := s.git.GetTree(ctx, commit.TreeSHA)
	if err != nil {
		return document{}, "", mapGitError(err)
	}
	var blobSHA string
	entries := tree.Entries
	if len(entries) == 0 {
		entries = tree.Tree
	}
	for _, entry := range entries {
		if entry.Path == StatePath {
			if entry.Type != "" && entry.Type != "blob" {
				return document{}, "", fmt.Errorf("%w: state path is not a blob", ErrInvalid)
			}
			blobSHA = entry.SHA
			if blobSHA == "" {
				blobSHA = entry.BlobSHA
			}
			break
		}
	}
	if blobSHA == "" {
		return document{SchemaVersion: SchemaVersion}, ref.SHA, nil
	}
	blob, err := s.git.GetBlob(ctx, blobSHA)
	if err != nil {
		return document{}, "", mapGitError(err)
	}
	doc, err := decode(blob.Content)
	if err != nil {
		return document{}, ref.SHA, err
	}
	for i := range doc.Invocations {
		if doc.Invocations[i].Lease != nil {
			doc.Invocations[i].Lease.Function = doc.Invocations[i].Function
			doc.Invocations[i].Lease.ID = doc.Invocations[i].ID
		}
	}
	return doc, ref.SHA, nil
}

func (s *Store) mutate(ctx context.Context, function, id string, fn func(*document) (Invocation, bool, error)) (Invocation, error) {
	return s.mutateWithGuard(ctx, function, id, fn, nil)
}

func (s *Store) leaseExpiryGuard(lease func() Lease) func(document) error {
	return func(_ document) error {
		if !lease().ExpiresAt.After(s.now().UTC()) {
			return fmt.Errorf("%w: lease expired", ErrStaleLease)
		}
		return nil
	}
}

func (s *Store) mutateWithGuard(ctx context.Context, function, id string, fn func(*document) (Invocation, bool, error), finalGuard func(document) error) (Invocation, error) {
	var zero Invocation
	for range s.maxCAS {
		doc, head, err := s.read(ctx)
		if err != nil {
			return zero, err
		}
		if doc.SchemaVersion == 0 {
			doc.SchemaVersion = SchemaVersion
		}
		in, changed, fnErr := fn(&doc)
		if fnErr != nil {
			return in, fnErr
		}
		if !changed {
			return in, nil
		}
		retention := s.terminalRetention
		if retention <= 0 {
			retention = defaultTerminalRetention
		}
		compactTerminals(&doc, retention)
		body, err := encode(doc)
		if err != nil {
			return zero, err
		}
		blob, err := s.git.CreateBlob(ctx, body)
		if err != nil {
			return zero, mapGitError(err)
		}
		baseTree := ""
		parents := []string{}
		if head != "" {
			prior, getErr := s.git.GetCommit(ctx, head)
			if getErr != nil {
				return zero, mapGitError(getErr)
			}
			baseTree = prior.TreeSHA
			parents = []string{head}
		}
		tree, err := s.git.CreateTree(ctx, baseTree, []TreeEntry{{Path: StatePath, Mode: "100644", Type: "blob", SHA: blob.SHA}})
		if err != nil {
			return zero, mapGitError(err)
		}
		commit, err := s.git.CreateCommit(ctx, "ghaas state v1", tree.SHA, parents)
		if err != nil {
			return zero, mapGitError(err)
		}
		// Lease validity is checked again after all fallible network work and
		// immediately before the provider CAS. A lease that expires during
		// blob/tree/commit creation must not publish its stale transition.
		if finalGuard != nil {
			if guardErr := finalGuard(doc); guardErr != nil {
				return in, guardErr
			}
		}
		if head == "" {
			err = s.git.CreateRef(ctx, StateRef, commit.SHA)
		} else {
			err = s.git.UpdateRef(ctx, StateRef, commit.SHA)
		}
		if err != nil {
			err = mapGitError(err)
			if errors.Is(err, ErrConflict) {
				continue
			}
			return zero, err
		}
		return in, nil
	}
	return zero, fmt.Errorf("%w: CAS retries exhausted", ErrConflict)
}
