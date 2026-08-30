package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/pranavra0/ghaas/internal/compiler"
	"github.com/pranavra0/ghaas/internal/github"
	"github.com/pranavra0/ghaas/pkg/manifest"
)

const defaultManifest = "ghaas.yaml"

// Services are the seams between handlers and manifest/compiler/provider
// packages. The production command wires these once; tests can replace them
// without needing a network or process-global state.
type Services struct {
	ManifestPath string
	LoadManifest func(path string) (manifest.Manifest, error)
	Validate     func(manifest.Manifest) error
	Compile      func(manifest.Manifest, string) (compiler.Artifact, error)

	GitHub       github.GitHub
	NewGitHub    func() (github.GitHub, error)
	DefaultRef   string
	WorkflowFor  func(function string) string
	Version      string
	InitTemplate string
	Stdout       io.Writer
}

type commandSpec struct {
	Name    string
	Summary string
	Usage   string
	Options string
	Flags   []string
	Example []string
	Hidden  bool
}

// commandTable is the source of truth for help and completion output.
var commandTable = []commandSpec{
	{Name: "init", Summary: "create a starter manifest", Usage: "ghaas init [--force]", Options: "--force  overwrite an existing ghaas.yaml", Flags: []string{"--force"}, Example: []string{"ghaas init"}},
	{Name: "validate", Summary: "validate ghaas.yaml", Usage: "ghaas validate", Example: []string{"ghaas validate"}},
	{Name: "generate", Summary: "print generated workflow YAML", Usage: "ghaas generate [FUNCTION]", Example: []string{"ghaas generate", "ghaas generate weekly"}},
	{Name: "deploy", Summary: "write generated workflows", Usage: "ghaas deploy [--check] [--function FUNCTION]", Options: "--check                 verify generated workflows without writing\n--function FUNCTION     deploy one function", Flags: []string{"--check", "--function"}, Example: []string{"ghaas deploy", "ghaas deploy --check"}},
	{Name: "invoke", Summary: "dispatch a function workflow", Usage: "ghaas invoke [--ref REF] FUNCTION", Options: "--ref REF               dispatch ref (empty uses the repository default)", Flags: []string{"--ref"}, Example: []string{"ghaas invoke weekly", "ghaas invoke weekly --ref release"}},
	{Name: "status", Summary: "show a workflow run", Usage: "ghaas status FUNCTION [--invocation ID]", Options: "--invocation ID         exact logical invocation ID", Flags: []string{"--invocation"}, Example: []string{"ghaas status weekly", "ghaas status weekly --invocation weekly/UUID"}},
	{Name: "logs", Summary: "show workflow logs", Usage: "ghaas logs FUNCTION [--invocation ID]", Options: "--invocation ID         exact logical invocation ID or provider run ID", Flags: []string{"--invocation"}, Example: []string{"ghaas logs weekly", "ghaas logs weekly --invocation weekly/UUID"}},
	{Name: "version", Summary: "print the release version", Usage: "ghaas version", Example: []string{"ghaas version"}},
	{Name: "completion", Summary: "print shell completion", Usage: "ghaas completion {bash|zsh|fish}", Example: []string{"ghaas completion bash"}},
	{Name: "runtime invoke", Summary: "execute a function (workflow use only)", Usage: "ghaas runtime invoke FUNCTION", Hidden: true},
}

type Root struct{ services Services }

func NewRoot(services Services) *Root {
	if services.ManifestPath == "" {
		services.ManifestPath = defaultManifest
	}
	if services.Stdout == nil {
		services.Stdout = os.Stdout
	}
	if services.WorkflowFor == nil {
		services.WorkflowFor = func(function string) string { return "ghaas-" + function + ".yml" }
	}
	if services.InitTemplate == "" {
		services.InitTemplate = "version: 1\n\ndefaults:\n  timeout: 15m\n\nfunctions:\n  hello:\n    runtime: command\n    command:\n      - echo\n      - hello\n"
	}
	return &Root{services: services}
}

func (r *Root) Execute(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		r.printRootHelp()
		return nil
	}
	if args[0] == "--version" || args[0] == "-v" {
		return r.version(nil)
	}
	switch args[0] {
	case "init":
		if commandHelp(args[1:]) {
			r.printCommandHelp("init")
			return nil
		}
		return r.init(args[1:])
	case "validate":
		if commandHelp(args[1:]) {
			r.printCommandHelp("validate")
			return nil
		}
		return r.validate(args[1:])
	case "generate":
		if commandHelp(args[1:]) {
			r.printCommandHelp("generate")
			return nil
		}
		return r.generate(args[1:])
	case "deploy":
		if commandHelp(args[1:]) {
			r.printCommandHelp("deploy")
			return nil
		}
		return r.deploy(args[1:])
	case "invoke":
		if commandHelp(args[1:]) {
			r.printCommandHelp("invoke")
			return nil
		}
		return r.invoke(ctx, args[1:])
	case "status":
		if commandHelp(args[1:]) {
			r.printCommandHelp("status")
			return nil
		}
		return r.status(ctx, args[1:])
	case "logs":
		if commandHelp(args[1:]) {
			r.printCommandHelp("logs")
			return nil
		}
		return r.logs(ctx, args[1:])
	case "version":
		if commandHelp(args[1:]) {
			r.printCommandHelp("version")
			return nil
		}
		return r.version(args[1:])
	case "completion":
		if commandHelp(args[1:]) {
			r.printCommandHelp("completion")
			return nil
		}
		return r.completion(args[1:])
	default:
		return fmt.Errorf("unknown command %q (try --help)", args[0])
	}
}

func commandHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func (r *Root) printRootHelp() {
	fmt.Fprintln(r.services.Stdout, styledHeading(r.services.Stdout, "ghaas — GitHub Actions as a service"))
	fmt.Fprintln(r.services.Stdout, "")
	fmt.Fprintln(r.services.Stdout, "Usage:")
	fmt.Fprintln(r.services.Stdout, "  ghaas <command> [options]")
	fmt.Fprintln(r.services.Stdout, "")
	fmt.Fprintln(r.services.Stdout, "Commands:")
	for _, spec := range commandTable {
		if spec.Hidden {
			continue
		}
		fmt.Fprintf(r.services.Stdout, "  %-12s %s\n", spec.Name, spec.Summary)
	}
	fmt.Fprintln(r.services.Stdout, "")
	fmt.Fprintln(r.services.Stdout, "Options:")
	fmt.Fprintln(r.services.Stdout, "  -h, --help   show help")
	fmt.Fprintln(r.services.Stdout, "  -v, --version   print the release version")
	fmt.Fprintln(r.services.Stdout, "")
	fmt.Fprintln(r.services.Stdout, "Examples:")
	fmt.Fprintln(r.services.Stdout, "  ghaas init && ghaas validate")
	fmt.Fprintln(r.services.Stdout, "  ghaas deploy --check")
	fmt.Fprintln(r.services.Stdout, "  ghaas invoke weekly --ref main")
}

func styledHeading(out io.Writer, text string) string {
	if _, disabled := os.LookupEnv("NO_COLOR"); disabled {
		return text
	}
	file, ok := out.(*os.File)
	if !ok {
		return text
	}
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return text
	}
	return "\033[1m" + text + "\033[0m"
}

func (r *Root) printCommandHelp(name string) {
	for _, spec := range commandTable {
		if spec.Name != name {
			continue
		}
		fmt.Fprintf(r.services.Stdout, "%s\n\nUsage:\n  %s\n", styledHeading(r.services.Stdout, spec.Summary), spec.Usage)
		if spec.Options != "" {
			fmt.Fprintf(r.services.Stdout, "\nOptions:\n  %s\n", strings.ReplaceAll(spec.Options, "\n", "\n  "))
		}
		if len(spec.Example) > 0 {
			fmt.Fprintln(r.services.Stdout, "\nExamples:")
			for _, example := range spec.Example {
				fmt.Fprintf(r.services.Stdout, "  %s\n", example)
			}
		}
		return
	}
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// normalizeFlags allows the familiar command FUNCTION --flag value syntax
// while retaining flag.FlagSet's strict unknown-option handling.
func normalizeFlags(args []string) []string {
	flags, positionals := make([]string, 0, len(args)), make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
		} else {
			positionals = append(positionals, arg)
		}
	}
	return append(flags, positionals...)
}

func (r *Root) loadAndValidate() (manifest.Manifest, error) {
	if r.services.LoadManifest == nil {
		return manifest.Manifest{}, errors.New("manifest loader is not configured")
	}
	m, err := r.services.LoadManifest(r.services.ManifestPath)
	if err != nil {
		return manifest.Manifest{}, err
	}
	if r.services.Validate != nil {
		if err := r.services.Validate(m); err != nil {
			return manifest.Manifest{}, err
		}
	}
	return m, nil
}

func selectedFunctions(m manifest.Manifest, requested string) ([]string, error) {
	if requested != "" {
		if _, ok := m.Functions[requested]; !ok {
			return nil, fmt.Errorf("function %q not found", requested)
		}
		return []string{requested}, nil
	}
	names := make([]string, 0, len(m.Functions))
	for name := range m.Functions {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errors.New("manifest contains no functions")
	}
	return names, nil
}

// Run is the convenience entrypoint used by cmd/ghaas.
func Run(ctx context.Context, args []string, services Services) error {
	return NewRoot(services).Execute(ctx, args)
}
