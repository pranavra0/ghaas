package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRepositoryFileAndIssueHelpers(t *testing.T) {
	var putBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/contents/state/invocation.json":
			if r.URL.Query().Get("ref") != StateBranchName {
				t.Errorf("ref = %q", r.URL.Query().Get("ref"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "path": "state/invocation.json", "sha": "oldsha",
				"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("old")),
			})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/octo/repo/contents/state/invocation.json":
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]any{
				"path": "state/invocation.json", "sha": "newsha", "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString([]byte("new")),
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/repo/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "title": "exhausted", "html_url": "https://github.com/octo/repo/issues/9"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.GetStateFile(context.Background(), "state/invocation.json")
	if err != nil || got.SHA != "oldsha" || string(got.Content) != "old" {
		t.Fatalf("GetStateFile = %#v, %v", got, err)
	}
	updated, err := client.PutStateFile(context.Background(), "state/invocation.json", "save state", []byte("new"), "oldsha")
	if err != nil || updated.SHA != "newsha" || string(updated.Content) != "new" {
		t.Fatalf("PutStateFile = %#v, %v", updated, err)
	}
	if putBody["branch"] != StateBranchName || putBody["sha"] != "oldsha" {
		t.Fatalf("update payload = %#v", putBody)
	}
	issue, err := client.CreateIssue(context.Background(), "exhausted", "details")
	if err != nil || issue.Number != 9 || issue.HTMLURL == "" {
		t.Fatalf("CreateIssue = %#v, %v", issue, err)
	}
}
