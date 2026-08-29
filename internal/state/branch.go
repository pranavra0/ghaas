package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ghaas/internal/invocation"
)

const maxStateRecordBytes int64 = 1 << 20

// BranchStore is a practical file-backed Store. Files live below
// <root>/<branch>/functions/<function>/<invocation-key>.json, mirroring the
// ghaas-state branch layout. Updates use read/compare/atomic-rename optimistic
// writes. A process-local lock also makes independent BranchStore values sharing
// a namespace safe for concurrent use.
type BranchStore struct {
	Root   string
	Branch string
}

// Branch and FileStore are descriptive aliases for BranchStore.
type Branch = BranchStore
type FileStore = BranchStore

var branchLocks sync.Map // map[string]*sync.RWMutex

var errSymlinkPath = errors.New("state path contains symlink")

func NewBranchStore(root string, branch ...string) *BranchStore {
	name := "ghaas-state"
	if len(branch) > 0 && branch[0] != "" {
		name = branch[0]
	}
	return &BranchStore{Root: root, Branch: name}
}

func NewBranch(root string, branch ...string) *BranchStore {
	return NewBranchStore(root, branch...)
}
func NewFileStore(root string, branch ...string) *BranchStore {
	return NewBranchStore(root, branch...)
}

// OpenBranchStore creates the backing directory and returns a ready store.
func OpenBranchStore(root string, branch ...string) (*BranchStore, error) {
	store := NewBranchStore(root, branch...)
	namespace, err := store.namespaceRoot()
	if err != nil {
		return nil, err
	}
	lock := store.lock()
	lock.Lock()
	defer lock.Unlock()
	if err := secureMkdirAll(filepath.Join(namespace, "functions")); err != nil {
		return nil, fmt.Errorf("create state root: %w", err)
	}
	return store, nil
}

// functionsRoot is retained as a path-only helper for compatibility with the
// original implementation. Callers that access it must validate the path via
// namespaceRoot and ensureNoSymlink first.
func (s *BranchStore) functionsRoot() string {
	root := s.Root
	if root == "" {
		root = "."
	}
	return filepath.Join(root, s.Branch, "functions")
}

// namespaceRoot returns the canonical lexical namespace for this branch. It
// deliberately does not resolve symlinks: existing symlinks are rejected by
// the path checks below rather than silently followed.
func (s *BranchStore) namespaceRoot() (string, error) {
	root := s.Root
	if root == "" {
		root = "."
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve state root: %w", err)
	}
	if err := validateBranchName(s.Branch); err != nil {
		return "", err
	}
	namespace := filepath.Clean(filepath.Join(root, filepath.FromSlash(s.Branch)))
	rel, err := filepath.Rel(root, namespace)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid state branch %q: outside state root", s.Branch)
	}
	return namespace, nil
}

func validateBranchName(branch string) error {
	if branch == "" {
		return fmt.Errorf("invalid state branch: branch is required")
	}
	if strings.ContainsRune(branch, '\\') {
		return fmt.Errorf("invalid state branch %q: path separator is not allowed", branch)
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("invalid state branch %q: contains a control character", branch)
		}
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid state branch %q: malformed path", branch)
		}
	}
	return nil
}

func (s *BranchStore) lock() *sync.RWMutex {
	key, err := s.namespaceRoot()
	if err != nil {
		root := s.Root
		if root == "" {
			root = "."
		}
		if absolute, absErr := filepath.Abs(root); absErr == nil {
			root = absolute
		}
		key = filepath.Clean(root) + "\x00" + s.Branch
	}
	value, _ := branchLocks.LoadOrStore(key, &sync.RWMutex{})
	return value.(*sync.RWMutex)
}

func (s *BranchStore) Get(ctx context.Context, function string, id invocation.InvocationID) (*invocation.Invocation, error) {
	if err := stateContextError(ctx); err != nil {
		return nil, err
	}
	if _, err := canonicalInvocationID(function, id); err != nil {
		return nil, err
	}
	path, err := s.recordPath(function, id)
	if err != nil {
		return nil, err
	}
	lock := s.lock()
	lock.RLock()
	defer lock.RUnlock()
	return s.read(path)
}

func (s *BranchStore) Create(ctx context.Context, value invocation.Invocation) error {
	if err := stateContextError(ctx); err != nil {
		return err
	}
	if err := validateRecord(value); err != nil {
		return err
	}
	path, err := s.recordPath(value.Function, value.ID)
	if err != nil {
		return err
	}
	lock := s.lock()
	lock.Lock()
	defer lock.Unlock()
	if err := ensureNoSymlink(path, true); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check state record: %w", err)
	}
	return s.write(path, value)
}

func (s *BranchStore) CompareAndSwap(ctx context.Context, previous, next invocation.Invocation) error {
	if err := stateContextError(ctx); err != nil {
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
	path, err := s.recordPath(previous.Function, previous.ID)
	if err != nil {
		return err
	}
	lock := s.lock()
	lock.Lock()
	defer lock.Unlock()
	current, err := s.read(path)
	if err != nil {
		return err
	}
	if current == nil || !sameInvocation(*current, previous) {
		return ErrConflict
	}
	return s.write(path, next)
}

func (s *BranchStore) List(ctx context.Context, function string, limit int) ([]invocation.Invocation, error) {
	if err := stateContextError(ctx); err != nil {
		return nil, err
	}
	if function != "" {
		if err := validateFunction(function); err != nil {
			return nil, err
		}
	}
	namespace, err := s.namespaceRoot()
	if err != nil {
		return nil, err
	}
	lock := s.lock()
	lock.RLock()
	defer lock.RUnlock()

	functionsRoot := filepath.Join(namespace, "functions")
	if err := ensureNoSymlink(functionsRoot, true); err != nil {
		return nil, err
	}
	functions := []string{function}
	if function == "" {
		entries, err := os.ReadDir(functionsRoot)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return []invocation.Invocation{}, nil
			}
			return nil, fmt.Errorf("list state functions: %w", err)
		}
		functions = functions[:0]
		for _, entry := range entries {
			entryPath := filepath.Join(functionsRoot, entry.Name())
			if err := ensureNoSymlink(entryPath, false); err != nil {
				return nil, err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%w: %s", errSymlinkPath, entryPath)
			}
			if entry.IsDir() {
				if err := validateFunction(entry.Name()); err != nil {
					return nil, err
				}
				functions = append(functions, entry.Name())
			}
		}
	}

	items := make([]invocation.Invocation, 0)
	for _, fn := range functions {
		if err := stateContextError(ctx); err != nil {
			return nil, err
		}
		values, err := s.listFunction(filepath.Join(functionsRoot, fn), fn)
		if err != nil {
			return nil, err
		}
		items = append(items, values...)
	}
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

func (s *BranchStore) listFunction(path, function string) ([]invocation.Invocation, error) {
	if err := ensureNoSymlink(path, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list state records: %w", err)
	}
	items := make([]invocation.Invocation, 0, len(entries))
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		if err := ensureNoSymlink(entryPath, false); err != nil {
			return nil, err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %s", errSymlinkPath, entryPath)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		value, err := s.read(entryPath)
		if err != nil {
			return nil, err
		}
		if value == nil || value.Function != function {
			return nil, fmt.Errorf("%w: state path identity mismatch", invocation.ErrInvalidInvocation)
		}
		items = append(items, *value)
	}
	return items, nil
}

func (s *BranchStore) recordPath(function string, id invocation.InvocationID) (string, error) {
	canonical, err := canonicalInvocationID(function, id)
	if err != nil {
		return "", err
	}
	namespace, err := s.namespaceRoot()
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(canonical), "/")
	return filepath.Join(namespace, "functions", function, url.PathEscape(parts[1])+".json"), nil
}

func (s *BranchStore) read(path string) (*invocation.Invocation, error) {
	if err := ensureNoSymlink(path, true); err != nil {
		return nil, err
	}
	function, id, err := s.pathIdentity(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read state record: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStateRecordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read state record: %w", err)
	}
	if int64(len(data)) > maxStateRecordBytes {
		return nil, fmt.Errorf("state record %q exceeds %d bytes", path, maxStateRecordBytes)
	}
	var value invocation.Invocation
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode state record %q: %w", path, err)
	}
	if err := validateRecord(value); err != nil {
		return nil, fmt.Errorf("validate state record %q: %w", path, err)
	}
	if value.Function != function || value.ID != id {
		return nil, fmt.Errorf("%w: state path identity mismatch", invocation.ErrInvalidInvocation)
	}
	copy := clone(value)
	return &copy, nil
}

func (s *BranchStore) pathIdentity(path string) (string, invocation.InvocationID, error) {
	namespace, err := s.namespaceRoot()
	if err != nil {
		return "", "", err
	}
	functionsRoot := filepath.Join(namespace, "functions")
	base, err := filepath.Abs(functionsRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve state path: %w", err)
	}
	full, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve state path: %w", err)
	}
	rel, err := filepath.Rel(base, full)
	if err != nil || filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: state path is outside namespace", invocation.ErrInvalidInvocation)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || !strings.HasSuffix(parts[1], ".json") {
		return "", "", fmt.Errorf("%w: malformed state path", invocation.ErrInvalidInvocation)
	}
	function := parts[0]
	encodedKey := strings.TrimSuffix(parts[1], ".json")
	key, err := url.PathUnescape(encodedKey)
	if err != nil {
		return "", "", fmt.Errorf("%w: malformed state path: %v", invocation.ErrInvalidInvocation, err)
	}
	id, err := canonicalInvocationID(function, invocation.InvocationID(function+"/"+key))
	if err != nil {
		return "", "", err
	}
	if expected := url.PathEscape(key) + ".json"; parts[1] != expected {
		return "", "", fmt.Errorf("%w: state path identity mismatch", invocation.ErrInvalidInvocation)
	}
	return function, id, nil
}

func (s *BranchStore) write(path string, value invocation.Invocation) error {
	if err := validateRecord(value); err != nil {
		return err
	}
	function, id, err := s.pathIdentity(path)
	if err != nil {
		return err
	}
	if function != value.Function || id != value.ID {
		return fmt.Errorf("%w: state path identity mismatch", invocation.ErrInvalidInvocation)
	}
	if err := secureMkdirAll(filepath.Dir(path)); err != nil {
		return fmt.Errorf("create state function directory: %w", err)
	}
	if err := ensureNoSymlink(path, true); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state record: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set state temp permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write state record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync state record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state record: %w", err)
	}
	if err := ensureNoSymlink(tmpName, false); err != nil {
		return err
	}
	if err := ensureNoSymlink(path, true); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit state record: %w", err)
	}
	return nil
}

// ensureNoSymlink validates every existing component. A missing final or
// descendant component is allowed when creating a record, but any lookup or
// filesystem error fails closed.
func ensureNoSymlink(path string, allowMissing bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve state path: %w", err)
	}
	root := filepath.VolumeName(absolute) + string(filepath.Separator)
	rel, err := filepath.Rel(root, absolute)
	if err != nil {
		return fmt.Errorf("resolve state path: %w", err)
	}
	current := root
	if rel == "." {
		return checkPathComponent(current, allowMissing)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := checkPathComponent(current, allowMissing); err != nil {
			return err
		}
	}
	return nil
}

func checkPathComponent(path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if allowMissing {
			return nil
		}
		return err
	}
	if err != nil {
		return fmt.Errorf("inspect state path %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", errSymlinkPath, path)
	}
	return nil
}

func secureMkdirAll(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve state path: %w", err)
	}
	root := filepath.VolumeName(absolute) + string(filepath.Separator)
	rel, err := filepath.Rel(root, absolute)
	if err != nil {
		return fmt.Errorf("resolve state path: %w", err)
	}
	current := root
	if rel == "." {
		return ensureNoSymlink(current, false)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create state directory %q: %w", current, err)
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return fmt.Errorf("inspect state directory %q: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", errSymlinkPath, current)
		}
		if !info.IsDir() {
			return fmt.Errorf("state path %q is not a directory", current)
		}
	}
	return nil
}

func sameInvocation(a, b invocation.Invocation) bool {
	if a.SchemaVersion != b.SchemaVersion || a.Function != b.Function || a.ID != b.ID || a.Status != b.Status || a.Attempts != b.Attempts || a.WorkflowRunID != b.WorkflowRunID || a.Trigger != b.Trigger || !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	if !sameTimePtr(a.StartedAt, b.StartedAt) || !sameTimePtr(a.CompletedAt, b.CompletedAt) {
		return false
	}
	if (a.Lease == nil) != (b.Lease == nil) || (a.Lease != nil && (a.Lease.Owner != b.Lease.Owner || !a.Lease.ExpiresAt.Equal(b.Lease.ExpiresAt))) {
		return false
	}
	if (a.Result == nil) != (b.Result == nil) {
		return false
	}
	return a.Result == nil || *a.Result == *b.Result
}

func sameTimePtr(a, b *time.Time) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || a.Equal(*b)
}

func stateContextError(ctx context.Context) error {
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

var _ Store = (*BranchStore)(nil)
