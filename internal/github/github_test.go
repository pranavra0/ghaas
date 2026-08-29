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
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{{"id": 7, "status": "completed", "conclusion": "success"}}})
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DispatchWorkflow(context.Background(), "ghaas-test.yml", "main", map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/octo/repo/actions/workflows/ghaas-test.yml/dispatches" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotBody["ref"] != "main" {
		t.Fatalf("body = %#v", gotBody)
	}
	runs, err := client.ListWorkflowRuns(context.Background(), "ghaas-test.yml", 2)
	if err != nil || len(runs) != 1 || runs[0].ID != 7 {
		t.Fatalf("runs = %#v, err = %v", runs, err)
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
