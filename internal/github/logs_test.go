package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestClientWorkflowLogsFollowsRedirectWithoutLeakingToken(t *testing.T) {
	var gotAuthorization string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("redirected logs\n"))
	}))
	defer final.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", final.URL+"/signed?token=example")
		w.WriteHeader(http.StatusFound)
	}))
	defer api.Close()

	client, err := NewClient(api.URL, "octo", "repo", "secret")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	body, err := client.GetWorkflowLogs(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "redirected logs\n" {
		t.Fatalf("logs = %q, err = %v", data, err)
	}
	if gotAuthorization != "" {
		t.Fatalf("authorization leaked to signed log URL: %q", gotAuthorization)
	}
}

func TestClientWorkflowLogsNeverRestoresAuthorizationAfterCrossOriginRedirect(t *testing.T) {
	var gotAuthorization string
	var apiURL string
	signed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", apiURL+"/final")
		w.WriteHeader(http.StatusFound)
	}))
	defer signed.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/repo/actions/runs/42/logs":
			w.Header().Set("Location", signed.URL+"/signed")
			w.WriteHeader(http.StatusFound)
		case "/final":
			gotAuthorization = r.Header.Get("Authorization")
			_, _ = w.Write([]byte("redirected logs\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	apiURL = api.URL

	client, err := NewClient(api.URL, "octo", "repo", "secret")
	if err != nil {
		t.Fatal(err)
	}
	body, err := client.GetWorkflowLogs(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "redirected logs\n" {
		t.Fatalf("logs = %q, err = %v", data, err)
	}
	if gotAuthorization != "" {
		t.Fatalf("authorization restored after cross-origin redirect: %q", gotAuthorization)
	}
}

func TestClientWorkflowLogsRejectsRedirectUserinfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://user:password@example.test/signed")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetWorkflowLogs(context.Background(), 42); err == nil {
		t.Fatal("userinfo redirect was accepted")
	} else if strings.Contains(err.Error(), "password") {
		t.Fatalf("redirect error leaked userinfo: %v", err)
	}
}
func TestClientWorkflowLogsRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "67108865")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "octo", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetWorkflowLogs(context.Background(), 42); err == nil {
		t.Fatal("GetWorkflowLogs unexpectedly accepted oversized response")
	}
}
