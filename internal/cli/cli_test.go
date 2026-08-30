package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pranavra0/ghaas/internal/compiler"
	"github.com/pranavra0/ghaas/internal/github"
	"github.com/pranavra0/ghaas/pkg/manifest"
)

type fakeGitHub struct {
	workflow string
	ref      string
	inputs   map[string]string
	runs     []github.WorkflowRun
	logs     string
	logID    int64
}

func (f *fakeGitHub) DispatchWorkflow(_ context.Context, workflow, ref string, inputs map[string]string) error {
	f.workflow, f.ref, f.inputs = workflow, ref, inputs
	return nil
}
func (f *fakeGitHub) ListWorkflowRuns(_ context.Context, _ string, _ int) ([]github.WorkflowRun, error) {
	return f.runs, nil
}
func (f *fakeGitHub) GetWorkflowLogs(_ context.Context, id int64) (io.ReadCloser, error) {
	f.logID = id
	return io.NopCloser(strings.NewReader(f.logs)), nil
}

func testManifest() manifest.Manifest {
	return manifest.Manifest{Version: 1, Functions: map[string]manifest.Function{
		"alpha": {Runtime: manifest.RuntimeCommand, Command: []string{"echo", "hello"}},
		"beta":  {Runtime: manifest.RuntimeCommand, Command: []string{"echo", "world"}},
	}}
}

func testServices(out *bytes.Buffer, gh github.GitHub) Services {
	m := testManifest()
	return Services{
		ManifestPath: "ghaas.yaml",
		LoadManifest: func(string) (manifest.Manifest, error) { return m, nil },
		Validate:     func(manifest.Manifest) error { return nil },
		Compile: func(m manifest.Manifest, name string) (compiler.Artifact, error) {
			return compiler.Artifact{Name: name, Path: filepath.Join(".github", "workflows", "ghaas-"+name+".yml"), Content: []byte("name: " + name + "\n")}, nil
		},
		GitHub:      gh,
		WorkflowFor: func(name string) string { return "ghaas-" + name + ".yml" },
		Stdout:      out,
	}
}

func TestDeployCheckDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	root := NewRoot(testServices(&out, nil))
	if err := root.Execute(context.Background(), []string{"deploy", "--check"}); err == nil {
		t.Fatal("expected check to report missing workflow")
	}
	if _, err := os.Stat(".github"); !os.IsNotExist(err) {
		t.Fatalf("check created files: %v", err)
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

func TestHelpVersionAndCompletion(t *testing.T) {
	var out bytes.Buffer
	s := testServices(&out, nil)
	s.Version = "v9.9.9"
	root := NewRoot(s)
	for _, args := range [][]string{{"--help"}, {"generate", "--help"}, {"--version"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}} {
		out.Reset()
		if err := root.Execute(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if out.Len() == 0 {
			t.Fatalf("%v produced no output", args)
		}
	}
	if !strings.Contains(out.String(), "complete -c ghaas") || !strings.Contains(out.String(), "-l ref") {
		t.Fatalf("fish completion missing command flags: %s", out.String())
	}
}

func TestHumanOutputIsPlainWithNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	s := testServices(&out, nil)
	s.Version = "v0.1.0"
	root := NewRoot(s)
	if err := root.Execute(context.Background(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("help output contains ANSI escapes: %q", out.String())
	}
}

func TestInvokeUsesUUIDInputAndRef(t *testing.T) {
	fake := &fakeGitHub{}
	var out bytes.Buffer
	root := NewRoot(testServices(&out, fake))
	if err := root.Execute(context.Background(), []string{"invoke", "alpha", "--ref", "release"}); err != nil {
		t.Fatal(err)
	}
	if fake.workflow != "ghaas-alpha.yml" || fake.ref != "release" {
		t.Fatalf("dispatch = %q %q", fake.workflow, fake.ref)
	}
	id := fake.inputs["ghaas_invocation_id"]
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("input is not UUID: %q", id)
	}
	if !strings.Contains(out.String(), "  alpha/"+id) {
		t.Fatalf("logical ID missing: %s", out.String())
	}
}

func TestStatusAndLogsMatchDisplayTitleExactly(t *testing.T) {
	fake := &fakeGitHub{runs: []github.WorkflowRun{
		{ID: 7, DisplayTitle: "ghaas: alpha/wrong", Status: "completed", Conclusion: "success"},
		{ID: 8, DisplayTitle: "ghaas: alpha/right", Status: "completed", Conclusion: "failure"},
	}, logs: "logs for exact run\n"}
	var out bytes.Buffer
	root := NewRoot(testServices(&out, fake))
	if err := root.Execute(context.Background(), []string{"status", "alpha", "--invocation", "alpha/right"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "× failed") {
		t.Fatalf("wrong run status: %s", out.String())
	}
	out.Reset()
	if err := root.Execute(context.Background(), []string{"logs", "alpha", "--invocation", "alpha/right"}); err != nil {
		t.Fatal(err)
	}
	if fake.logID != 8 || !strings.Contains(out.String(), "logs for exact run") {
		t.Fatalf("logs = %d %q", fake.logID, out.String())
	}
	if err := root.Execute(context.Background(), []string{"status", "alpha", "--invocation", "alpha/missing"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing target error = %v", err)
	}
	if err := root.Execute(context.Background(), []string{"logs", "alpha", "--invocation", "alpha/missing"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing logs error = %v", err)
	}
}
func TestInvokeDefaultRefPassesThrough(t *testing.T) {
	fake := &fakeGitHub{}
	var out bytes.Buffer
	root := NewRoot(testServices(&out, fake))
	if err := root.Execute(context.Background(), []string{"invoke", "alpha"}); err != nil {
		t.Fatal(err)
	}
	if fake.ref != "" {
		t.Fatalf("default ref = %q, want empty for provider discovery", fake.ref)
	}
}

func TestExactRunRequiresCanonicalTitleAndRejectsAmbiguity(t *testing.T) {
	runs := []github.WorkflowRun{
		{ID: 1, DisplayTitle: "alpha/right"},
		{ID: 2, DisplayTitle: "ghaas: alpha/right"},
		{ID: 3, DisplayTitle: "ghaas: alpha/right"},
	}
	if run, ok := exactRun(runs[:1], "alpha/right"); ok || run.ID != 0 {
		t.Fatal("bare display title matched explicit logical ID")
	}
	if run, ok := exactRun(runs[1:], "alpha/right"); ok || run.ID != 0 {
		t.Fatal("ambiguous canonical title matched")
	}
}

func TestStatusSanitizesRunURL(t *testing.T) {
	var out bytes.Buffer
	printRunStatus(&out, "alpha", github.WorkflowRun{
		ID:      9,
		Status:  "completed",
		HTMLURL: "https://user:password@example.test/runs/9?token=secret#fragment",
	})
	got := out.String()
	if strings.Contains(got, "password") || strings.Contains(got, "token") || strings.Contains(got, "fragment") {
		t.Fatalf("status leaked URL credentials or components: %s", got)
	}
	if !strings.Contains(got, "https://example.test/runs/9") {
		t.Fatalf("sanitized URL missing: %s", got)
	}
}

func TestStatusRejectsAmbiguousExactTitle(t *testing.T) {
	fake := &fakeGitHub{runs: []github.WorkflowRun{
		{ID: 10, DisplayTitle: "ghaas: alpha/same"},
		{ID: 11, DisplayTitle: "ghaas: alpha/same"},
	}}
	var out bytes.Buffer
	root := NewRoot(testServices(&out, fake))
	if err := root.Execute(context.Background(), []string{"status", "alpha", "--invocation", "alpha/same"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous status error = %v", err)
	}
}

func TestExplicitInvocationMustBelongToFunction(t *testing.T) {
	var out bytes.Buffer
	root := NewRoot(testServices(&out, &fakeGitHub{}))
	for _, command := range []string{"status", "logs"} {
		if err := root.Execute(context.Background(), []string{command, "alpha", "--invocation", "beta/id"}); err == nil || !strings.Contains(err.Error(), "does not belong") {
			t.Fatalf("%s accepted another function's invocation: %v", command, err)
		}
	}
}
