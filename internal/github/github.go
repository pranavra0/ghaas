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

// NewClient creates a client for owner/name. baseURL is normally
// https://api.github.com; it is injectable for httptest servers.
func NewClient(baseURL, owner, name, token string) (*Client, error) {
	if owner == "" || name == "" {
		return nil, errors.New("github: owner and repository are required")
	}
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/")
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("github: invalid base URL %q", baseURL)
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
	segments := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		segments = append(segments, pathEscapeSegments(p)...)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.Join(segments, "/")
	u.RawPath = ""
	return u.String()
}

func pathEscapeSegments(s string) []string {
	// URL.Path stores decoded path text; URL.String performs the escaping.
	// Storing PathEscape output here would escape percent signs a second time.
	return []string{s}
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("github: %s %s: %s", method, endpoint, message)
	}
	return resp, nil
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
	defer resp.Body.Close()
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

// GetWorkflowLogs returns the response body from the Actions logs endpoint.
// GitHub's endpoint commonly returns a zip archive; callers may pass the
// stream to ReadLogs, which extracts a useful text stream when applicable.
func (c *Client) GetWorkflowLogs(ctx context.Context, runID int64) (io.ReadCloser, error) {
	if runID <= 0 {
		return nil, errors.New("github: run ID must be positive")
	}
	resp, err := c.request(ctx, http.MethodGet, c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "actions", "runs", strconv.FormatInt(runID, 10), "logs"), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ReadLogs writes an Actions log response to dst. The API returns a zip in
// production, while test doubles and GitHub-compatible servers may return
// plain text; support both without requiring callers to know the transport.
func ReadLogs(dst io.Writer, src io.Reader) error {
	const maxLogBytes int64 = 64 << 20
	data, readErr := io.ReadAll(io.LimitReader(src, maxLogBytes+1))
	if readErr != nil {
		return fmt.Errorf("read workflow logs: %w", readErr)
	}
	if int64(len(data)) > maxLogBytes {
		return fmt.Errorf("workflow logs exceed %d bytes", maxLogBytes)
	}
	if zr, zipErr := zip.NewReader(bytes.NewReader(data), int64(len(data))); zipErr == nil {
		var written int64
		for _, f := range zr.File {
			rc, openErr := f.Open()
			if openErr != nil {
				return openErr
			}
			n, copyErr := io.Copy(dst, io.LimitReader(rc, maxLogBytes-written+1))
			closeErr := rc.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			written += n
			if written > maxLogBytes {
				return fmt.Errorf("expanded workflow logs exceed %d bytes", maxLogBytes)
			}
		}
		return nil
	}
	_, writeErr := dst.Write(data)
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
	if owner == "" || name == "" || strings.ContainsAny(owner, " \t\r\n") || strings.ContainsAny(name, " \t\r\n") {
		return Repository{}, fmt.Errorf("github: repository must be owner/name, got %q", value)
	}
	return Repository{Owner: owner, Name: name}, nil
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
	if strings.HasPrefix(remote, "git@") {
		if at := strings.IndexByte(remote, '@'); at >= 0 {
			hostPath := remote[at+1:]
			if colon := strings.IndexByte(hostPath, ':'); colon >= 0 {
				if host := hostPath[:colon]; host != "github.com" && host != "www.github.com" {
					return Repository{}, fmt.Errorf("github: git remote host %q is not GitHub", host)
				}
				remote = hostPath[colon+1:]
			}
		}
	} else if u, parseErr := url.Parse(remote); parseErr == nil && u.Host != "" {
		if u.Host != "github.com" && u.Host != "www.github.com" {
			return Repository{}, fmt.Errorf("github: git remote host %q is not GitHub", u.Host)
		}
		remote = strings.TrimPrefix(u.Path, "/")
	} else {
		return Repository{}, fmt.Errorf("github: unsupported git remote %q", remote)
	}
	return parseRepository(remote)
}
