package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ErrNotFound indicates that GitHub did not find the requested object.
var ErrNotFound = errors.New("github: not found")

// ErrConflict indicates that a compare-and-swap ref update lost a race.
var ErrConflict = errors.New("github: conflict")
var _ GitData = (*Client)(nil)

// APIError preserves the HTTP status while retaining a bounded provider error
// message. Use errors.Is with ErrNotFound or ErrConflict for CAS decisions.
type APIError struct {
	Method     string
	Endpoint   string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("github: %s %s: HTTP %d", e.Method, redactURL(e.Endpoint), e.StatusCode)
	}
	return fmt.Sprintf("github: %s %s: %s", e.Method, redactURL(e.Endpoint), e.Message)
}

func (e *APIError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	default:
		return nil
	}
}

// MaxGitDataResponseBytes bounds JSON responses from the Git Data API. State
// is a single bounded aggregate, so accepting an unbounded tree/blob response
// would otherwise permit a provider response to exhaust the runner.
const MaxGitDataResponseBytes int64 = 16 << 20

// maxGitTreeEntries bounds the number of entries retained from one
// non-recursive tree response. GitHub's recursive tree endpoint can truncate
// unrelated entries; state reads avoid it by following only the exact state
// path components.
const maxGitTreeEntries = 100_000

// GitData is the GitHub Git Database subset used by durable state.
type GitData interface {
	GetRef(ctx context.Context, ref string) (GitRef, error)
	CreateRef(ctx context.Context, ref, sha string) error
	UpdateRef(ctx context.Context, ref, sha string) error
	GetCommit(ctx context.Context, sha string) (GitCommit, error)
	CreateCommit(ctx context.Context, message, treeSHA string, parents []string) (GitCommit, error)
	GetTree(ctx context.Context, sha string) (GitTree, error)
	CreateTree(ctx context.Context, baseTree string, entries []TreeEntry) (GitTree, error)
	GetBlob(ctx context.Context, sha string) (GitBlob, error)
	CreateBlob(ctx context.Context, content []byte) (GitBlob, error)
}

// GitRef is a Git reference and its pointed-to object.
type GitRef struct {
	Ref    string    `json:"ref"`
	SHA    string    `json:"-"`
	Object GitObject `json:"object"`
}

// Ref is retained as a short compatibility name for GitRef.
type Ref = GitRef

// GitObject is the object descriptor returned by refs and commits.
type GitObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type,omitempty"`
	URL  string `json:"url,omitempty"`
}

// GitCommit is a commit and its root tree.
type GitCommit struct {
	SHA     string      `json:"sha"`
	TreeSHA string      `json:"-"`
	Tree    GitObject   `json:"tree"`
	Parents []GitObject `json:"parents,omitempty"`
	Message string      `json:"message,omitempty"`
}

// Commit is retained as a short compatibility name for GitCommit.
type Commit = GitCommit

// TreeEntry is a file or subtree in a Git tree. Content is accepted by the
// create-tree endpoint for UTF-8 files; state normally uses BlobSHA instead.
type TreeEntry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	SHA     string `json:"sha,omitempty"`
	BlobSHA string `json:"-"`
	Size    int64  `json:"size,omitempty"`
	URL     string `json:"url,omitempty"`
	Content string `json:"content,omitempty"`
}

// GitTree is a tree and its entries. Entries is the wire-compatible name used
// by the state store; Tree is populated as a compatibility alias.
type GitTree struct {
	SHA       string      `json:"sha"`
	URL       string      `json:"url,omitempty"`
	Entries   []TreeEntry `json:"tree"`
	Tree      []TreeEntry `json:"-"`
	Truncated bool        `json:"truncated,omitempty"`
}

// Tree is retained as a short compatibility name for GitTree.
type Tree = GitTree

// GitBlob is a blob with decoded content.
type GitBlob struct {
	SHA      string `json:"sha"`
	Encoding string `json:"encoding,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Content  []byte `json:"-"`
}

// Blob is retained as a short compatibility name for GitBlob.
type Blob = GitBlob

func normalizeGitRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "refs/")
	if ref == "" || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "..") {
		return "", errors.New("github: invalid git ref")
	}
	return ref, nil
}

func requireSHA(kind, sha string) error {
	if strings.TrimSpace(sha) == "" {
		return fmt.Errorf("github: %s SHA is required", kind)
	}
	return nil
}

func (c *Client) gitEndpoint(parts ...string) string {
	return c.endpoint(append([]string{"repos", c.Repository.Owner, c.Repository.Name, "git"}, parts...)...)
}

func decodeBounded(body io.Reader, dst any) error {
	data, err := io.ReadAll(io.LimitReader(body, MaxGitDataResponseBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > MaxGitDataResponseBytes {
		return fmt.Errorf("github: response exceeds %d bytes", MaxGitDataResponseBytes)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return err
	}
	return nil
}

func (c *Client) getGitJSON(ctx context.Context, endpoint string, dst any) error {
	resp, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := decodeBounded(resp.Body, dst); err != nil {
		return fmt.Errorf("github: decode response: %w", err)
	}
	return nil
}

func (c *Client) postGitJSON(ctx context.Context, endpoint string, payload any, dst any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("github: encode request: %w", err)
	}
	resp, err := c.request(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if dst == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
		return nil
	}
	if err := decodeBounded(resp.Body, dst); err != nil {
		return fmt.Errorf("github: decode response: %w", err)
	}
	return nil
}

// GetRef reads a Git reference. Both refs/heads/name and heads/name are
// accepted; the response's canonical ref is returned unchanged.
func (c *Client) GetRef(ctx context.Context, ref string) (GitRef, error) {
	normalized, err := normalizeGitRef(ref)
	if err != nil {
		return GitRef{}, err
	}
	var out GitRef
	if err := c.getGitJSON(ctx, c.gitEndpoint("ref", normalized), &out); err != nil {
		return GitRef{}, err
	}
	out.SHA = out.Object.SHA
	if out.SHA == "" {
		return GitRef{}, errors.New("github: ref response has no object SHA")
	}
	return out, nil
}

// CreateRef creates a reference with force disabled.
func (c *Client) CreateRef(ctx context.Context, ref, sha string) error {
	normalized, err := normalizeGitRef(ref)
	if err != nil {
		return err
	}
	if err := requireSHA("ref", sha); err != nil {
		return err
	}
	payload := struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "refs/" + normalized, SHA: sha}
	return c.postGitJSON(ctx, c.gitEndpoint("refs"), payload, nil)
}

// UpdateRef performs a compare-and-swap update. force is deliberately always
// false: state writers must never overwrite a concurrent commit.
func (c *Client) UpdateRef(ctx context.Context, ref, sha string) error {
	normalized, err := normalizeGitRef(ref)
	if err != nil {
		return err
	}
	if err := requireSHA("ref", sha); err != nil {
		return err
	}
	payload := struct {
		SHA   string `json:"sha"`
		Force bool   `json:"force"`
	}{SHA: sha, Force: false}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("github: encode ref update: %w", err)
	}
	resp, err := c.request(ctx, http.MethodPatch, c.gitEndpoint("refs", normalized), bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	return nil
}

// GetCommit reads a commit and exposes its root tree SHA directly.
func (c *Client) GetCommit(ctx context.Context, sha string) (GitCommit, error) {
	if err := requireSHA("commit", sha); err != nil {
		return GitCommit{}, err
	}
	var out GitCommit
	if err := c.getGitJSON(ctx, c.gitEndpoint("commits", sha), &out); err != nil {
		return GitCommit{}, err
	}
	out.TreeSHA = out.Tree.SHA
	if out.SHA == "" || out.TreeSHA == "" {
		return GitCommit{}, errors.New("github: commit response is missing SHA or tree")
	}
	return out, nil
}

// CreateCommit creates a commit with the supplied root tree and parents.
func (c *Client) CreateCommit(ctx context.Context, message, treeSHA string, parents []string) (GitCommit, error) {
	if strings.TrimSpace(message) == "" {
		return GitCommit{}, errors.New("github: commit message is required")
	}
	if err := requireSHA("tree", treeSHA); err != nil {
		return GitCommit{}, err
	}
	for _, parent := range parents {
		if err := requireSHA("parent commit", parent); err != nil {
			return GitCommit{}, err
		}
	}
	payload := struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents,omitempty"`
	}{Message: message, Tree: treeSHA, Parents: parents}
	var out GitCommit
	if err := c.postGitJSON(ctx, c.gitEndpoint("commits"), payload, &out); err != nil {
		return GitCommit{}, err
	}
	out.TreeSHA = out.Tree.SHA
	if out.SHA == "" {
		return GitCommit{}, errors.New("github: commit response has no SHA")
	}
	return out, nil
}

// stateTreePath is deliberately resolved component by component. The state
// store asks for GetTree on the commit root, but a recursive tree response is
// allowed to truncate when unrelated repository entries are numerous.
const stateTreePath = ".ghaas/state/v1.json"

// getTreeLevel reads one tree without recursive expansion. The response cap
// bounds bytes while this entry cap bounds the amount of decoded metadata.
func (c *Client) getTreeLevel(ctx context.Context, sha string) (GitTree, error) {
	if err := requireSHA("tree", sha); err != nil {
		return GitTree{}, err
	}
	var wire struct {
		SHA       string          `json:"sha"`
		URL       string          `json:"url,omitempty"`
		Entries   json.RawMessage `json:"tree"`
		Truncated bool            `json:"truncated,omitempty"`
	}
	if err := c.getGitJSON(ctx, c.gitEndpoint("trees", sha), &wire); err != nil {
		return GitTree{}, err
	}
	entriesJSON := bytes.TrimSpace(wire.Entries)
	if len(entriesJSON) == 0 || bytes.Equal(entriesJSON, []byte("null")) {
		return GitTree{}, errors.New("github: tree response has no entries")
	}
	var entries []TreeEntry
	if err := json.Unmarshal(entriesJSON, &entries); err != nil {
		return GitTree{}, fmt.Errorf("github: decode tree entries: %w", err)
	}
	if wire.Truncated {
		return GitTree{}, errors.New("github: tree response is truncated")
	}
	if len(entries) > maxGitTreeEntries {
		return GitTree{}, fmt.Errorf("github: tree response has more than %d entries", maxGitTreeEntries)
	}
	for i := range entries {
		if entries[i].BlobSHA == "" {
			entries[i].BlobSHA = entries[i].SHA
		}
	}
	out := GitTree{SHA: wire.SHA, URL: wire.URL, Entries: entries}
	out.Tree = out.Entries
	return out, nil
}

// GetTree reads the state path by walking exact tree components. It returns
// only the final state entry, using its full path so existing state readers do
// not need to know about the provider's nested tree representation.
func (c *Client) GetTree(ctx context.Context, sha string) (GitTree, error) {
	if err := requireSHA("tree", sha); err != nil {
		return GitTree{}, err
	}
	components := strings.Split(stateTreePath, "/")
	currentSHA := sha
	resultSHA := sha
	for i, component := range components {
		tree, err := c.getTreeLevel(ctx, currentSHA)
		if err != nil {
			return GitTree{}, err
		}
		if i == 0 && strings.TrimSpace(tree.SHA) != "" {
			resultSHA = tree.SHA
		}
		var match *TreeEntry
		for j := range tree.Entries {
			entry := &tree.Entries[j]
			if entry.Path != component {
				continue
			}
			if match != nil {
				return GitTree{}, fmt.Errorf("github: tree response has duplicate state path component %q", component)
			}
			match = entry
		}
		if match == nil {
			return GitTree{SHA: resultSHA}, nil
		}
		if strings.TrimSpace(match.SHA) == "" {
			return GitTree{}, fmt.Errorf("github: state path component %q has no SHA", component)
		}
		if i < len(components)-1 {
			if match.Type != "tree" {
				return GitTree{}, fmt.Errorf("github: state path component %q is not a tree", component)
			}
			currentSHA = match.SHA
			continue
		}
		if match.Type != "blob" {
			return GitTree{}, errors.New("github: state path is not a blob")
		}
		match.Path = stateTreePath
		match.BlobSHA = match.SHA
		entries := []TreeEntry{*match}
		return GitTree{SHA: resultSHA, Entries: entries, Tree: entries}, nil
	}
	return GitTree{SHA: resultSHA}, nil
}

// CreateTree creates a tree relative to baseTree. A blank baseTree creates a
// root tree, which is useful when initializing the state ref.
func (c *Client) CreateTree(ctx context.Context, baseTree string, entries []TreeEntry) (GitTree, error) {
	for _, entry := range entries {
		if strings.TrimSpace(entry.Path) == "" || strings.TrimSpace(entry.Mode) == "" || strings.TrimSpace(entry.Type) == "" {
			return GitTree{}, errors.New("github: tree entry requires path, mode, and type")
		}
	}
	wireEntries := make([]TreeEntry, len(entries))
	copy(wireEntries, entries)
	for i := range wireEntries {
		if wireEntries[i].SHA == "" {
			wireEntries[i].SHA = wireEntries[i].BlobSHA
		}
		wireEntries[i].BlobSHA = ""
		wireEntries[i].Size = 0
		wireEntries[i].URL = ""
	}
	payload := struct {
		BaseTree string      `json:"base_tree,omitempty"`
		Tree     []TreeEntry `json:"tree"`
	}{BaseTree: baseTree, Tree: wireEntries}
	var out GitTree
	if err := c.postGitJSON(ctx, c.gitEndpoint("trees"), payload, &out); err != nil {
		return GitTree{}, err
	}
	if out.SHA == "" {
		return GitTree{}, errors.New("github: tree response has no SHA")
	}
	out.Tree = out.Entries
	return out, nil
}

// GetBlob reads and base64-decodes a blob.
func (c *Client) GetBlob(ctx context.Context, sha string) (GitBlob, error) {
	if err := requireSHA("blob", sha); err != nil {
		return GitBlob{}, err
	}
	var wire struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
		Content  string `json:"content"`
	}
	if err := c.getGitJSON(ctx, c.gitEndpoint("blobs", sha), &wire); err != nil {
		return GitBlob{}, err
	}
	if wire.SHA == "" {
		return GitBlob{}, errors.New("github: blob response has no SHA")
	}
	if wire.Encoding != "" && wire.Encoding != "base64" {
		return GitBlob{}, fmt.Errorf("github: unsupported blob encoding %q", wire.Encoding)
	}
	content, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(wire.Content), ""))
	if err != nil {
		return GitBlob{}, fmt.Errorf("github: decode blob: %w", err)
	}
	if int64(len(content)) > MaxGitDataResponseBytes {
		return GitBlob{}, fmt.Errorf("github: blob exceeds %d bytes", MaxGitDataResponseBytes)
	}
	return GitBlob{SHA: wire.SHA, Encoding: wire.Encoding, Size: wire.Size, Content: content}, nil
}

// CreateBlob creates a base64-encoded blob and returns its provider SHA.
func (c *Client) CreateBlob(ctx context.Context, content []byte) (GitBlob, error) {
	if int64(len(content)) > MaxGitDataResponseBytes {
		return GitBlob{}, fmt.Errorf("github: blob exceeds %d bytes", MaxGitDataResponseBytes)
	}
	payload := struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}{Content: base64.StdEncoding.EncodeToString(content), Encoding: "base64"}
	var out GitBlob
	if err := c.postGitJSON(ctx, c.gitEndpoint("blobs"), payload, &out); err != nil {
		return GitBlob{}, err
	}
	if out.SHA == "" {
		return GitBlob{}, errors.New("github: blob response has no SHA")
	}
	out.Content = append([]byte(nil), content...)
	out.Size = int64(len(content))
	return out, nil
}

// GetWorkflowRun retrieves one exact workflow run by numeric ID.
func (c *Client) GetWorkflowRun(ctx context.Context, runID int64) (WorkflowRun, error) {
	if runID <= 0 {
		return WorkflowRun{}, errors.New("github: run ID must be positive")
	}
	var out WorkflowRun
	if err := c.getGitJSON(ctx, c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "runs", strconv.FormatInt(runID, 10)), &out); err != nil {
		return WorkflowRun{}, err
	}
	return out, nil
}

// GetWorkflowRunAttempt retrieves one exact attempt of a workflow run.
func (c *Client) GetWorkflowRunAttempt(ctx context.Context, runID int64, attempt int) (WorkflowRun, error) {
	if runID <= 0 {
		return WorkflowRun{}, errors.New("github: run ID must be positive")
	}
	if attempt <= 0 {
		return WorkflowRun{}, errors.New("github: run attempt must be positive")
	}
	var out WorkflowRun
	if err := c.getGitJSON(ctx, c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "runs", strconv.FormatInt(runID, 10), "attempts", strconv.Itoa(attempt)), &out); err != nil {
		return WorkflowRun{}, err
	}
	return out, nil
}

// GetWorkflowAttemptLogs returns bounded logs for one exact workflow attempt.
func (c *Client) GetWorkflowAttemptLogs(ctx context.Context, runID int64, attempt int) (io.ReadCloser, error) {
	if runID <= 0 {
		return nil, errors.New("github: run ID must be positive")
	}
	if attempt <= 0 {
		return nil, errors.New("github: run attempt must be positive")
	}
	endpoint := c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "runs", strconv.FormatInt(runID, 10), "attempts", strconv.Itoa(attempt), "logs")
	resp, err := c.getLogsResponse(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > MaxWorkflowLogBytes {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("workflow logs exceed %d bytes", MaxWorkflowLogBytes)
	}
	return &boundedReadCloser{ReadCloser: resp.Body, remaining: MaxWorkflowLogBytes}, nil
}

// GetWorkflowRunAttemptLogs is an explicit alias for callers that use the API
// endpoint's terminology.
func (c *Client) GetWorkflowRunAttemptLogs(ctx context.Context, runID int64, attempt int) (io.ReadCloser, error) {
	return c.GetWorkflowAttemptLogs(ctx, runID, attempt)
}
