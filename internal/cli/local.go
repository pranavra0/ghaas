package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (r *Root) init(args []string) error {
	fs := newFlags("init")
	force := fs.Bool("force", false, "overwrite an existing manifest")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid init options")
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
	if err := writeAtomic(r.services.ManifestPath, []byte(r.services.InitTemplate)); err != nil {
		return err
	}
	fmt.Fprintf(r.services.Stdout, "created %s\n", r.services.ManifestPath)
	return nil
}

func (r *Root) validate(args []string) error {
	fs := newFlags("validate")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid validate options")
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

func (r *Root) generate(args []string) error {
	fs := newFlags("generate")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid generate options")
	}
	if fs.NArg() > 1 {
		return errors.New("generate accepts at most one function name")
	}
	m, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	requested := ""
	if fs.NArg() == 1 {
		requested = fs.Arg(0)
	}
	functions, err := selectedFunctions(m, requested)
	if err != nil {
		return err
	}
	if r.services.Compile == nil {
		return errors.New("compiler is not configured")
	}
	for i, function := range functions {
		artifact, compileErr := r.services.Compile(m, function)
		if compileErr != nil {
			return compileErr
		}
		if len(functions) > 1 {
			if i > 0 {
				fmt.Fprintln(r.services.Stdout, "---")
			}
			fmt.Fprintf(r.services.Stdout, "# %s\n", function)
		}
		fmt.Fprint(r.services.Stdout, string(artifact.Content))
		if len(artifact.Content) > 0 && artifact.Content[len(artifact.Content)-1] != '\n' {
			fmt.Fprintln(r.services.Stdout)
		}
	}
	return nil
}

func (r *Root) deploy(args []string) error {
	fs := newFlags("deploy")
	check := fs.Bool("check", false, "check generated workflows without writing")
	function := fs.String("function", "", "deploy one function")
	if err := fs.Parse(normalizeFlags(args)); err != nil {
		return errors.New("invalid deploy options")
	}
	if fs.NArg() != 0 {
		return errors.New("deploy does not accept positional arguments; use --function")
	}
	m, err := r.loadAndValidate()
	if err != nil {
		return err
	}
	functions, err := selectedFunctions(m, *function)
	if err != nil {
		return err
	}
	if r.services.Compile == nil {
		return errors.New("compiler is not configured")
	}
	type pending struct {
		name, path string
		content    []byte
	}
	pendingWorkflows := make([]pending, 0, len(functions))
	for _, name := range functions {
		artifact, compileErr := r.services.Compile(m, name)
		if compileErr != nil {
			return compileErr
		}
		filename := artifact.Path
		if filename == "" {
			filename = r.services.WorkflowFor(name)
		}
		filename = filepath.Clean(filename)
		path := filename
		slash := filepath.ToSlash(filename)
		if !strings.HasPrefix(slash, ".github/workflows/") {
			if filepath.IsAbs(filename) || filename == "." || filename == ".." || strings.HasPrefix(filename, ".."+string(filepath.Separator)) {
				return fmt.Errorf("invalid generated workflow path %q", filename)
			}
			path = filepath.Join(".github", "workflows", filename)
		}
		if filepath.IsAbs(path) {
			return fmt.Errorf("invalid generated workflow path %q", path)
		}
		pendingWorkflows = append(pendingWorkflows, pending{name: name, path: path, content: append([]byte(nil), artifact.Content...)})
	}
	for _, workflow := range pendingWorkflows {
		if err := rejectSymlinkPath(workflow.path); err != nil {
			return err
		}
		existing, readErr := os.ReadFile(workflow.path)
		matches := readErr == nil && string(existing) == string(workflow.content)
		displayPath := filepath.ToSlash(workflow.path)
		if *check {
			if !matches {
				if readErr != nil && !os.IsNotExist(readErr) {
					return fmt.Errorf("read %s: %w", workflow.path, readErr)
				}
				return fmt.Errorf("generated workflow differs: %s", workflow.path)
			}
			fmt.Fprintf(r.services.Stdout, "✓ %s  current\n  %s\n", workflow.name, displayPath)
			continue
		}
		if err := writeAtomic(workflow.path, workflow.content); err != nil {
			return err
		}
		fmt.Fprintf(r.services.Stdout, "✓ %s  deployed\n  %s\n", workflow.name, displayPath)
	}
	if *check && *function == "" {
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
	if err = tmp.Chmod(0o644); err == nil {
		_, err = tmp.Write(data)
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
