package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ghaas/internal/github"
)

type fakeGitHub struct {
	dispatchWorkflow string
	runs              []github.WorkflowRun
	logs              string
}

func (f *fakeGitHub) DispatchWorkflow(_ context.Context, workflow, _ string, _ map[string]string) error {
	f.dispatchWorkflow = workflow
	return nil
}
func (f *fakeGitHub) ListWorkflowRuns(_ context.Context, _ string, _ int) ([]github.WorkflowRun, error) {
	return f.runs, nil
}
func (f *fakeGitHub) GetWorkflowLogs(_ context.Context, _ int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.logs)), nil
}

func testServices(out *bytes.Buffer, gh github.GitHub) Services {
	return Services{
		ManifestPath: "ghaas.yaml",
		LoadManifest: func(string) (any, error) { return "manifest", nil },
		Validate:     func(any) error { return nil },
		FunctionNames: func(any) []string {
			return []string{"zeta", "alpha"}
		},
		Compile: func(_ any, name string) (Compiled, error) {
			return Compiled{Filename: "ghaas-" + name + ".yml", Content: "name: " + name + "\n"}, nil
		},
		GitHub:      gh,
		WorkflowFor: func(name string) string { return "ghaas-" + name + ".yml" },
		Stdout:      out,
		Stderr:      out,
	}
}

func TestDeployCheckDoesNotWrite(t *testing.T) {
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	root := NewRoot(testServices(&out, nil))
	if err := root.Execute(context.Background(), []string{"deploy", "--check"}); err == nil {
		t.Fatal("expected check to report missing workflow")
	}
	if _, err := os.Stat(".github"); !os.IsNotExist(err) {
		t.Fatalf("check created files, stat error = %v", err)
	}
	if err := root.Execute(context.Background(), []string{"deploy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(".github", "workflows", "ghaas-alpha.yml")); err != nil {
		t.Fatal(err)
	}
	if err := root.Execute(context.Background(), []string{"deploy", "--check"}); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubHandlers(t *testing.T) {
	fake := &fakeGitHub{runs: []github.WorkflowRun{{ID: 42, Status: "completed", Conclusion: "success"}}, logs: "hello logs\n"}
	var out bytes.Buffer
	root := NewRoot(testServices(&out, fake))
	if err := root.Execute(context.Background(), []string{"invoke", "alpha"}); err != nil {
		t.Fatal(err)
	}
	if fake.dispatchWorkflow != "ghaas-alpha.yml" {
		t.Fatalf("dispatched workflow %q", fake.dispatchWorkflow)
	}
	if err := root.Execute(context.Background(), []string{"status", "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := root.Execute(context.Background(), []string{"logs", "alpha"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, expected := range []string{"dispatched alpha", "Status: success", "hello logs"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output missing %q: %s", expected, text)
		}
	}
}
