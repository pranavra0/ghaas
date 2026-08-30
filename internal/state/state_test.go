package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pranavra0/ghaas/internal/github"
)

type fakeGitData struct {
	mu         sync.Mutex
	refs       map[string]string
	blobs      map[string][]byte
	trees      map[string]github.GitTree
	commits    map[string]github.GitCommit
	next       int
	createRefs int
	updates    int
	conflicts  int
}

func newFakeGitData() *fakeGitData {
	return &fakeGitData{refs: map[string]string{}, blobs: map[string][]byte{}, trees: map[string]github.GitTree{}, commits: map[string]github.GitCommit{}}
}
func (f *fakeGitData) id(prefix string) string {
	f.next++
	return prefix + string(rune('a'+f.next%26)) + time.Now().Format("150405.000000000")
}
func (f *fakeGitData) GetRef(_ context.Context, ref string) (github.GitRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha, ok := f.refs[ref]
	if !ok {
		return github.GitRef{}, github.ErrNotFound
	}
	return github.GitRef{Ref: ref, SHA: sha}, nil
}
func (f *fakeGitData) CreateRef(_ context.Context, ref, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createRefs++
	if _, ok := f.refs[ref]; ok {
		return github.ErrConflict
	}
	f.refs[ref] = sha
	return nil
}
func (f *fakeGitData) UpdateRef(_ context.Context, ref, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if f.conflicts > 0 {
		f.conflicts--
		return github.ErrConflict
	}
	old, ok := f.refs[ref]
	if !ok || old == sha {
		if !ok {
			return github.ErrConflict
		}
	}
	f.refs[ref] = sha
	return nil
}
func (f *fakeGitData) GetCommit(_ context.Context, sha string) (github.GitCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.commits[sha]
	if !ok {
		return github.GitCommit{}, github.ErrNotFound
	}
	return c, nil
}
func (f *fakeGitData) CreateCommit(_ context.Context, message, treeSHA string, parents []string) (github.GitCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := f.id("c")
	c := github.GitCommit{SHA: sha, TreeSHA: treeSHA, Message: message}
	f.commits[sha] = c
	return c, nil
}
func (f *fakeGitData) GetTree(_ context.Context, sha string) (github.GitTree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tree, ok := f.trees[sha]
	if !ok {
		return github.GitTree{}, github.ErrNotFound
	}
	return tree, nil
}
func (f *fakeGitData) CreateTree(_ context.Context, _ string, entries []github.TreeEntry) (github.GitTree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := f.id("t")
	tree := github.GitTree{SHA: sha, Entries: append([]github.TreeEntry(nil), entries...), Tree: append([]github.TreeEntry(nil), entries...)}
	f.trees[sha] = tree
	return tree, nil
}
func (f *fakeGitData) GetBlob(_ context.Context, sha string) (github.GitBlob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.blobs[sha]
	if !ok {
		return github.GitBlob{}, github.ErrNotFound
	}
	return github.GitBlob{SHA: sha, Content: append([]byte(nil), content...)}, nil
}
func (f *fakeGitData) CreateBlob(_ context.Context, content []byte) (github.GitBlob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sha := f.id("b")
	f.blobs[sha] = append([]byte(nil), content...)
	return github.GitBlob{SHA: sha, Content: append([]byte(nil), content...)}, nil
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestDurableStateLifecycleAndFencing(t *testing.T) {
	git := newFakeGitData()
	clock := &fakeClock{now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
	store := New(git, WithClockSource(clock), WithLeaseDuration(time.Minute))
	ctx := context.Background()
	in, err := store.Ensure(ctx, "fn", "fn/uuid", 2)
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != StatusPending || in.Attempts != 0 || in.MaxAttempts != 2 {
		t.Fatalf("ensure = %#v", in)
	}
	if _, err := store.Ensure(ctx, "fn", "fn/uuid", 9); err != nil {
		t.Fatal(err)
	}
	if git.createRefs != 1 {
		t.Fatalf("bootstrap create refs = %d", git.createRefs)
	}

	in, lease, err := store.Acquire(ctx, "fn", "fn/uuid", "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != StatusRunning || in.Attempts != 1 || lease.Fence != 1 || lease.Token == "" {
		t.Fatalf("acquire = %#v lease=%#v", in, lease)
	}
	if _, _, err := store.Acquire(ctx, "fn", "fn/uuid", "worker-b"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second acquire = %v", err)
	}
	if _, err := store.BindProvider(ctx, lease, 42, 3, "workflow_dispatch"); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Second)
	renewed, err := store.Renew(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Fence != lease.Fence || !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("renewed = %#v", renewed)
	}
	if _, err := store.Complete(ctx, renewed, false, Result{ExitCode: 7, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	failed, err := store.Get(ctx, "fn", "fn/uuid")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed || failed.Attempts != 1 || failed.Provider == nil || failed.Provider.RunID != 42 || failed.Result == nil {
		t.Fatalf("failed = %#v", failed)
	}
	if _, err := store.Complete(ctx, lease, true); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("stale complete = %v", err)
	}

	second, lease2, err := store.Acquire(ctx, "fn", "fn/uuid", "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	if second.Attempts != 2 || lease2.Fence != 2 {
		t.Fatalf("retry = %#v lease=%#v", second, lease2)
	}
	if _, err := store.Complete(ctx, lease2, false); err != nil {
		t.Fatal(err)
	}
	exhausted, err := store.Get(ctx, "fn", "fn/uuid")
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.Status != StatusExhausted {
		t.Fatalf("exhausted = %#v", exhausted)
	}
}

func TestCASConflictRebasesAndStrictDecode(t *testing.T) {
	git := newFakeGitData()
	clock := &fakeClock{now: time.Unix(0, 0).UTC()}
	store := New(git, WithClockSource(clock), WithMaxCASRetries(3))
	if _, err := store.Ensure(context.Background(), "a", "a/1"); err != nil {
		t.Fatal(err)
	}
	git.conflicts = 1
	if _, err := store.Ensure(context.Background(), "b", "b/1"); err != nil {
		t.Fatal(err)
	}
	if git.updates == 0 {
		t.Fatal("expected CAS update")
	}
	if _, err := decode([]byte(`{"schema_version":1,"invocations":[],"unknown":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown field = %v", err)
	}
	if _, err := decode([]byte(`{"schema_version":1,"invocations":[]} {}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("trailing data = %v", err)
	}
}

func TestExpiredLeaseIsReclaimedWithNewFence(t *testing.T) {
	git := newFakeGitData()
	clock := &fakeClock{now: time.Unix(100, 0).UTC()}
	store := New(git, WithClockSource(clock), WithLeaseDuration(time.Minute))
	ctx := context.Background()
	if _, err := store.Ensure(ctx, "fn", "fn/expired", 2); err != nil {
		t.Fatal(err)
	}
	_, oldLease, err := store.Acquire(ctx, "fn", "fn/expired", "old")
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	_, newLease, err := store.Acquire(ctx, "fn", "fn/expired", "new")
	if err != nil {
		t.Fatal(err)
	}
	if newLease.Fence <= oldLease.Fence {
		t.Fatalf("fence did not advance: old=%d new=%d", oldLease.Fence, newLease.Fence)
	}
	if _, err := store.Complete(ctx, oldLease, true); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("stale completion = %v", err)
	}
	if _, err := store.Complete(ctx, newLease, true); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredFinalAttemptBecomesDurablyExhausted(t *testing.T) {
	git := newFakeGitData()
	clock := &fakeClock{now: time.Unix(200, 0).UTC()}
	store := New(git, WithClockSource(clock), WithLeaseDuration(time.Minute))
	ctx := context.Background()
	if _, err := store.Ensure(ctx, "fn", "fn/final", 1); err != nil {
		t.Fatal(err)
	}
	_, lease, err := store.Acquire(ctx, "fn", "fn/final", "worker")
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	in, _, err := store.Acquire(ctx, "fn", "fn/final", "reclaimer")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("reclaim final attempt error = %v, want exhausted", err)
	}
	if in.Status != StatusExhausted || in.Lease != nil || in.Attempts != 1 {
		t.Fatalf("reclaim final attempt = %#v, want exhausted without lease", in)
	}
	stored, err := store.Get(ctx, "fn", "fn/final")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusExhausted || stored.Lease != nil || stored.CompletedAt == nil {
		t.Fatalf("durable final attempt = %#v, want exhausted with completion time", stored)
	}
	if _, err := store.Complete(ctx, lease, true); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("stale final completion = %v, want stale lease", err)
	}
}

func TestProviderCanRebindWithinFencedLease(t *testing.T) {
	git := newFakeGitData()
	clock := &fakeClock{now: time.Unix(300, 0).UTC()}
	store := New(git, WithClockSource(clock), WithLeaseDuration(time.Minute))
	ctx := context.Background()
	if _, err := store.Ensure(ctx, "fn", "fn/rebind", 2); err != nil {
		t.Fatal(err)
	}
	in, lease, err := store.Acquire(ctx, "fn", "fn/rebind", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindProvider(ctx, lease, 42, 1, "workflow_dispatch"); err != nil {
		t.Fatal(err)
	}
	updatesBeforeIdempotent := git.updates
	same, err := store.BindProvider(ctx, lease, 42, 1, "workflow_dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if git.updates != updatesBeforeIdempotent {
		t.Fatal("idempotent provider bind created a state update")
	}
	if same.Attempts != in.Attempts {
		t.Fatalf("idempotent bind changed attempts: before=%d after=%d", in.Attempts, same.Attempts)
	}
	rebound, err := store.BindProvider(ctx, lease, 42, 2, "workflow_dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Provider == nil || rebound.Provider.RunID != 42 || rebound.Provider.RunAttempt != 2 {
		t.Fatalf("rebound provider = %#v", rebound.Provider)
	}
	if rebound.Attempts != 1 || rebound.Fence != lease.Fence {
		t.Fatalf("rebind changed logical attempt history: %#v lease=%#v", rebound, lease)
	}
}

func TestListReturnsDurableFunctionRecords(t *testing.T) {
	git := newFakeGitData()
	clock := &fakeClock{now: time.Unix(400, 0).UTC()}
	store := New(git, WithClockSource(clock))
	ctx := context.Background()
	for _, item := range [][2]string{{"alpha", "alpha/1"}, {"beta", "beta/1"}, {"alpha", "alpha/2"}} {
		if _, err := store.Ensure(ctx, item[0], item[1]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.List(ctx, "alpha", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Function != "alpha" || got[0].ID != "alpha/2" {
		t.Fatalf("limited alpha list = %#v", got)
	}
	got, err = store.List(ctx, "alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "alpha/2" || got[1].ID != "alpha/1" {
		t.Fatalf("alpha list = %#v", got)
	}
	if got, err := store.List(ctx, "missing", 10); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("missing function list = %#v, err=%v", got, err)
	}
	if _, err := store.List(ctx, "alpha", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero list limit = %v, want invalid", err)
	}
}

func TestListSortsNewestBeforeApplyingLimit(t *testing.T) {
	git := newFakeGitData()
	clock := &fakeClock{now: time.Unix(1_000, 0).UTC()}
	store := New(git, WithClockSource(clock))
	ctx := context.Background()
	for i := range 101 {
		clock.now = time.Unix(int64(1_000+i), 0).UTC()
		id := fmt.Sprintf("alpha/%03d", i)
		if _, err := store.Ensure(ctx, "alpha", id); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := store.List(ctx, "alpha", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 || latest[0].ID != "alpha/100" {
		t.Fatalf("latest invocation = %#v", latest)
	}
	bounded, err := store.List(ctx, "alpha", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 100 || bounded[0].ID != "alpha/100" || bounded[99].ID != "alpha/001" {
		t.Fatalf("bounded newest invocations = first=%#v last=%#v", bounded[0], bounded[len(bounded)-1])
	}

	clock.now = time.Unix(2_000, 0).UTC()
	for _, id := range []string{"alpha/a", "alpha/z"} {
		if _, err := store.Ensure(ctx, "alpha", id); err != nil {
			t.Fatal(err)
		}
	}
	tied, err := store.List(ctx, "alpha", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tied) != 1 || tied[0].ID != "alpha/z" {
		t.Fatalf("tie-broken latest invocation = %#v", tied)
	}
}
