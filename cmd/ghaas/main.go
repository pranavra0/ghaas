package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pranavra0/ghaas/internal/cli"
	"github.com/pranavra0/ghaas/internal/compiler"
	"github.com/pranavra0/ghaas/internal/config"
	"github.com/pranavra0/ghaas/internal/github"
	"github.com/pranavra0/ghaas/internal/invocation"
	ghaasruntime "github.com/pranavra0/ghaas/internal/runtime"
	"github.com/pranavra0/ghaas/internal/state"
	"github.com/pranavra0/ghaas/pkg/manifest"
)

// ReleaseVersion is replaced by release builds with -ldflags -X.
// Development builds deliberately identify as dev and require an explicit
// released installer version when generating workflows.
var ReleaseVersion = "dev"

const runtimeVersionEnv = "GHAAS_RUNTIME_VERSION"

func installerVersion() string {
	if ReleaseVersion == "dev" {
		return os.Getenv(runtimeVersionEnv)
	}
	return ReleaseVersion
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := execute(ctx, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghaas:", err)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func execute(ctx context.Context, args []string) (int, error) {
	if ctx == nil {
		return 1, errors.New("nil context")
	}
	if len(args) >= 2 && args[0] == "runtime" && args[1] == "invoke" {
		for _, arg := range args[2:] {
			if arg == "-h" || arg == "--help" {
				fmt.Fprintln(os.Stdout, "Usage:")
				fmt.Fprintln(os.Stdout, "  ghaas runtime invoke FUNCTION")
				return 0, nil
			}
		}
		if len(args) != 3 {
			return 1, errors.New("usage: ghaas runtime invoke FUNCTION")
		}
		client, err := github.NewClientFromEnv()
		if err != nil {
			return 1, err
		}
		store := state.New(client)
		environment := sanitizeRuntimeEnvironment()
		return ghaasruntime.Invoke(ctx, args[2], ghaasruntime.Options{
			ManifestPath: "ghaas.yaml",
			LoadManifest: config.Load,
			Environment:  environment,
			Runner:       invocation.Runner{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Env: environment},
			Store:        store,
		})
	}
	if err := cli.Run(ctx, args, services()); err != nil {
		return 1, err
	}
	return 0, nil
}

// sanitizeRuntimeEnvironment removes control-plane credentials from both the
// runtime process and the environment inherited by configured commands. The
// GitHub client is constructed before this runs, so it retains its token.
func sanitizeRuntimeEnvironment() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if key == "GITHUB_TOKEN" || key == "GH_TOKEN" || strings.HasPrefix(key, "GHAAS_STATE_") {
			_ = os.Unsetenv(key)
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func services() cli.Services {
	runtimeVersion := installerVersion()
	s := cli.Services{
		ManifestPath: "ghaas.yaml",
		LoadManifest: config.Load,
		Validate:     config.Validate,
		Compile: func(m manifest.Manifest, function string) (compiler.Artifact, error) {
			fn, ok := m.Functions[function]
			if !ok {
				return compiler.Artifact{}, fmt.Errorf("function %q not found", function)
			}
			return compiler.Compile(function, fn, compiler.Options{
				GhaasVersion:   runtimeVersion,
				DefaultTimeout: time.Duration(m.Defaults.Timeout),
			})
		},
		NewGitHub:   func() (github.GitHub, error) { return github.NewClientFromEnv() },
		DefaultRef:  "",
		WorkflowFor: func(function string) string { return filepath.Base(compiler.WorkflowPath(function)) },
		Version:     ReleaseVersion,
	}

	// Bind one provider client to both Actions calls and the Git Data-backed
	// state store. If no repository/token is available, retain the constructor
	// seam so help and local development remain usable without GitHub access.
	if client, err := github.NewClientFromEnv(); err == nil {
		s.GitHub = client
		s.State = state.New(client)
	}
	return s
}
