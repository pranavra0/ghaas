// Package cli implements ghaas command handlers. Dependencies are injected so
// parsing and local commands remain usable without a live GitHub API.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ghaas/internal/github"
)

const defaultManifest = "ghaas.yaml"

// Compiled is the compiler output needed by deploy and generate.
type Compiled struct {
	Filename string
	Content  string
}

// Services are the seams between handlers and the manifest/compiler packages.
// The concrete adapters live in cmd/ghaas, keeping this package easy to test.
type Services struct {
	ManifestPath  string
	LoadManifest  func(path string) (any, error)
	Validate      func(manifest any) error
	FunctionNames func(manifest any) []string
	Compile       func(manifest any, function string) (Compiled, error)

	GitHub       github.GitHub
	NewGitHub    func() (github.GitHub, error)
	DefaultRef   string
	WorkflowFor  func(function string) string
	Version      string
	InitTemplate string
	Stdout       io.Writer
	Stderr       io.Writer
}

// Dependencies is an alternate, descriptive name for Services.
type Dependencies = Services

// Root owns process-level dependency injection and dispatches subcommands.
type Root struct {
	services Services
}

func NewRoot(services Services) *Root {
	if services.ManifestPath == "" {
		services.ManifestPath = defaultManifest
	}
	if services.Stdout == nil {
		services.Stdout = os.Stdout
	}
	if services.Stderr == nil {
		services.Stderr = os.Stderr
	}
	if services.DefaultRef == "" {
		services.DefaultRef = os.Getenv("GITHUB_REF_NAME")
		if services.DefaultRef == "" {
			services.DefaultRef = "main"
		}
	}
	if services.WorkflowFor == nil {
		services.WorkflowFor = func(function string) string { return "ghaas-" + function + ".yml" }
	}
	if services.InitTemplate == "" {
		services.InitTemplate = "version: 1\n\nfunctions:\n  hello:\n    runtime: command\n    command:\n      - echo\n      - hello\n"
	}
	return &Root{services: services}
}

// New is a concise constructor for callers embedding the handlers.
func New(services Services) *Root { return NewRoot(services) }

// Execute runs one invocation and returns an error suitable for main to print.
func (r *Root) Execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ghaas <init|validate|generate|deploy|invoke|status|logs|version>")
	}
	switch args[0] {
	case "init":
		return r.init(args[1:])
	case "validate":
		return r.validate(args[1:])
	case "generate":
		return r.generate(args[1:])
	case "deploy":
		return r.deploy(args[1:])
	case "invoke":
		return r.invoke(ctx, args[1:])
	case "status":
		return r.status(ctx, args[1:])
	case "logs":
		return r.logs(ctx, args[1:])
	case "version":
		return r.version(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// normalizeFlags permits the conventional "command ARG --flag VALUE" form
// while still using the standard library flag package.
func normalizeFlags(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}

func (r *Root) init(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "overwrite an existing manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("init does not accept positional arguments")
	}
	if err := rejectSymlinkPath(filepath.Dir(r.services.ManifestPath)); err != nil {
		return err
	}
	if info, statErr := os.Lstat(r.services.ManifestPath); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", r.services.ManifestPath)
		}
		if !*force {
			return fmt.Errorf("%s already exists", r.services.ManifestPath)
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if err := os.WriteFile(r.services.ManifestPath, []byte(r.services.InitTemplate), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", r.services.ManifestPath, err)
	}
	fmt.Fprintf(r.services.Stdout, "created %s\n", r.services.ManifestPath)
	return nil
}

func (r *Root) loadAndValidate() (any, error) {
	if r.services.LoadManifest == nil {
		return nil, errors.New("manifest loader is not configured")
	}
	manifest, err := r.services.LoadManifest(r.services.ManifestPath)
	if err != nil {
		return nil, err
	}
	if r.services.Validate != nil {
		if err := r.services.Validate(manifest); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

func (r *Root) validate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("validate does not accept positional arguments")
	}
	if _, err := r.loadAndValidate(); err != nil {
		return err
	}
	fmt.Fprintln(r.services.Stdout, "manifest valid")
	return nil
}

func (r *Root) selectedFunctions(manifest any, requested string) ([]string, error) {
	if r.services.FunctionNames == nil {
		return nil, errors.New("manifest function discovery is not configured")
	}
	all := r.services.FunctionNames(manifest)
	if requested != "" {
		for _, name := range all {
			if name == requested {
				return []string{requested}, nil
			}
		}
		return nil, fmt.Errorf("function %q not found", requested)
	}
	functions := append([]string(nil), all...)
	sort.Strings(functions)
	if len(functions) == 0 {
		return nil, errors.New("manifest contains no functions")
	}
	return functions, nil
}

func (r *Root) generate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("generate accepts at most one function name")
	}
	manifest, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	requested := ""
	if fs.NArg() == 1 {
		requested = fs.Arg(0)
	}
	functions, err := r.selectedFunctions(manifest, requested)
	if err != nil {
		return err
	}
	if r.services.Compile == nil {
		return errors.New("compiler is not configured")
	}
	for i, function := range functions {
		compiled, err := r.services.Compile(manifest, function)
		if err != nil {
			return err
		}
		if len(functions) > 1 {
			if i > 0 {
				fmt.Fprintln(r.services.Stdout)
			}
			fmt.Fprintf(r.services.Stdout, "# %s\n", function)
		}
		fmt.Fprint(r.services.Stdout, compiled.Content)
		if !strings.HasSuffix(compiled.Content, "\n") {
			fmt.Fprintln(r.services.Stdout)
		}
	}
	return nil
}

func (r *Root) deploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "check generated workflows without writing")
	function := fs.String("function", "", "deploy one function")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("deploy does not accept positional arguments; use --function")
	}
	manifest, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	functions, err := r.selectedFunctions(manifest, *function)
	if err != nil {
		return err
	}
	if r.services.Compile == nil {
		return errors.New("compiler is not configured")
	}
	type pending struct {
		name    string
		path    string
		content []byte
	}
	pendingWorkflows := make([]pending, 0, len(functions))
	for _, name := range functions {
		fmt.Fprintf(r.services.Stdout, "compiling %s\n", name)
		compiled, compileErr := r.services.Compile(manifest, name)
		if compileErr != nil {
			return compileErr
		}
		filename := compiled.Filename
		if filename == "" {
			filename = r.services.WorkflowFor(name)
		}
		filename = filepath.Clean(filename)
		path := filename
		if !strings.HasPrefix(filepath.ToSlash(filename), ".github/workflows/") {
			if filepath.IsAbs(filename) || filename == "." || strings.HasPrefix(filename, ".."+string(filepath.Separator)) || filename == ".." {
				return fmt.Errorf("invalid generated workflow path %q", filename)
			}
			path = filepath.Join(".github", "workflows", filename)
		}
		if filepath.IsAbs(path) {
			return fmt.Errorf("invalid generated workflow path %q", path)
		}
		pendingWorkflows = append(pendingWorkflows, pending{name: name, path: path, content: []byte(compiled.Content)})
	}
	for _, workflow := range pendingWorkflows {
		existing, readErr := os.ReadFile(workflow.path)
		matches := readErr == nil && string(existing) == string(workflow.content)
		if *check {
			if !matches {
				if readErr != nil && !os.IsNotExist(readErr) {
					return fmt.Errorf("read %s: %w", workflow.path, readErr)
				}
				return fmt.Errorf("generated workflow differs: %s", workflow.path)
			}
			fmt.Fprintf(r.services.Stdout, "ok %s\n", workflow.path)
			continue
		}
		fmt.Fprintf(r.services.Stdout, "writing %s\n", workflow.path)
		if err := writeAtomic(workflow.path, workflow.content); err != nil {
			return err
		}
		fmt.Fprintf(r.services.Stdout, "deployed %s\n", workflow.name)
	}
	if *check {
		expected := make(map[string]struct{}, len(pendingWorkflows))
		for _, workflow := range pendingWorkflows {
			expected[filepath.Clean(workflow.path)] = struct{}{}
		}
		existing, globErr := filepath.Glob(filepath.Join(".github", "workflows", "ghaas-*.yml"))
		if globErr != nil {
			return fmt.Errorf("scan generated workflows: %w", globErr)
		}
		for _, path := range existing {
			if _, ok := expected[filepath.Clean(path)]; !ok {
				return fmt.Errorf("generated workflow differs: stale file %s", path)
			}
		}
	}
	if *check {
		if len(functions) == 1 {
			fmt.Fprintln(r.services.Stdout, "1 workflow current")
		} else {
			fmt.Fprintf(r.services.Stdout, "%d workflows current\n", len(functions))
		}
	} else if len(functions) == 1 {
		fmt.Fprintln(r.services.Stdout, "1 function deployed")
	} else {
		fmt.Fprintf(r.services.Stdout, "%d functions deployed\n", len(functions))
	}
	return nil
}
func writeAtomic(name string, data []byte) error {
	dir := filepath.Dir(name)
	if err := rejectSymlinkPath(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := rejectSymlinkPath(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ghaas-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary workflow: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if chmodErr := tmp.Chmod(0o644); chmodErr != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary workflow permissions: %w", chmodErr)
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary workflow: %w", err)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}
	return nil
}
func rejectSymlinkPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve workflow path: %w", err)
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect workflow path %q: %w", current, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workflow path component %q is a symbolic link", current)
		}
	}
	return nil
}

func (r *Root) client() (github.GitHub, error) {
	if r.services.GitHub != nil {
		return r.services.GitHub, nil
	}
	if r.services.NewGitHub == nil {
		return nil, errors.New("GitHub client is not configured")
	}
	return r.services.NewGitHub()
}

func (r *Root) invoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("invoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	ref := fs.String("ref", r.services.DefaultRef, "git ref")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ghaas invoke [--ref REF] FUNCTION")
	}
	name := fs.Arg(0)
	manifest, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	if _, err := r.selectedFunctions(manifest, name); err != nil {
		return err
	}
	client, err := r.client()
	if err != nil {
		return err
	}
	uuid, err := newInvocationUUID()
	if err != nil {
		return err
	}
	inputs := map[string]string{"ghaas_invocation_id": uuid}
	if err := client.DispatchWorkflow(ctx, r.services.WorkflowFor(name), *ref, inputs); err != nil {
		return err
	}
	fmt.Fprintf(r.services.Stdout, "dispatched %s\n", name)
	return nil
}
func newInvocationUUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate invocation UUID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return hex.EncodeToString(raw[:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" +
		hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" + hex.EncodeToString(raw[10:]), nil
}

func (r *Root) status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ghaas status FUNCTION")
	}
	name := fs.Arg(0)
	manifest, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	if _, err := r.selectedFunctions(manifest, name); err != nil {
		return err
	}
	client, err := r.client()
	if err != nil {
		return err
	}
	runs, err := client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 10)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintln(r.services.Stdout, "Status: no runs")
		return nil
	}
	latest := runs[0]
	for _, run := range runs[1:] {
		if run.CreatedAt.After(latest.CreatedAt) {
			latest = run
		}
	}
	status := latest.Status
	if latest.Conclusion != "" {
		status = latest.Conclusion
	}
	fmt.Fprintf(r.services.Stdout, "Function: %s\n", name)
	fmt.Fprintf(r.services.Stdout, "Workflow run: %d\n", latest.ID)
	fmt.Fprintf(r.services.Stdout, "Status: %s\n", status)
	started := latest.CreatedAt
	if latest.StartedAt != nil {
		started = *latest.StartedAt
	}
	if !started.IsZero() {
		fmt.Fprintf(r.services.Stdout, "Started: %s\n", started.Format(time.RFC3339))
	}
	return nil
}

func (r *Root) logs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	invocation := fs.String("invocation", "", "logical invocation ID or workflow run ID")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ghaas logs FUNCTION [--invocation ID]")
	}
	name := fs.Arg(0)
	manifest, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	if _, err := r.selectedFunctions(manifest, name); err != nil {
		return err
	}
	client, err := r.client()
	if err != nil {
		return err
	}
	var runID int64
	if *invocation != "" {
		if parsed, parseErr := strconv.ParseInt(*invocation, 10, 64); parseErr == nil && parsed > 0 {
			runID = parsed
		} else {
			runs, listErr := client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 100)
			if listErr != nil {
				return listErr
			}
			if len(runs) == 0 {
				return fmt.Errorf("no workflow runs for %s", name)
			}
			found := false
			for _, run := range runs {
				if run.Name == *invocation || run.Workflow == *invocation {
					runID = run.ID
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("invocation %q not found for %s", *invocation, name)
			}
		}
	} else {
		runs, listErr := client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 1)
		if listErr != nil {
			return listErr
		}
		if len(runs) == 0 {
			return fmt.Errorf("no workflow runs for %s", name)
		}
		runID = runs[0].ID
	}
	reader, err := client.GetWorkflowLogs(ctx, runID)
	if err != nil {
		return err
	}
	defer reader.Close()
	return github.ReadLogs(r.services.Stdout, reader)
}

func (r *Root) version(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("version does not accept positional arguments")
	}
	version := r.services.Version
	if version == "" {
		version = "dev"
	}
	fmt.Fprintln(r.services.Stdout, version)
	return nil
}

// Run is the convenience entry point used by cmd/ghaas.
func Run(ctx context.Context, args []string, services Services) error {
	return NewRoot(services).Execute(ctx, args)
}
