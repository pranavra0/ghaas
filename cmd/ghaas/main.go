package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ghaas/internal/cli"
	"ghaas/internal/compiler"
	"ghaas/internal/config"
	"ghaas/internal/github"
	"ghaas/internal/invocation"
	"ghaas/internal/state"
	"ghaas/pkg/manifest"
)

const version = "v0.1.0"

func main() {
	code, err := execute(context.Background(), os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghaas:", err)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func execute(ctx context.Context, args []string) (int, error) {
	if len(args) >= 2 && args[0] == "runtime" && args[1] == "invoke" {
		return runtimeInvoke(ctx, args[2:])
	}
	if err := cli.Run(ctx, args, services()); err != nil {
		return 1, err
	}
	return 0, nil
}

func runtimeInvoke(ctx context.Context, args []string) (int, error) {
	if len(args) != 1 || args[0] == "" {
		return 1, fmt.Errorf("usage: ghaas runtime invoke FUNCTION")
	}
	function := args[0]
	m, err := config.Load("ghaas.yaml")
	if err != nil {
		return 1, err
	}
	fn, ok := m.Functions[function]
	if !ok {
		return 1, fmt.Errorf("function %q not found", function)
	}
	timeout := 15 * time.Minute
	if fn.Timeout > 0 {
		timeout = time.Duration(fn.Timeout)
	} else if m.Defaults.Timeout > 0 {
		timeout = time.Duration(m.Defaults.Timeout)
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	id, err := runtimeInvocationID(function)
	if err != nil {
		return 1, err
	}
	trigger := os.Getenv("GITHUB_EVENT_NAME")
	switch trigger {
	case "", "workflow_dispatch":
		trigger = "manual"
	case "schedule":
		trigger = "schedule"
	}
	workflowRunID := os.Getenv("GITHUB_RUN_ID")
	env := append([]string(nil), os.Environ()...)
	env = withEnvironment(env, fn.Environment)
	env = withEnvironment(env, map[string]string{
		"GHAAS_FUNCTION":        function,
		"GHAAS_INVOCATION_ID":   string(id),
		"GHAAS_ATTEMPT":         "1",
		"GHAAS_TRIGGER":         trigger,
		"GHAAS_WORKFLOW_RUN_ID": workflowRunID,
	})
	runtime := invocation.Runtime{Store: state.NewMemory(), Runner: invocation.Runner{Env: env}}
	record, runErr := runtime.Invoke(runContext, function, id, fn.Command)
	if runErr != nil {
		if record.Result != nil && record.Result.ExitCode >= 0 {
			return record.Result.ExitCode, nil
		}
		return 1, runErr
	}
	return 0, nil
}

func runtimeInvocationID(function string) (invocation.InvocationID, error) {
	if value := strings.TrimSpace(os.Getenv("GHAAS_INVOCATION_ID")); value != "" {
		id := invocation.InvocationID(value)
		if err := invocation.ValidateInvocationID(id); err != nil {
			return "", fmt.Errorf("invalid GHAAS_INVOCATION_ID: %w", err)
		}
		if !strings.HasPrefix(value, function+"/") {
			return "", fmt.Errorf("GHAAS_INVOCATION_ID %q does not belong to function %q", value, function)
		}
		return id, nil
	}
	if os.Getenv("GITHUB_EVENT_NAME") == "schedule" {
		return invocation.NewScheduledID(function, time.Now().UTC().Format("2006-01-02"))
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate invocation ID: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	uuid := hex.EncodeToString(raw[:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" +
		hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" + hex.EncodeToString(raw[10:])
	return invocation.NewManualID(function, uuid)
}

func withEnvironment(base []string, values map[string]string) []string {
	index := make(map[string]int, len(base))
	for i, entry := range base {
		if key, _, ok := strings.Cut(entry, "="); ok {
			index[key] = i
		}
	}
	for key, value := range values {
		entry := key + "=" + value
		if i, ok := index[key]; ok {
			base[i] = entry
		} else {
			index[key] = len(base)
			base = append(base, entry)
		}
	}
	return base
}

func services() cli.Services {
	return cli.Services{
		ManifestPath: "ghaas.yaml",
		LoadManifest: func(path string) (any, error) { return config.Load(path) },
		Validate: func(value any) error {
			m, ok := value.(manifest.Manifest)
			if !ok {
				return fmt.Errorf("unexpected manifest type %T", value)
			}
			return config.Validate(m)
		},
		FunctionNames: func(value any) []string {
			m, ok := value.(manifest.Manifest)
			if !ok {
				return nil
			}
			names := make([]string, 0, len(m.Functions))
			for name := range m.Functions {
				names = append(names, name)
			}
			return names
		},
		Compile: func(value any, function string) (cli.Compiled, error) {
			m, ok := value.(manifest.Manifest)
			if !ok {
				return cli.Compiled{}, fmt.Errorf("unexpected manifest type %T", value)
			}
			fn, ok := m.Functions[function]
			if !ok {
				return cli.Compiled{}, fmt.Errorf("function %q not found", function)
			}
			defaultTimeout := 15 * time.Minute
			if m.Defaults.Timeout > 0 {
				defaultTimeout = time.Duration(m.Defaults.Timeout)
			}
			artifact, err := compiler.Compile(function, fn, compiler.Options{GhaasInstall: "go run ./cmd/ghaas", DefaultTimeout: defaultTimeout})
			if err != nil {
				return cli.Compiled{}, err
			}
			return cli.Compiled{Filename: filepath.Base(artifact.Path), Content: string(artifact.Content)}, nil
		},
		NewGitHub:   func() (github.GitHub, error) { return github.NewClientFromEnv() },
		WorkflowFor: func(function string) string { return filepath.Base(compiler.WorkflowPath(function)) },
		Version:     version,
	}
}
