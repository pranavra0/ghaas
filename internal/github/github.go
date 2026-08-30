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
	ID           int64      `json:"id"`
	Name         string     `json:"name,omitempty"`
	DisplayTitle string     `json:"display_title,omitempty"`
	Workflow     string     `json:"workflow,omitempty"`
	Status       string     `json:"status,omitempty"`
	Conclusion   string     `json:"conclusion,omitempty"`
	Event        string     `json:"event,omitempty"`
	HeadBranch   string     `json:"head_branch,omitempty"`
	HTMLURL      string     `json:"html_url,omitempty"`
	CreatedAt    time.Time  `json:"created_at,omitempty"`
	StartedAt    *time.Time `json:"run_started_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at,omitempty"`
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
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// DispatchWorkflow triggers workflow_dispatch for ref. When ref is empty, the
// repository's current default branch is resolved before dispatching. GitHub
// returns 204 on success.
func (c *Client) DispatchWorkflow(ctx context.Context, workflow, ref string, inputs map[string]string) error {
	if workflow == "" {
		return errors.New("github: workflow is required")
	}
	if strings.TrimSpace(ref) == "" {
		var err error
		ref, err = c.defaultBranch(ctx)
		if err != nil {
			return fmt.Errorf("github: resolve default branch: %w", err)
		}
	}
	payload := struct {
		Ref    string            `json:"ref"`
		Inputs map[string]string `json:"inputs,omitempty"`
	}{Ref: ref, Inputs: inputs}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("github: encode workflow dispatch: %w", err)
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

func (c *Client) defaultBranch(ctx context.Context) (string, error) {
	endpoint := c.endpoint("repos", c.Repository.Owner, c.Repository.Name)
	resp, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&repository); err != nil {
		return "", fmt.Errorf("github: decode repository: %w", err)
	}
	if strings.TrimSpace(repository.DefaultBranch) == "" {
		return "", errors.New("github: repository response has no default branch")
	}
	return repository.DefaultBranch, nil
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

// ListWorkflowRunsByDisplayTitle follows GitHub pagination and returns only
// runs whose display title exactly equals displayTitle. It is an optional
// capability used by CLI explicit-invocation lookups; ListWorkflowRuns keeps
// its existing one-page/default-latest behavior.
func (c *Client) ListWorkflowRunsByDisplayTitle(ctx context.Context, workflow, displayTitle string) ([]WorkflowRun, error) {
	if workflow == "" {
		return nil, errors.New("github: workflow is required")
	}
	if displayTitle == "" {
		return nil, errors.New("github: display title is required")
	}
	u, err := url.Parse(c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "workflows", workflow, "runs"))
	if err != nil {
		return nil, err
	}
	query := u.Query()
	query.Set("per_page", "100")
	u.RawQuery = query.Encode()
	next := u.String()
	var matches []WorkflowRun
	for next != "" {
		nextURL, err := url.Parse(next)
		if err != nil || nextURL.User != nil || !c.sameOrigin(nextURL.String()) {
			return nil, fmt.Errorf("github: invalid workflow runs pagination URL %q", redactURL(next))
		}
		resp, err := c.request(ctx, http.MethodGet, nextURL.String(), nil)
		if err != nil {
			return nil, err
		}
		var result struct {
			Runs []WorkflowRun `json:"workflow_runs"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&result)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("github: decode workflow runs: %w", decodeErr)
		}
		for _, run := range result.Runs {
			if run.DisplayTitle == displayTitle {
				matches = append(matches, run)
			}
		}
		rawNext := nextLink(link)
		if rawNext == "" {
			next = ""
			continue
		}
		candidate, err := url.Parse(rawNext)
		if err != nil {
			return nil, fmt.Errorf("github: invalid workflow runs pagination URL %q", redactURL(rawNext))
		}
		next = nextURL.ResolveReference(candidate).String()
	}
	return matches, nil
}

func nextLink(header string) string {
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		if len(parts) < 2 {
			continue
		}
		relNext := false
		for _, attr := range parts[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				relNext = true
				break
			}
		}
		if !relNext {
			continue
		}
		value := strings.TrimSpace(parts[0])
		if len(value) >= 2 && value[0] == '<' && value[len(value)-1] == '>' {
			return value[1 : len(value)-1]
		}
	}
	return ""
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
	authorize := c.sameOrigin(endpoint)
	client := *c.httpClient()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	for redirects := 0; ; redirects++ {
		resp, err := c.doWithHTTPClient(&client, ctx, http.MethodGet, current, nil, authorize)
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
			return nil, fmt.Errorf("github: invalid workflow logs redirect URL %q", redactURL(location))
		}
		base, _ := url.Parse(current)
		next = base.ResolveReference(next)
		if next.User != nil || (next.Scheme != "http" && next.Scheme != "https") || next.Host == "" {
			return nil, fmt.Errorf("github: invalid workflow logs redirect URL %q", redactURL(next.String()))
		}
		if !c.sameOrigin(next.String()) {
			authorize = false
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
// from the origin git remote. Non-GitHub remotes are rejected.
func DiscoverRepository() (Repository, error) {
	if value := strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY")); value != "" {
		return ParseRepository(value)
	}
	return discoverGitRemote()
}

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
	return ParseRepository(remote)
}
