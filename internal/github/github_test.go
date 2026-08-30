package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
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
