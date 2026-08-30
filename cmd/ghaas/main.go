package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pranavra0/ghaas/internal/cli"
	"github.com/pranavra0/ghaas/internal/compiler"
	"github.com/pranavra0/ghaas/internal/config"
	"github.com/pranavra0/ghaas/internal/github"
	"github.com/pranavra0/ghaas/internal/invocation"
	ghaasruntime "github.com/pranavra0/ghaas/internal/runtime"
	"github.com/pranavra0/ghaas/pkg/manifest"
)

// ReleaseVersion is replaced by release builds with -ldflags -X.
var ReleaseVersion = "v0.1.0"

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
		return ghaasruntime.Invoke(ctx, args[2], ghaasruntime.Options{
			ManifestPath: "ghaas.yaml",
			LoadManifest: config.Load,
			Runner:       invocation.Runner{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Env: os.Environ()},
		})
	}
	if err := cli.Run(ctx, args, services()); err != nil {
		return 1, err
	}
	return 0, nil
}

func services() cli.Services {
	return cli.Services{
		ManifestPath: "ghaas.yaml",
		LoadManifest: config.Load,
		Validate:     config.Validate,
		Compile: func(m manifest.Manifest, function string) (compiler.Artifact, error) {
			fn, ok := m.Functions[function]
			if !ok {
				return compiler.Artifact{}, fmt.Errorf("function %q not found", function)
			}
			return compiler.Compile(function, fn, compiler.Options{
				GhaasVersion:   ReleaseVersion,
				DefaultTimeout: time.Duration(m.Defaults.Timeout),
			})
		},
		NewGitHub:   func() (github.GitHub, error) { return github.NewClientFromEnv() },
		DefaultRef:  "",
		WorkflowFor: func(function string) string { return filepath.Base(compiler.WorkflowPath(function)) },
		Version:     ReleaseVersion,
	}
}
