// Package compiler turns ghaas function definitions into deterministic GitHub workflows.
package compiler

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"time"
)

var functionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Options controls generated workflow details. GhaasInstall is the complete,
// explicit command used by the generated installation step. The aliases are
// retained to make the small compiler convenient for callers with existing
// option naming; GhaasInstall takes precedence.
type Options struct {
	GhaasInstall        string
	InstallCommand      string
	Install             string
	GhaasInstallCommand string
	InstallURL          string
	GhaasVersion        string
	// DefaultTimeout applies when a function has no explicit timeout.
	DefaultTimeout time.Duration
	Timeout        time.Duration
}

func (o Options) DefaultTimeoutValue() time.Duration {
	if o.DefaultTimeout > 0 {
		return o.DefaultTimeout
	}
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 15 * time.Minute
}

type Artifact struct {
	Name     string
	Path     string
	Content  []byte
	Data     []byte
	Workflow []byte
	YAML     []byte
	Bytes    []byte
}

// WorkflowPath returns the deterministic path for a function workflow.
func WorkflowPath(name string) string {
	return ".github/workflows/ghaas-" + name + ".yml"
}

// Compile generates one workflow for a validated function definition.
func Compile(name string, function any, options Options) (Artifact, error) {
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
	return Artifact{Name: name, Path: WorkflowPath(name), Content: content, Data: content, Workflow: content, YAML: content, Bytes: content}, nil
}

// CompileAll generates all functions in deterministic name order. manifest may
// be a config value containing a Functions map, or the map itself.
func CompileAll(manifest any, options Options) ([]Artifact, error) {
	functions, err := functionsValue(manifest)
	if err != nil {
		return nil, err
	}
	if options.DefaultTimeout <= 0 && options.Timeout <= 0 {
		v := indirect(reflect.ValueOf(manifest))
		if v.IsValid() && v.Kind() == reflect.Struct {
			defaults := indirect(field(v, "Defaults"))
			if defaults.IsValid() {
				if timeout := durationFromReflect(field(defaults, "Timeout")); timeout > 0 {
					options.DefaultTimeout = timeout
				}
			}
		}
	}
	keys := functions.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	artifacts := make([]Artifact, 0, len(keys))
	for _, key := range keys {
		artifact, err := Compile(key.String(), functions.MapIndex(key).Interface(), options)
		if err != nil {
			return nil, fmt.Errorf("compile %q: %w", key.String(), err)
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func functionsValue(manifest any) (reflect.Value, error) {
	v := indirect(reflect.ValueOf(manifest))
	if !v.IsValid() {
		return reflect.Value{}, fmt.Errorf("manifest is nil")
	}
	if v.Kind() == reflect.Struct {
		v = indirect(field(v, "Functions"))
	}
	if !v.IsValid() || v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String {
		return reflect.Value{}, fmt.Errorf("manifest functions must be a map")
	}
	return v, nil
}

func indirect(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func field(v reflect.Value, name string) reflect.Value {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v.FieldByName(name)
}
