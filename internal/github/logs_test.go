package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientWorkflowLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/octo/repo/actions/runs/42/logs" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte("logs\n"))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	body, err := client.GetWorkflowLogs(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "logs\n" {
		t.Fatalf("logs = %q, err = %v", data, err)
	}
}
