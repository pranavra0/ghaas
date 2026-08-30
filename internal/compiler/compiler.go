// Package compiler turns ghaas function definitions into deterministic GitHub workflows.
package compiler

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pranavra0/ghaas/pkg/manifest"
)

var functionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// releaseVersionPattern is the strict, shell-safe v-prefixed SemVer grammar
// used by generated installers and release tags.
var releaseVersionPattern = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

const (
	defaultGhaasRepository = "pranavra0/ghaas"
	defaultGhaasVersion    = "v0.1.0"
)

// Options controls the released ghaas version and generated job timeout.
type Options struct {
	GhaasVersion   string
	DefaultTimeout time.Duration
}

func (o Options) DefaultTimeoutValue() time.Duration {
	if o.DefaultTimeout > 0 {
		return o.DefaultTimeout
	}
	return 15 * time.Minute
}

func (o Options) installer() (string, error) {
	version := strings.TrimSpace(o.GhaasVersion)
	if version == "" {
		version = defaultGhaasVersion
	} else {
		version = strings.TrimPrefix(version, "@")
		if !releaseVersionPattern.MatchString(version) {
			return "", fmt.Errorf("GhaasVersion %q is not a valid release version", o.GhaasVersion)
		}
	}
	return `set -euo pipefail
version="` + version + `"
case "$(uname -m)" in
  x86_64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported runner architecture: $(uname -m)" >&2; exit 1 ;;
esac
archive="ghaas-${version}-linux-${arch}.tar.gz"
base_url="https://github.com/` + defaultGhaasRepository + `/releases/download/${version}"
release_dir="$RUNNER_TEMP/ghaas-release"
install_dir="$RUNNER_TEMP/ghaas-bin"
mkdir -p "$release_dir" "$install_dir"
curl --fail --location --silent --show-error --retry 3 \
  --output "$release_dir/$archive" "$base_url/$archive"
curl --fail --location --silent --show-error --retry 3 \
  --output "$release_dir/SHA256SUMS" "$base_url/SHA256SUMS"
(
  cd "$release_dir"
  expected="$(awk -v archive="$archive" '$2 == archive { print $1; exit }' SHA256SUMS)"
  if [[ ! "$expected" =~ ^[[:xdigit:]]{64}$ ]]; then
    echo "missing or invalid checksum for $archive" >&2
    exit 1
  fi
  printf '%s  %s\n' "$expected" "$archive" | sha256sum --check --status
)
tar --extract --gzip --file "$release_dir/$archive" --directory "$install_dir"
echo "$install_dir" >> "$GITHUB_PATH"`, nil
}

// Artifact is the single generated-workflow representation.
type Artifact struct {
	Name    string
	Path    string
	Content []byte
}

// WorkflowPath returns the deterministic path for a function workflow.
func WorkflowPath(name string) string {
	return ".github/workflows/ghaas-" + name + ".yml"
}

// Compile generates one workflow for a validated function definition.
func Compile(name string, function manifest.Function, options Options) (Artifact, error) {
	if name == "" {
		return Artifact{}, fmt.Errorf("function name is required")
	}
	if !functionNamePattern.MatchString(name) {
		return Artifact{}, fmt.Errorf("function name %q contains unsafe characters", name)
	}
	wf, err := buildWorkflow(name, function, options)
	if err != nil {
		return Artifact{}, err
	}
	content, err := renderWorkflow(wf)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Name: name, Path: WorkflowPath(name), Content: content}, nil
}

// CompileAll generates all functions in deterministic name order.
func CompileAll(m manifest.Manifest, options Options) ([]Artifact, error) {
	if options.DefaultTimeout <= 0 && m.Defaults.Timeout > 0 {
		options.DefaultTimeout = time.Duration(m.Defaults.Timeout)
	}
	names := make([]string, 0, len(m.Functions))
	for name := range m.Functions {
		names = append(names, name)
	}
	sort.Strings(names)
	artifacts := make([]Artifact, 0, len(names))
	for _, name := range names {
		artifact, err := Compile(name, m.Functions[name], options)
		if err != nil {
			return nil, fmt.Errorf("compile %q: %w", name, err)
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}
