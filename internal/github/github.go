// Package github contains the provider-neutral GitHub Actions client used by
// the CLI. The small interface keeps the control plane independent of the
// HTTP implementation and makes command handlers straightforward to test.
package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GitHub is the subset of the GitHub Actions API needed by ghaas.
type GitHub interface {
	DispatchWorkflow(ctx context.Context, workflow, ref string, inputs map[string]string) error
	ListWorkflowRuns(ctx context.Context, workflow string, limit int) ([]WorkflowRun, error)
	GetWorkflowLogs(ctx context.Context, runID int64) (io.ReadCloser, error)
}

// WorkflowRun is the provider-neutral representation of an Actions run.
type WorkflowRun struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name,omitempty"`
	Workflow   string     `json:"workflow,omitempty"`
	Status     string     `json:"status,omitempty"`
	Conclusion string     `json:"conclusion,omitempty"`
	Event      string     `json:"event,omitempty"`
	HeadBranch string     `json:"head_branch,omitempty"`
	HTMLURL    string     `json:"html_url,omitempty"`
	CreatedAt  time.Time  `json:"created_at,omitempty"`
	StartedAt  *time.Time `json:"run_started_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at,omitempty"`
}

// Repository identifies a GitHub repository.
type Repository struct {
	Owner string
	Name  string
}

func (r Repository) String() string { return r.Owner + "/" + r.Name }

// Client is a direct net/http implementation of GitHub.
type Client struct {
	BaseURL    *url.URL
	HTTPClient *http.Client
	Token      string
	Repository Repository
}

// MaxWorkflowLogBytes bounds both downloaded and expanded workflow logs.
// GitHub normally returns a small zip archive, but a malicious or broken
// endpoint must not be able to make the CLI allocate unbounded memory.
const MaxWorkflowLogBytes int64 = 64 << 20

const maxLogRedirects = 5

// Issue is the subset of a GitHub issue returned by CreateIssue.
type Issue struct {
	Number  int64  `json:"number"`
	Title   string `json:"title,omitempty"`
	Body    string `json:"body,omitempty"`
	State   string `json:"state,omitempty"`
	HTMLURL string `json:"html_url,omitempty"`
}

// IssueCreator is implemented by clients that support dead-letter issues.
// It is intentionally separate from GitHub so existing lightweight fakes do
// not need to implement optional functionality.
type IssueCreator interface {
	CreateIssue(ctx context.Context, title, body string) (Issue, error)
}

// GitHubWithIssues combines the required Actions API with optional issue
// creation for callers that need dead-letter notifications.
type GitHubWithIssues interface {
	GitHub
	IssueCreator
}

// RepositoryFile is a file in a repository's contents API. SHA can be passed
// back to PutRepositoryFile for optimistic concurrency.
type RepositoryFile struct {
	Path    string
	SHA     string
	Content []byte
}

// StateBranch is the optional branch-backed state API implemented by Client.
// It is separate from GitHub because state support is not needed by all
// callers and should not burden their fakes.
type StateBranch interface {
	GetStateFile(ctx context.Context, path string) (RepositoryFile, error)
	PutStateFile(ctx context.Context, path, message string, content []byte, sha string) (RepositoryFile, error)
}

// StateBranchName is the conventional branch used by the optional state
// helpers. Callers can use GetRepositoryFile/PutRepositoryFile for another
// branch.
const StateBranchName = "ghaas-state"

// NewClient creates a client for owner/name. baseURL is normally
// https://api.github.com; it is injectable for httptest servers.
func NewClient(baseURL, owner, name, token string) (*Client, error) {
	if !validRepositoryComponent(owner) || !validRepositoryComponent(name) {
		return nil, errors.New("github: owner and repository are required")
	}
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("github: invalid base URL %q", baseURL)
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
		if u.RawPath != "" {
			u.RawPath += "/"
		}
	}
	return &Client{BaseURL: u, HTTPClient: http.DefaultClient, Token: token, Repository: Repository{Owner: owner, Name: name}}, nil
}

// NewClientForRepository uses the supplied repository and token.
func NewClientForRepository(repo Repository, token string) (*Client, error) {
	return NewClient("", repo.Owner, repo.Name, token)
}

// NewHTTPClient is an explicit constructor spelling for callers that prefer
// to make the transport implementation visible.
func NewHTTPClient(baseURL, owner, name, token string) (*Client, error) {
	return NewClient(baseURL, owner, name, token)
}

// NewClientFromEnv discovers the repository and reads GITHUB_TOKEN.
func NewClientFromEnv() (*Client, error) {
	repo, err := DiscoverRepository()
	if err != nil {
		return nil, err
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("github: GITHUB_TOKEN is not set")
	}
	return NewClientForRepository(repo, token)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient == nil {
		return http.DefaultClient
	}
	return c.HTTPClient
}

func (c *Client) endpoint(parts ...string) string {
	u := *c.BaseURL
	decodedBase := strings.TrimSuffix(u.Path, "/")
	escapedBase := strings.TrimSuffix(u.EscapedPath(), "/")
	decoded := make([]string, 0, len(parts))
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		decoded = append(decoded, part)
		escaped = append(escaped, url.PathEscape(part))
	}
	u.Path = decodedBase + "/" + strings.Join(decoded, "/")
	u.RawPath = escapedBase + "/" + strings.Join(escaped, "/")
	return u.String()
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Response, error) {
	resp, err := c.do(ctx, method, endpoint, body, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.responseError(method, endpoint, resp)
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, body io.Reader, authorize bool) (*http.Response, error) {
	return c.doWithHTTPClient(c.httpClient(), ctx, method, endpoint, body, authorize)
}

func (c *Client) doWithHTTPClient(client *http.Client, ctx context.Context, method, endpoint string, body io.Reader, authorize bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ghaas")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if authorize && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return client.Do(req)
}

func (c *Client) responseError(method, endpoint string, resp *http.Response) error {
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = resp.Status
	}
	return fmt.Errorf("github: %s %s: %s", method, redactURL(endpoint), message)
}

func redactURL(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// DispatchWorkflow triggers workflow_dispatch for ref (usually the default
// branch). GitHub returns 204 on success.
func (c *Client) DispatchWorkflow(ctx context.Context, workflow, ref string, inputs map[string]string) error {
	if workflow == "" {
		return errors.New("github: workflow is required")
	}
	if ref == "" {
		ref = "main"
	}
	payload := struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs,omitempty"`
	}{Ref: ref, Inputs: inputs}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, http.MethodPost, c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "workflows", workflow, "dispatches"), bytes.NewReader(data))
	if err != nil {
		return err
	}
	// A successful dispatch normally has no body. Drain a small response body
	// anyway so keep-alive connections can be reused with test doubles and
	// GitHub-compatible servers that return one.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
	return nil
}

// ListWorkflowRuns lists at most limit runs for workflow. A non-positive limit
// uses GitHub's default page size.
func (c *Client) ListWorkflowRuns(ctx context.Context, workflow string, limit int) ([]WorkflowRun, error) {
	if workflow == "" {
		return nil, errors.New("github: workflow is required")
	}
	u, err := url.Parse(c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "workflows", workflow, "runs"))
	if err != nil {
		return nil, err
	}
	if limit > 0 {
		if limit > 100 {
			limit = 100
		}
		q := u.Query()
		q.Set("per_page", strconv.Itoa(limit))
		u.RawQuery = q.Encode()
	}
	resp, err := c.request(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("github: decode workflow runs: %w", err)
	}
	if limit > 0 && len(result.Runs) > limit {
		result.Runs = result.Runs[:limit]
	}
	return result.Runs, nil
}

// GetWorkflowLogs returns a bounded response body from the Actions logs
// endpoint. GitHub commonly responds with a redirect to a short-lived signed
// URL; redirects are followed even when a caller's HTTP client disables its
// own redirect handling.
func (c *Client) GetWorkflowLogs(ctx context.Context, runID int64) (io.ReadCloser, error) {
	if runID <= 0 {
		return nil, errors.New("github: run ID must be positive")
	}
	endpoint := c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "runs", strconv.FormatInt(runID, 10), "logs")
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

func (c *Client) getLogsResponse(ctx context.Context, endpoint string) (*http.Response, error) {
	current := endpoint
	client := *c.httpClient()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for redirects := 0; ; redirects++ {
		resp, err := c.doWithHTTPClient(&client, ctx, http.MethodGet, current, nil, c.sameOrigin(current))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return nil, c.responseError(http.MethodGet, current, resp)
		}
		if redirects >= maxLogRedirects {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("github: too many redirects while fetching workflow logs")
		}
		location := strings.TrimSpace(resp.Header.Get("Location"))
		_ = resp.Body.Close()
		if location == "" {
			return nil, errors.New("github: workflow logs redirect has no location")
		}
		next, err := url.Parse(location)
		if err != nil {
			return nil, fmt.Errorf("github: invalid workflow logs redirect: %w", err)
		}
		base, _ := url.Parse(current)
		next = base.ResolveReference(next)
		if (next.Scheme != "http" && next.Scheme != "https") || next.Host == "" {
			return nil, fmt.Errorf("github: invalid workflow logs redirect URL %q", redactURL(next.String()))
		}
		current = next.String()
	}
}

func (c *Client) sameOrigin(endpoint string) bool {
	target, err := url.Parse(endpoint)
	if err != nil || c.BaseURL == nil {
		return false
	}
	return strings.EqualFold(target.Scheme, c.BaseURL.Scheme) &&
		strings.EqualFold(target.Host, c.BaseURL.Host)
}

type boundedReadCloser struct {
	io.ReadCloser
	remaining int64
	exceeded  bool
}

func (r *boundedReadCloser) Read(p []byte) (int, error) {
	if r.exceeded {
		return 0, fmt.Errorf("workflow logs exceed %d bytes", MaxWorkflowLogBytes)
	}
	if r.remaining > 0 {
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.ReadCloser.Read(p)
		r.remaining -= int64(n)
		return n, err
	}
	// Probe one byte after the limit. Returning EOF here would silently
	// truncate a chunked response, so report an explicit size error instead.
	var probe [1]byte
	n, err := r.ReadCloser.Read(probe[:])
	if n > 0 {
		r.exceeded = true
		return 0, fmt.Errorf("workflow logs exceed %d bytes", MaxWorkflowLogBytes)
	}
	return 0, err
}

// ReadLogs writes an Actions log response to dst. The API returns a zip in
// production, while test doubles and GitHub-compatible servers may return
// plain text; support both without requiring callers to know the transport.
func ReadLogs(dst io.Writer, src io.Reader) error {
	data, readErr := io.ReadAll(io.LimitReader(src, MaxWorkflowLogBytes+1))
	if readErr != nil {
		return fmt.Errorf("read workflow logs: %w", readErr)
	}
	if int64(len(data)) > MaxWorkflowLogBytes {
		return fmt.Errorf("workflow logs exceed %d bytes", MaxWorkflowLogBytes)
	}
	if zr, zipErr := zip.NewReader(bytes.NewReader(data), int64(len(data))); zipErr == nil {
		var written int64
		for _, f := range zr.File {
			if f.UncompressedSize64 > uint64(MaxWorkflowLogBytes-written) {
				return fmt.Errorf("expanded workflow logs exceed %d bytes", MaxWorkflowLogBytes)
			}
			rc, openErr := f.Open()
			if openErr != nil {
				return openErr
			}
			n, copyErr := io.Copy(dst, io.LimitReader(rc, MaxWorkflowLogBytes-written+1))
			closeErr := rc.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			written += n
			if written > MaxWorkflowLogBytes {
				return fmt.Errorf("expanded workflow logs exceed %d bytes", MaxWorkflowLogBytes)
			}
		}
		return nil
	}
	n, writeErr := dst.Write(data)
	if writeErr == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return writeErr
}

// DiscoverRepository resolves owner/name from GITHUB_REPOSITORY first, then
// from the origin git remote. It deliberately rejects non-GitHub remotes.
func DiscoverRepository() (Repository, error) {
	if value := strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY")); value != "" {
		return parseRepository(value)
	}
	return discoverGitRemote()
}

func parseRepository(value string) (Repository, error) { return ParseRepository(value) }

// ParseRepository validates the canonical owner/name form.
func ParseRepository(value string) (Repository, error) {
	value = strings.TrimSpace(strings.TrimSuffix(value, "/"))
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return Repository{}, fmt.Errorf("github: repository must be owner/name, got %q", value)
	}
	owner, name := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !validRepositoryComponent(owner) || !validRepositoryComponent(name) {
		return Repository{}, fmt.Errorf("github: repository must be owner/name, got %q", value)
	}
	return Repository{Owner: owner, Name: name}, nil
}

func validRepositoryComponent(value string) bool {
	if value == "" || value == "." || value == ".." ||
		strings.ContainsAny(value, "/\\ \t\r\n:?#@%") {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// DiscoverRepositoryFromEnv is an explicit alias useful to callers that want
// to make the source of discovery clear.
func DiscoverRepositoryFromEnv() (Repository, error) { return DiscoverRepository() }

func discoverGitRemote() (Repository, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	output, err := cmd.Output()
	if err != nil {
		return Repository{}, errors.New("github: set GITHUB_REPOSITORY or configure git remote origin")
	}
	remote := strings.TrimSpace(string(output))
	if remote == "" {
		return Repository{}, errors.New("github: git remote origin is empty")
	}
	if strings.HasPrefix(strings.ToLower(remote), "git@") {
		at := strings.IndexByte(remote, '@')
		hostPath := remote[at+1:]
		colon := strings.IndexByte(hostPath, ':')
		if colon <= 0 {
			return Repository{}, fmt.Errorf("github: unsupported git remote %q", remote)
		}
		host := strings.ToLower(hostPath[:colon])
		if host != "github.com" && host != "www.github.com" {
			return Repository{}, fmt.Errorf("github: git remote host %q is not GitHub", hostPath[:colon])
		}
		remote = hostPath[colon+1:]
	} else {
		u, parseErr := url.Parse(remote)
		if parseErr != nil || u.Host == "" {
			return Repository{}, fmt.Errorf("github: unsupported git remote %q", remote)
		}
		host := strings.ToLower(u.Hostname())
		if host != "github.com" && host != "www.github.com" {
			return Repository{}, fmt.Errorf("github: git remote host %q is not GitHub", u.Host)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return Repository{}, fmt.Errorf("github: unsupported git remote %q", remote)
		}
		remote = strings.TrimPrefix(u.Path, "/")
	}
	return parseRepository(remote)
}
