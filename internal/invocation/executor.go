package invocation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// CommandRunner is the small seam used by callers and tests.
type CommandRunner interface {
	Run(context.Context, []string) (int, error)
}

// Runner executes a command directly, never through a shell. Nil streams use
// the corresponding process-standard stream. Env, when non-nil, replaces the
// inherited environment; RunWithEnvironment applies additional values over it.
type Runner struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Dir    string
	Env    []string
}

// Run executes args while preserving each argument boundary. On a normal
// non-zero exit, the returned error is *exec.ExitError and code is the child
// exit status. A command that cannot start returns -1. If context cancellation
// caused termination, ctx.Err is returned so callers can distinguish timeout
// from an ordinary command failure.
func (r Runner) Run(ctx context.Context, args []string) (int, error) {
	return r.run(ctx, args, nil)
}

// RunWithEnvironment runs a command with values merged into its environment.
// It never invokes a shell and never mutates r.Env.
func (r Runner) RunWithEnvironment(ctx context.Context, args []string, env map[string]string) (int, error) {
	return r.run(ctx, args, env)
}

func (r Runner) run(ctx context.Context, args []string, env map[string]string) (int, error) {
	if ctx == nil {
		return -1, errors.New("nil context")
	}
	if len(args) == 0 || args[0] == "" {
		return -1, errors.New("command is required")
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = r.Stdin
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if r.Dir != "" {
		cmd.Dir = r.Dir
	}
	if r.Env != nil {
		cmd.Env = append([]string(nil), r.Env...)
	}
	if env != nil {
		base := cmd.Env
		if base == nil {
			base = os.Environ()
		}
		cmd.Env = MergeEnvironment(base, env)
	}

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), err
	}
	return -1, fmt.Errorf("start command: %w", err)
}

// Execute runs args with the process-standard streams and ambient environment.
func Execute(ctx context.Context, args []string) (int, error) {
	return (Runner{}).Run(ctx, args)
}
