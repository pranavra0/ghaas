package invocation

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunnerPreservesArgumentBoundariesAndExitCode(t *testing.T) {
	var out bytes.Buffer
	runner := Runner{Stdout: &out, Stderr: &out}
	code, err := runner.Run(context.Background(), []string{"printf", "%s|%s", "a b", "c"})
	if err != nil || code != 0 || out.String() != "a b|c" {
		t.Fatalf("run = code %d, err %v, output %q", code, err, out.String())
	}
	code, err = runner.Run(context.Background(), []string{"false"})
	if code != 1 || err == nil {
		t.Fatalf("false = code %d, err %v", code, err)
	}
}

func TestRunnerEnvironmentOverridesBase(t *testing.T) {
	var out bytes.Buffer
	runner := Runner{Stdout: &out, Stderr: &out, Env: []string{"VALUE=ambient", "KEEP=yes"}}
	code, err := runner.RunWithEnvironment(context.Background(), []string{"sh", "-c", "printf '%s|%s' \"$VALUE\" \"$KEEP\""}, map[string]string{"VALUE": "manifest"})
	if err != nil || code != 0 || out.String() != "manifest|yes" {
		t.Fatalf("run = code %d, err %v, output %q", code, err, out.String())
	}
}

func TestRunnerHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	code, err := (Runner{}).Run(ctx, []string{"sleep", "1"})
	if code != -1 || err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("run = code %d, err %v, want context deadline", code, err)
	}
}
