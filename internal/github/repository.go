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
	"net/url"
	"strings"
)

const maxRepositoryResponseBytes int64 = 32 << 20

// Branch identifies a Git reference returned by the Git database API.
type Branch struct {
	Name string
	SHA  string
}

// GetRepositoryFile reads a file through GitHub's repository contents API.
// ref may be empty to use the repository's default branch.
func (c *Client) GetRepositoryFile(ctx context.Context, path, ref string) (RepositoryFile, error) {
	endpoint, err := c.contentsEndpoint(path, ref)
	if err != nil {
		return RepositoryFile{}, err
	}
	resp, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RepositoryFile{}, err
	}
	defer resp.Body.Close()
	data, err := readRepositoryResponse(resp.Body)
	if err != nil {
		return RepositoryFile{}, err
	}
	var payload struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return RepositoryFile{}, fmt.Errorf("github: decode repository file: %w", err)
	}
	if payload.Type != "" && payload.Type != "file" {
		return RepositoryFile{}, fmt.Errorf("github: repository path %q is not a file", path)
	}
	content, err := decodeRepositoryContent(payload.Encoding, payload.Content)
	if err != nil {
		return RepositoryFile{}, fmt.Errorf("github: decode repository file %q: %w", path, err)
	}
	if payload.Path == "" {
		payload.Path = strings.Trim(path, "/")
	}
	return RepositoryFile{Path: payload.Path, SHA: payload.SHA, Content: content}, nil
}

// GetContents is an alias using the name from GitHub's REST API.
func (c *Client) GetContents(ctx context.Context, path, ref string) (RepositoryFile, error) {
	return c.GetRepositoryFile(ctx, path, ref)
}

// PutRepositoryFile creates or updates a file. Supplying the current SHA
// enables GitHub's optimistic concurrency check; an empty SHA creates a file.
func (c *Client) PutRepositoryFile(ctx context.Context, path, ref, message string, content []byte, sha string) (RepositoryFile, error) {
	endpoint, err := c.contentsEndpoint(path, "")
	if err != nil {
		return RepositoryFile{}, err
	}
	payload := struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch,omitempty"`
		SHA     string `json:"sha,omitempty"`
	}{Message: message, Content: base64.StdEncoding.EncodeToString(content), Branch: ref, SHA: sha}
	if strings.TrimSpace(message) == "" {
		return RepositoryFile{}, errors.New("github: commit message is required")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return RepositoryFile{}, err
	}
	resp, err := c.request(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return RepositoryFile{}, err
	}
	defer resp.Body.Close()
	responseData, err := readRepositoryResponse(resp.Body)
	if err != nil {
		return RepositoryFile{}, err
	}
	var result struct {
		Content struct {
			Path     string `json:"path"`
			SHA      string `json:"sha"`
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(responseData, &result); err != nil {
		return RepositoryFile{}, fmt.Errorf("github: decode repository update: %w", err)
	}
	updated, err := decodeRepositoryContent(result.Content.Encoding, result.Content.Content)
	if err != nil {
		return RepositoryFile{}, fmt.Errorf("github: decode repository update %q: %w", path, err)
	}
	if result.Content.Path == "" {
		result.Content.Path = strings.Trim(path, "/")
	}
	return RepositoryFile{Path: result.Content.Path, SHA: result.Content.SHA, Content: updated}, nil
}

// CreateOrUpdateFile follows the argument order used by common GitHub REST
// clients while retaining the lower-level PutRepositoryFile helper.
func (c *Client) CreateOrUpdateFile(ctx context.Context, path string, content []byte, message, branch, sha string) (RepositoryFile, error) {
	return c.PutRepositoryFile(ctx, path, branch, message, content, sha)
}

// GetStateFile reads a file from the conventional ghaas state branch.
func (c *Client) GetStateFile(ctx context.Context, path string) (RepositoryFile, error) {
	return c.GetRepositoryFile(ctx, path, StateBranchName)
}

// PutStateFile writes a file to the conventional ghaas state branch.
func (c *Client) PutStateFile(ctx context.Context, path, message string, content []byte, sha string) (RepositoryFile, error) {
	return c.PutRepositoryFile(ctx, path, StateBranchName, message, content, sha)
}

// GetBranch resolves a branch name to its commit SHA.
func (c *Client) GetBranch(ctx context.Context, branch string) (Branch, error) {
	parts, err := branchPath(branch)
	if err != nil {
		return Branch{}, err
	}
	endpoint := c.endpoint(append([]string{"repos", c.Repository.Owner, c.Repository.Name, "git", "ref", "heads"}, parts...)...)
	resp, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Branch{}, err
	}
	defer resp.Body.Close()
	var result struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRepositoryResponseBytes)).Decode(&result); err != nil {
		return Branch{}, fmt.Errorf("github: decode branch: %w", err)
	}
	name := strings.TrimPrefix(result.Ref, "refs/heads/")
	if name == "" {
		name = branch
	}
	if result.Object.SHA == "" {
		return Branch{}, errors.New("github: branch response has no commit SHA")
	}
	return Branch{Name: name, SHA: result.Object.SHA}, nil
}

// CreateBranch creates branch from an existing commit SHA. GitHub's git refs
// API requires a SHA rather than a branch name, so callers can use GetBranch
// first when they want to fork from a branch.
func (c *Client) CreateBranch(ctx context.Context, branch, sha string) (Branch, error) {
	parts, err := branchPath(branch)
	if err != nil {
		return Branch{}, err
	}
	if strings.TrimSpace(sha) == "" {
		return Branch{}, errors.New("github: commit SHA is required")
	}
	payload := struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "refs/heads/" + strings.Join(parts, "/"), SHA: sha}
	data, err := json.Marshal(payload)
	if err != nil {
		return Branch{}, err
	}
	endpoint := c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "git", "refs")
	resp, err := c.request(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Branch{}, err
	}
	defer resp.Body.Close()
	var result struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRepositoryResponseBytes)).Decode(&result); err != nil {
		return Branch{}, fmt.Errorf("github: decode branch creation: %w", err)
	}
	name := strings.TrimPrefix(result.Ref, "refs/heads/")
	if name == "" {
		name = strings.Join(parts, "/")
	}
	if result.Object.SHA == "" {
		result.Object.SHA = sha
	}
	return Branch{Name: name, SHA: result.Object.SHA}, nil
}

// CreateIssue creates a repository issue, useful for an exhausted invocation
// dead-letter notification. Labels and assignees are intentionally left to
// the caller's separate GitHub automation rather than expanding this small
// abstraction.
func (c *Client) CreateIssue(ctx context.Context, title, body string) (Issue, error) {
	if strings.TrimSpace(title) == "" {
		return Issue{}, errors.New("github: issue title is required")
	}
	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body,omitempty"`
	}{Title: title, Body: body}
	data, err := json.Marshal(payload)
	if err != nil {
		return Issue{}, err
	}
	endpoint := c.endpoint("repos", c.Repository.Owner, c.Repository.Name, "issues")
	resp, err := c.request(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Issue{}, err
	}
	defer resp.Body.Close()
	responseData, err := readRepositoryResponse(resp.Body)
	if err != nil {
		return Issue{}, err
	}
	var issue Issue
	if err := json.Unmarshal(responseData, &issue); err != nil {
		return Issue{}, fmt.Errorf("github: decode issue: %w", err)
	}
	return issue, nil
}

func (c *Client) contentsEndpoint(path, ref string) (string, error) {
	parts, err := repositoryPath(path)
	if err != nil {
		return "", err
	}
	endpoint := c.endpoint(append([]string{"repos", c.Repository.Owner, c.Repository.Name, "contents"}, parts...)...)
	if ref == "" {
		return endpoint, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("ref", ref)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func branchPath(branch string) ([]string, error) {
	parts, err := repositoryPath(branch)
	if err != nil {
		return nil, errors.New("github: branch name is required")
	}
	return parts, nil
}

func repositoryPath(path string) ([]string, error) {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil, errors.New("github: repository file path is required")
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if !validPathComponent(part) {
			return nil, fmt.Errorf("github: invalid repository file path %q", path)
		}
	}
	return parts, nil
}

func validPathComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func decodeRepositoryContent(encoding, content string) ([]byte, error) {
	if content == "" {
		if encoding == "none" {
			return nil, errors.New("GitHub did not return file content (file may be too large)")
		}
		return nil, nil
	}
	if encoding != "" && !strings.EqualFold(encoding, "base64") {
		return nil, fmt.Errorf("unsupported content encoding %q", encoding)
	}
	compact := strings.Join(strings.Fields(content), "")
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func readRepositoryResponse(src io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(src, maxRepositoryResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("github: read repository response: %w", err)
	}
	if int64(len(data)) > maxRepositoryResponseBytes {
		return nil, fmt.Errorf("github: repository response exceeds %d bytes", maxRepositoryResponseBytes)
	}
	return data, nil
}
