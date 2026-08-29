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
	"ghaas/internal/invocation"
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

	GitHub    github.GitHub
	NewGitHub func() (github.GitHub, error)
	// State is optional for compatibility with stateless clients. When
	// configured, invoke records a pending logical invocation and status/logs
	// prefer that record over the provider's workflow-run view.
	State    invocation.StateStore
	NewState func() (invocation.StateStore, error)
	// StateFor allows applications to select a backend from the validated
	// manifest/function (for example memory versus branch state). State and
	// NewState remain the compatibility defaults.
	StateFor func(manifest any, function string) (invocation.StateStore, error)
	// NewInvocationID is injectable for deterministic tests and alternate
	// identity providers. It must return a function-scoped invocation ID.
	NewInvocationID func(function string) (invocation.InvocationID, error)
	DefaultRef      string
	WorkflowFor     func(function string) string
	Version         string
	InitTemplate    string
	Stdout          io.Writer
	Stderr          io.Writer
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
	if args[0] == "--version" || args[0] == "-v" {
		return r.version(args[1:])
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
	if err := os.MkdirAll(filepath.Dir(r.services.ManifestPath), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
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
				fmt.Fprintln(r.services.Stdout, "---")
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
		if symlinkErr := rejectSymlinkPath(workflow.path); symlinkErr != nil {
			return symlinkErr
		}
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
	if *check && *function == "" {
		// A full check owns the generated workflow set and therefore reports
		// stale ghaas-* files. A function-scoped check deliberately compares
		// only the selected artifact so unrelated functions can be checked in
		// isolation.
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
	client, err := r.services.NewGitHub()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("GitHub constructor returned nil client")
	}
	return client, nil
}

func (r *Root) stateStore(manifest any, function string) (invocation.StateStore, error) {
	if r.services.StateFor != nil {
		store, err := r.services.StateFor(manifest, function)
		if err != nil {
			return nil, err
		}
		if store == nil {
			return nil, errors.New("state selector returned nil store")
		}
		return store, nil
	}
	if r.services.State != nil {
		return r.services.State, nil
	}
	if r.services.NewState == nil {
		return nil, nil
	}
	store, err := r.services.NewState()
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("state constructor returned nil store")
	}
	// Keep one store for all handlers in this process. This is important for
	// callers that use Root as an embeddable control plane.
	r.services.State = store
	return store, nil
}

func (r *Root) invocationID(function string) (invocation.InvocationID, error) {
	if r.services.NewInvocationID != nil {
		id, err := r.services.NewInvocationID(function)
		if err != nil {
			return "", err
		}
		if err := invocation.ValidateInvocationID(id); err != nil {
			return "", fmt.Errorf("invalid invocation ID: %w", err)
		}
		if !strings.HasPrefix(string(id), function+"/") {
			return "", fmt.Errorf("invocation ID %q does not belong to function %q", id, function)
		}
		return id, nil
	}
	uuid, err := newInvocationUUID()
	if err != nil {
		return "", err
	}
	return invocation.NewManualID(function, uuid)
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
	if strings.TrimSpace(name) == "" {
		return errors.New("function name is required")
	}
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
	id, err := r.invocationID(name)
	if err != nil {
		return err
	}
	store, err := r.stateStore(manifest, name)
	if err != nil {
		return err
	}
	if store != nil {
		record := invocation.Invocation{
			SchemaVersion: 1,
			Function:      name,
			ID:            id,
			Status:        invocation.StatusPending,
			Attempts:      0,
			CreatedAt:     time.Now().UTC(),
		}
		if createErr := store.Create(ctx, record); createErr != nil {
			if !errors.Is(createErr, invocation.ErrConflict) {
				return fmt.Errorf("record invocation %s: %w", id, createErr)
			}
			// An injected deterministic ID can legitimately already exist.
			// Reusing a terminal record would dispatch a duplicate logical
			// invocation, so fail closed instead.
			existing, getErr := store.Get(ctx, name, id)
			if getErr != nil {
				return fmt.Errorf("record invocation %s: %w", id, createErr)
			}
			if existing != nil && existing.Status.IsTerminal() {
				return fmt.Errorf("invocation %s is already %s", id, existing.Status)
			}
			return fmt.Errorf("invocation %s already exists", id)
		}
	}
	// The workflow compiler declares this exact input name. Keep the UUID
	// function-scoped in the state ID while passing only its key to Actions.
	inputs := map[string]string{"ghaas_invocation_id": string(id)}
	if err := client.DispatchWorkflow(ctx, r.services.WorkflowFor(name), *ref, inputs); err != nil {
		return err
	}
	fmt.Fprintf(r.services.Stdout, "dispatched %s\n", name)
	fmt.Fprintf(r.services.Stdout, "invocation: %s\n", id)
	return nil
}
func formatInvocationStatus(out io.Writer, record invocation.Invocation) {
	fmt.Fprintln(out, "Function:", record.Function)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Last logical invocation:")
	fmt.Fprintf(out, "  ID:       %s\n", record.ID)
	fmt.Fprintf(out, "  Status:   %s\n", record.Status)
	fmt.Fprintf(out, "  Attempts: %d\n", record.Attempts)
	if record.WorkflowRunID > 0 {
		fmt.Fprintf(out, "  Workflow run: %d\n", record.WorkflowRunID)
	}
	if record.StartedAt != nil {
		fmt.Fprintf(out, "  Started:  %s\n", record.StartedAt.Format(time.RFC3339))
	}
	if record.StartedAt != nil {
		end := time.Now().UTC()
		if record.CompletedAt != nil {
			end = *record.CompletedAt
		}
		if end.After(*record.StartedAt) {
			fmt.Fprintf(out, "  Duration: %.1fs\n", end.Sub(*record.StartedAt).Seconds())
		}
	}
	if record.Result != nil && record.Result.Error != "" {
		fmt.Fprintf(out, "  Error:    %s\n", record.Result.Error)
	}
}

func latestInvocation(records []invocation.Invocation) (invocation.Invocation, bool) {
	if len(records) == 0 {
		return invocation.Invocation{}, false
	}
	latest := records[0]
	for _, candidate := range records[1:] {
		if candidate.CreatedAt.After(latest.CreatedAt) ||
			(candidate.CreatedAt.Equal(latest.CreatedAt) && string(candidate.ID) > string(latest.ID)) {
			latest = candidate
		}
	}
	return latest, true
}

type providerRunState struct {
	status  invocation.InvocationStatus
	skipped bool
}

// classifyWorkflowRun translates the provider's state vocabulary into the
// logical invocation state machine. A skipped Actions run is intentionally
// kept separate: duplicate runs do not create a logical invocation record.
func classifyWorkflowRun(run github.WorkflowRun) providerRunState {
	conclusion := strings.ToLower(strings.TrimSpace(run.Conclusion))
	switch conclusion {
	case "success":
		return providerRunState{status: invocation.StatusSucceeded}
	case "skipped":
		return providerRunState{skipped: true}
	case "failure", "cancelled", "timed_out", "action_required", "stale", "startup_failure", "neutral":
		return providerRunState{status: invocation.StatusFailed}
	}

	switch strings.ToLower(strings.TrimSpace(run.Status)) {
	case "queued", "requested", "waiting", "pending", "in_progress":
		return providerRunState{status: invocation.StatusRunning}
	case "success":
		return providerRunState{status: invocation.StatusSucceeded}
	case "failure", "cancelled", "timed_out":
		return providerRunState{status: invocation.StatusFailed}
	case "completed":
		// GitHub normally supplies a conclusion for completed runs. Treat an
		// omitted conclusion conservatively as a failed provider result rather
		// than leaving a logical invocation pending forever.
		return providerRunState{status: invocation.StatusFailed}
	default:
		return providerRunState{}
	}
}

func workflowRunTime(run github.WorkflowRun, fallback time.Time) time.Time {
	if run.StartedAt != nil && !run.StartedAt.IsZero() {
		return run.StartedAt.UTC()
	}
	if !run.CreatedAt.IsZero() {
		return run.CreatedAt.UTC()
	}
	if !fallback.IsZero() {
		return fallback.UTC()
	}
	return time.Now().UTC()
}

func workflowRunCompletionTime(run github.WorkflowRun, started time.Time) time.Time {
	completed := run.UpdatedAt
	if completed.IsZero() {
		completed = run.CreatedAt
	}
	if completed.IsZero() {
		completed = time.Now().UTC()
	}
	completed = completed.UTC()
	if !started.IsZero() && completed.Before(started) {
		completed = started
	}
	return completed
}

func workflowRunHasInvocationID(run github.WorkflowRun, id invocation.InvocationID) bool {
	value := string(id)
	name := strings.TrimSpace(run.Name)
	workflow := strings.TrimSpace(run.Workflow)
	return strings.Contains(name, value) || strings.Contains(workflow, value)
}

// workflowRunForInvocation finds the provider run corresponding to a logical
// record. A persisted run ID or provider-exposed invocation metadata is an
// exact association. For status reconciliation only, a run created after a
// pending record is a useful fallback because workflow_dispatch does not
// return a run ID; callers that require an exact target pass strict=true.
func workflowRunForInvocation(record invocation.Invocation, runs []github.WorkflowRun, strict bool) (github.WorkflowRun, bool) {
	existingRunID := record.WorkflowRunID
	previousAttemptTerminal := false
	if existingRunID > 0 {
		for _, run := range runs {
			if run.ID != existingRunID {
				continue
			}
			provider := classifyWorkflowRun(run)
			if record.Status == invocation.StatusPending && record.Attempts > 0 &&
				(provider.status == invocation.StatusSucceeded || provider.status == invocation.StatusFailed) {
				// Retry keeps the previous attempt's run ID for history. Do
				// not replay that terminal attempt while the retry is pending.
				previousAttemptTerminal = true
				break
			}
			return run, true
		}
	}
	for _, run := range runs {
		if run.ID > 0 && workflowRunHasInvocationID(run, record.ID) &&
			(!previousAttemptTerminal || run.ID != existingRunID) {
			return run, true
		}
	}
	if strict || record.CreatedAt.IsZero() {
		return github.WorkflowRun{}, false
	}
	if len(runs) == 1 && runs[0].ID > 0 && runs[0].ID != existingRunID {
		// Some provider fakes and older API adapters omit timestamps. A
		// singleton run is still an unambiguous reconciliation candidate for
		// status, while strict callers continue to reject all heuristics.
		return runs[0], true
	}

	var selected github.WorkflowRun
	var selectedAt time.Time
	found := false
	for _, run := range runs {
		created := run.CreatedAt
		if created.IsZero() && run.StartedAt != nil {
			created = *run.StartedAt
		}
		if run.ID <= 0 || run.ID == existingRunID || created.IsZero() || created.Before(record.CreatedAt) {
			continue
		}
		if !found || created.Before(selectedAt) ||
			(created.Equal(selectedAt) && run.ID < selected.ID) {
			selected = run
			selectedAt = created
			found = true
		}
	}
	return selected, found
}

func invocationTimesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func reconcileInvocation(ctx context.Context, store invocation.StateStore, current invocation.Invocation, run github.WorkflowRun) (invocation.Invocation, error) {
	if run.ID <= 0 || (current.Status != invocation.StatusPending && current.Status != invocation.StatusRunning) {
		return current, nil
	}
	provider := classifyWorkflowRun(run)
	if provider.skipped || provider.status == "" {
		return current, nil
	}

	next := current
	if current.Status == invocation.StatusPending {
		started := workflowRunTime(run, current.CreatedAt)
		if started.Before(current.CreatedAt) {
			started = current.CreatedAt
		}
		var err error
		next, err = invocation.Start(current, started, run.ID)
		if err != nil {
			return current, err
		}
	} else {
		if next.WorkflowRunID == 0 {
			next.WorkflowRunID = run.ID
		}
		if next.StartedAt == nil {
			started := workflowRunTime(run, current.CreatedAt)
			if started.Before(current.CreatedAt) {
				started = current.CreatedAt
			}
			next.StartedAt = &started
		}
	}

	if provider.status == invocation.StatusSucceeded || provider.status == invocation.StatusFailed {
		started := current.CreatedAt
		if next.StartedAt != nil {
			started = *next.StartedAt
		}
		finished := workflowRunCompletionTime(run, started)
		result := invocation.InvocationResult{ExitCode: 0}
		if provider.status == invocation.StatusFailed {
			result.ExitCode = 1
			conclusion := strings.TrimSpace(run.Conclusion)
			if conclusion == "" {
				conclusion = strings.TrimSpace(run.Status)
			}
			result.Error = "workflow run concluded " + strings.ToLower(conclusion)
		}
		var err error
		if provider.status == invocation.StatusSucceeded {
			next, err = invocation.Succeed(next, finished, result)
		} else {
			next, err = invocation.Fail(next, finished, result)
		}
		if err != nil {
			return current, err
		}
	} else if next.WorkflowRunID == current.WorkflowRunID &&
		invocationTimesEqual(next.StartedAt, current.StartedAt) {
		return current, nil
	}

	if err := store.CompareAndSwap(ctx, current, next); err != nil {
		if errors.Is(err, invocation.ErrConflict) {
			latest, getErr := store.Get(ctx, current.Function, current.ID)
			if getErr == nil && latest != nil {
				return *latest, nil
			}
		}
		return current, fmt.Errorf("reconcile invocation %s: %w", current.ID, err)
	}
	return next, nil
}

func reconcileInvocations(ctx context.Context, store invocation.StateStore, function string, runs []github.WorkflowRun, records []invocation.Invocation) ([]invocation.Invocation, error) {
	reconciled := append([]invocation.Invocation(nil), records...)
	used := make(map[int64]struct{}, len(records))
	for _, record := range records {
		if record.WorkflowRunID > 0 {
			used[record.WorkflowRunID] = struct{}{}
		}
	}
	for i, record := range records {
		if record.Function != function ||
			(record.Status != invocation.StatusPending && record.Status != invocation.StatusRunning) {
			continue
		}
		run, ok := workflowRunForInvocation(record, runs, false)
		if !ok || run.ID <= 0 {
			continue
		}
		if _, exists := used[run.ID]; exists && record.WorkflowRunID != run.ID {
			continue
		}
		used[run.ID] = struct{}{}
		next, err := reconcileInvocation(ctx, store, record, run)
		if err != nil {
			return reconciled, err
		}
		reconciled[i] = next
	}
	return reconciled, nil
}
func printReliabilityWithRuns(out io.Writer, records []invocation.Invocation, runs []github.WorkflowRun) {
	var successful, failed, skipped int
	represented := make(map[int64]struct{}, len(records))
	for _, record := range records {
		if record.WorkflowRunID > 0 {
			represented[record.WorkflowRunID] = struct{}{}
		}
		switch record.Status {
		case invocation.StatusSucceeded:
			successful++
		case invocation.StatusFailed, invocation.StatusExhausted:
			failed++
		}
	}
	for _, run := range runs {
		state := classifyWorkflowRun(run)
		if run.ID > 0 {
			if _, ok := represented[run.ID]; ok {
				if state.skipped {
					skipped++
				}
				continue
			}
		}
		switch {
		case state.skipped:
			skipped++
		case state.status == invocation.StatusSucceeded:
			successful++
		case state.status == invocation.StatusFailed:
			failed++
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Recent reliability:")
	fmt.Fprintf(out, "  successful: %d\n", successful)
	fmt.Fprintf(out, "  failed:      %d\n", failed)
	fmt.Fprintf(out, "  skipped:     %d\n", skipped)
}

func printReliability(out io.Writer, records []invocation.Invocation) {
	printReliabilityWithRuns(out, records, nil)
}

func (r *Root) status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(normalizeFlags(args)); err != nil {
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

	store, err := r.stateStore(manifest, name)
	if err != nil {
		return err
	}
	var records []invocation.Invocation
	if store != nil {
		records, err = store.List(ctx, name, 0)
		if err != nil {
			return fmt.Errorf("list invocations for %s: %w", name, err)
		}
	}

	providerConfigured := r.services.GitHub != nil || r.services.NewGitHub != nil
	needProvider := store == nil || len(records) == 0
	for _, record := range records {
		if record.Status == invocation.StatusPending || record.Status == invocation.StatusRunning {
			needProvider = true
			break
		}
	}
	var runs []github.WorkflowRun
	runsLoaded := false
	if providerConfigured && (needProvider || store != nil) {
		client, clientErr := r.client()
		if clientErr != nil {
			if len(records) == 0 || needProvider {
				return clientErr
			}
		} else {
			runs, err = client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 10)
			if err != nil {
				if len(records) == 0 || needProvider {
					return err
				}
			} else {
				runsLoaded = true
				if store != nil && len(records) > 0 {
					records, err = reconcileInvocations(ctx, store, name, runs, records)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	if latest, ok := latestInvocation(records); ok {
		formatInvocationStatus(r.services.Stdout, latest)
		printReliabilityWithRuns(r.services.Stdout, records, runs)
		return nil
	}

	if !runsLoaded {
		client, err := r.client()
		if err != nil {
			return err
		}
		runs, err = client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 10)
		if err != nil {
			return err
		}
		runsLoaded = true
	}

	if len(runs) == 0 {
		fmt.Fprintln(r.services.Stdout, "Status: no runs")
		return nil
	}
	latest := runs[0]
	for _, run := range runs[1:] {
		if run.CreatedAt.After(latest.CreatedAt) ||
			(run.CreatedAt.Equal(latest.CreatedAt) && run.ID > latest.ID) {
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
	printReliabilityWithRuns(r.services.Stdout, nil, runs)
	return nil
}

func (r *Root) logs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	invocationID := fs.String("invocation", "", "logical invocation ID or workflow run ID")
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

	store, err := r.stateStore(manifest, name)
	if err != nil {
		return err
	}
	explicit := strings.TrimSpace(*invocationID)
	var logical *invocation.Invocation
	var runID int64
	if explicit != "" {
		if parsed, parseErr := strconv.ParseInt(explicit, 10, 64); parseErr == nil && parsed > 0 {
			// A numeric --invocation is an explicit provider run ID.
			runID = parsed
		} else {
			id := invocation.InvocationID(explicit)
			if err := invocation.ValidateInvocationID(id); err != nil {
				return fmt.Errorf("invalid invocation ID %q: %w", explicit, err)
			}
			if !strings.HasPrefix(explicit, name+"/") {
				return fmt.Errorf("invocation %q does not belong to function %q", explicit, name)
			}
			// Keep a synthetic record when no state backend is configured so
			// provider-exposed invocation metadata can still resolve this
			// explicit target. Without metadata, strict resolution below
			// correctly refuses to guess.
			logical = &invocation.Invocation{Function: name, ID: id}
			if store != nil {
				record, getErr := store.Get(ctx, name, id)
				if getErr != nil {
					if !errors.Is(getErr, invocation.ErrNotFound) {
						return getErr
					}
					return fmt.Errorf("invocation %q not found for %s", explicit, name)
				}
				if record == nil {
					return fmt.Errorf("invocation %q not found for %s", explicit, name)
				}
				logical = record
			}
			runID = logical.WorkflowRunID
		}
	} else if store != nil {
		records, listErr := store.List(ctx, name, 0)
		if listErr != nil {
			return fmt.Errorf("list invocations for %s: %w", name, listErr)
		}
		if latest, ok := latestInvocation(records); ok {
			logical = &latest
			runID = latest.WorkflowRunID
		}
	}

	client, err := r.client()
	if err != nil {
		return err
	}
	if runID == 0 {
		runs, listErr := client.ListWorkflowRuns(ctx, r.services.WorkflowFor(name), 100)
		if listErr != nil {
			return listErr
		}
		if explicit != "" {
			// An explicit logical ID is a strict target. The provider API does
			// not offer a universal input lookup, so only persisted run IDs or
			// provider-exposed metadata may establish this association.
			if logical == nil {
				return fmt.Errorf("invocation %q has no associated workflow run", explicit)
			}
			run, ok := workflowRunForInvocation(*logical, runs, true)
			if !ok || run.ID <= 0 {
				return fmt.Errorf("invocation %q has no associated workflow run", explicit)
			}
			runID = run.ID
		} else {
			if len(runs) == 0 {
				if logical != nil {
					return fmt.Errorf("invocation %s has no workflow run", logical.ID)
				}
				return fmt.Errorf("no workflow runs for %s", name)
			}
			if logical != nil {
				if run, ok := workflowRunForInvocation(*logical, runs, false); ok {
					runID = run.ID
				}
			}
			if runID == 0 {
				latest := runs[0]
				for _, run := range runs[1:] {
					if run.CreatedAt.After(latest.CreatedAt) ||
						(run.CreatedAt.Equal(latest.CreatedAt) && run.ID > latest.ID) {
						latest = run
					}
				}
				runID = latest.ID
			}
		}
	}
	if runID <= 0 {
		if logical != nil {
			return fmt.Errorf("invocation %s has no workflow run", logical.ID)
		}
		return fmt.Errorf("no workflow run for %s", name)
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
