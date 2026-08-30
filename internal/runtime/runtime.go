// Package runtime executes a manifest function in the current process environment.
// It is the small entrypoint used by generated workflows; control-plane state and
// provider concerns do not belong here.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pranavra0/ghaas/internal/invocation"
	"github.com/pranavra0/ghaas/pkg/manifest"
)

const DefaultTimeout = 15 * time.Minute

// Loader reads and validates a manifest. The main command supplies config.Load;
// keeping it injectable makes runtime invocation deterministic in tests.
type Loader func(string) (manifest.Manifest, error)

// Options controls one runtime invocation.
type Options struct {
	ManifestPath string
	LoadManifest Loader
	Runner       invocation.Runner
	Environment  []string
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
}

// Invoke loads function from the manifest, merges its environment over the
// ambient environment, and executes argv directly. The returned code is the
// child exit status. A non-zero child status is not itself an error.
func Invoke(ctx context.Context, function string, options Options) (int, error) {
	if ctx == nil {
		return 1, errors.New("nil context")
	}
	function = strings.TrimSpace(function)
	if function == "" {
		return 1, errors.New("function name is required")
	}
	path := options.ManifestPath
	if path == "" {
		path = "ghaas.yaml"
	}
	if options.LoadManifest == nil {
		return 1, errors.New("manifest loader is not configured")
	}
	m, err := options.LoadManifest(path)
	if err != nil {
		return 1, err
	}
	fn, ok := m.Functions[function]
	if !ok {
		return 1, fmt.Errorf("function %q not found", function)
	}
	if len(fn.Command) == 0 || strings.TrimSpace(fn.Command[0]) == "" {
		return 1, fmt.Errorf("function %q command is required", function)
	}

	timeout := time.Duration(fn.EffectiveTimeout(m.Defaults))
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ambient := options.Environment
	if ambient == nil {
		ambient = options.Runner.Env
	}
	if ambient == nil {
		ambient = os.Environ()
	}
	id, err := invocationID(function, ambient)
	if err != nil {
		return 1, err
	}
	metadata := metadataEnvironment(ambient, function, id)
	runner := options.Runner
	if options.Stdin != nil {
		runner.Stdin = options.Stdin
	}
	if options.Stdout != nil {
		runner.Stdout = options.Stdout
	}
	if options.Stderr != nil {
		runner.Stderr = options.Stderr
	}
	if options.Environment != nil {
		runner.Env = append([]string(nil), options.Environment...)
	}
	code, runErr := runner.RunWithEnvironment(runContext, fn.Command, merge(fn.Environment, metadata))
	if runErr == nil {
		return code, nil
	}
	if runContext.Err() != nil {
		return 1, fmt.Errorf("function %q: %w", function, runContext.Err())
	}
	if code >= 0 {
		return code, nil
	}
	return 1, fmt.Errorf("function %q: %w", function, runErr)
}

func invocationID(function string, ambient []string) (string, error) {
	value := strings.TrimSpace(environmentValue(ambient, "GHAAS_INVOCATION_ID"))
	if value != "" {
		if strings.ContainsAny(value, "\r\n") {
			return "", errors.New("invalid GHAAS_INVOCATION_ID")
		}
		// workflow_dispatch carries only the UUID input. The function scope is
		// added once at the runtime boundary so commands always receive the
		// canonical logical ID.
		prefix := function + "/"
		if strings.HasPrefix(value, prefix) {
			return value, nil
		}
		return prefix + value, nil
	}
	if value := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ID")); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return "", fmt.Errorf("invalid GITHUB_RUN_ID %q", value)
		}
		return function + "/" + value, nil
	}
	// Local execution has no provider run ID. Keep the fallback deterministic so
	// repeated runtime calls can still be identified without random state.
	return function + "/local", nil
}

func metadataEnvironment(ambient []string, function, id string) map[string]string {
	trigger := strings.TrimSpace(environmentValue(ambient, "GITHUB_EVENT_NAME"))
	switch trigger {
	case "workflow_dispatch":
		trigger = "manual"
	case "schedule":
		trigger = "schedule"
	case "":
		trigger = "manual"
	}
	attempt := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ATTEMPT"))
	if attempt == "" {
		attempt = "1"
	}
	runID := strings.TrimSpace(environmentValue(ambient, "GITHUB_RUN_ID"))
	return map[string]string{
		"GHAAS_FUNCTION":        function,
		"GHAAS_INVOCATION_ID":   id,
		"GHAAS_ATTEMPT":         attempt,
		"GHAAS_TRIGGER":         trigger,
		"GHAAS_WORKFLOW_RUN_ID": runID,
	}
}

func environmentValue(environment []string, key string) string {
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

func merge(manifestEnv, metadata map[string]string) map[string]string {
	values := make(map[string]string, len(manifestEnv)+len(metadata))
	for key, value := range manifestEnv {
		values[key] = value
	}
	for key, value := range metadata {
		values[key] = value
	}
	return values
}
