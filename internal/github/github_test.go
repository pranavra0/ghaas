package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientDispatchAndRuns(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "trunk"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/octo/repo/actions/workflows/ghaas-test.yml/dispatches":
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/actions/workflows/ghaas-test.yml/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{{
				"id": 7, "status": "completed", "conclusion": "success", "display_title": "alpha",
			}}})
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DispatchWorkflow(context.Background(), "ghaas-test.yml", "", map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/octo/repo/actions/workflows/ghaas-test.yml/dispatches" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotBody["ref"] != "trunk" {
		t.Fatalf("body = %#v", gotBody)
	}
	runs, err := client.ListWorkflowRuns(context.Background(), "ghaas-test.yml", 2)
	if err != nil || len(runs) != 1 || runs[0].ID != 7 || runs[0].DisplayTitle != "alpha" {
		t.Fatalf("runs = %#v, err = %v", runs, err)
	}
}

func TestClientDisplayTitleLookupPaginates(t *testing.T) {
	var requests []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/repo/actions/workflows/ghaas-test.yml/runs" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		requests = append(requests, r.URL.RawQuery)
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", `<`+srv.URL+r.URL.Path+`?per_page=100&page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{{
				"id": 1, "display_title": "ghaas: alpha/other",
			}}})
		case "2":
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{{
				"id": 2, "display_title": "ghaas: alpha/target",
			}}})
		default:
			t.Fatalf("unexpected page = %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := client.ListWorkflowRunsByDisplayTitle(context.Background(), "ghaas-test.yml", "ghaas: alpha/target")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != 2 {
		t.Fatalf("runs = %#v", runs)
	}
	if len(requests) != 2 {
		t.Fatalf("pagination requests = %#v", requests)
	}
}

func TestClientDispatchExplicitRefSkipsDefaultBranchLookup(t *testing.T) {
	var requests []string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DispatchWorkflow(context.Background(), "ghaas-test.yml", "release", nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0] != "POST /repos/octo/repo/actions/workflows/ghaas-test.yml/dispatches" {
		t.Fatalf("requests = %#v", requests)
	}
	if gotBody["ref"] != "release" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestClientEscapesEndpointSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/repos/octo/repo/actions/workflows/ghaas%2Ftest.yml/dispatches"
		if r.URL.EscapedPath() != want {
			t.Errorf("escaped path = %q, want %q", r.URL.EscapedPath(), want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DispatchWorkflow(context.Background(), "ghaas/test.yml", "main", nil); err != nil {
		t.Fatal(err)
	}
}
func TestReadLogsPlainAndZip(t *testing.T) {
	var plain bytes.Buffer
	if err := ReadLogs(&plain, bytes.NewBufferString("hello\n")); err != nil || plain.String() != "hello\n" {
		t.Fatalf("plain = %q, err = %v", plain.String(), err)
	}
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	w, err := zw.Create("run.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "from zip\n")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := ReadLogs(&output, bytes.NewReader(zipped.Bytes())); err != nil || output.String() != "from zip\n" {
		t.Fatalf("zip = %q, err = %v", output.String(), err)
	}
}
func TestGitDataRefCASAndTypedErrors(t *testing.T) {
	var force any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/git/ref/heads/state":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/state","object":{"sha":"old","type":"commit"}}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/octo/repo/git/refs/heads/state":
			var payload struct {
				SHA   string `json:"sha"`
				Force bool   `json:"force"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode update: %v", err)
			}
			force = payload.Force
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"ref changed"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := client.GetRef(context.Background(), "refs/heads/state")
	if err != nil || ref.SHA != "old" || ref.Ref != "refs/heads/state" {
		t.Fatalf("ref = %#v, err = %v", ref, err)
	}
	err = client.UpdateRef(context.Background(), "refs/heads/state", "new")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("update error = %v, want ErrConflict", err)
	}
	if force != false {
		t.Fatalf("force = %#v, want false", force)
	}
}
func TestExactWorkflowRunAttemptAndLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/repo/actions/runs/42":
			_, _ = io.WriteString(w, `{"id":42,"run_attempt":3,"status":"completed"}`)
		case "/repos/octo/repo/actions/runs/42/attempts/3":
			_, _ = io.WriteString(w, `{"id":42,"run_attempt":3,"status":"completed","conclusion":"success"}`)
		case "/repos/octo/repo/actions/runs/42/attempts/3/logs":
			_, _ = io.WriteString(w, "attempt logs\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	run, err := client.GetWorkflowRun(context.Background(), 42)
	if err != nil || run.ID != 42 || run.RunAttempt != 3 {
		t.Fatalf("run = %#v, err = %v", run, err)
	}
	run, err = client.GetWorkflowRunAttempt(context.Background(), 42, 3)
	if err != nil || run.Conclusion != "success" || run.RunAttempt != 3 {
		t.Fatalf("attempt = %#v, err = %v", run, err)
	}
	logs, err := client.GetWorkflowAttemptLogs(context.Background(), 42, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	var output bytes.Buffer
	if _, err := io.Copy(&output, logs); err != nil || output.String() != "attempt logs\n" {
		t.Fatalf("logs = %q, err = %v", output.String(), err)
	}
}
func TestGetTreeWalksNestedStatePathWithoutRecursiveExpansion(t *testing.T) {
	t.Run("nested path", func(t *testing.T) {
		var requests []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				t.Fatalf("tree query = %q, want none", r.URL.RawQuery)
			}
			requests = append(requests, r.URL.Path)
			switch r.URL.Path {
			case "/repos/octo/repo/git/trees/root":
				_ = json.NewEncoder(w).Encode(map[string]any{"sha": "root", "tree": []map[string]any{
					{"path": "unrelated", "type": "blob", "sha": "other"},
					{"path": ".ghaas", "type": "tree", "sha": "ghaas"},
				}})
			case "/repos/octo/repo/git/trees/ghaas":
				_ = json.NewEncoder(w).Encode(map[string]any{"sha": "ghaas", "tree": []map[string]any{
					{"path": "state", "type": "tree", "sha": "state"},
				}})
			case "/repos/octo/repo/git/trees/state":
				_ = json.NewEncoder(w).Encode(map[string]any{"sha": "state", "tree": []map[string]any{
					{"path": "v1.json", "type": "blob", "sha": "blob"},
				}})
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		client, err := NewClient(server.URL, "octo", "repo", "token")
		if err != nil {
			t.Fatal(err)
		}
		tree, err := client.GetTree(context.Background(), "root")
		if err != nil {
			t.Fatal(err)
		}
		if len(tree.Entries) != 1 || tree.Entries[0].Path != ".ghaas/state/v1.json" || tree.Entries[0].SHA != "blob" {
			t.Fatalf("tree entries = %#v", tree.Entries)
		}
		want := []string{
			"/repos/octo/repo/git/trees/root",
			"/repos/octo/repo/git/trees/ghaas",
			"/repos/octo/repo/git/trees/state",
		}
		if len(requests) != len(want) {
			t.Fatalf("tree requests = %#v, want %#v", requests, want)
		}
		for i := range want {
			if requests[i] != want[i] {
				t.Fatalf("tree request %d = %q, want %q", i, requests[i], want[i])
			}
		}
	})

	t.Run("truncated exact component fails closed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/octo/repo/git/trees/root" {
				t.Fatalf("unexpected tree path = %q", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"truncated": true, "tree": []map[string]any{
				{"path": ".ghaas", "type": "tree", "sha": "ghaas"},
			}})
		}))
		defer server.Close()
		client, err := NewClient(server.URL, "octo", "repo", "token")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.GetTree(context.Background(), "root"); err == nil {
			t.Fatal("truncated tree unexpectedly succeeded")
		}
	})

	t.Run("malformed exact component fails closed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/octo/repo/git/trees/root" {
				_ = json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]any{
					{"path": ".ghaas", "type": "tree", "sha": "ghaas"},
				}})
				return
			}
			if r.URL.Path == "/repos/octo/repo/git/trees/ghaas" {
				_ = json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]any{
					{"path": "state", "type": "tree", "sha": "state"},
				}})
				return
			}
			if r.URL.Path == "/repos/octo/repo/git/trees/state" {
				_ = json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]any{
					{"path": "v1.json", "type": "blob"},
				}})
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()
		client, err := NewClient(server.URL, "octo", "repo", "token")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.GetTree(context.Background(), "root"); err == nil {
			t.Fatal("malformed state entry unexpectedly succeeded")
		}
	})

}
