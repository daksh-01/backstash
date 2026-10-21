// Package execx runs external tools (pg_dump, docker, redis-cli, tar). Targets depend on the
// Runner interface so their command lines can be tested without any of those installed.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd is one process to run. Env holds extra KEY=VALUE pairs on top of our own environment,
// which is how passwords reach tools without ever appearing in argv.
type Cmd struct {
	Name   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
}

// InContainer rewrites c to run inside a container through `docker exec`. Every variable in c.Env
// is forwarded by name (`-e NAME`), so values reach the container without ever appearing in argv.
// `-i` is added only when c has stdin.
func InContainer(container string, cmd Cmd) Cmd {
	args := []string{"exec"}
	if cmd.Stdin != nil {
		args = append(args, "-i")
	}
	for _, kv := range cmd.Env {
		name, _, _ := strings.Cut(kv, "=")
		args = append(args, "-e", name)
	}
	args = append(args, container, cmd.Name)
	return Cmd{Name: "docker", Args: append(args, cmd.Args...), Env: cmd.Env, Stdin: cmd.Stdin, Stdout: cmd.Stdout}
}

// Runner runs a Cmd. A non-nil error already carries the tool's stderr.
type Runner interface {
	Run(ctx context.Context, cmd Cmd) error
}

// Exec is the real Runner.
type Exec struct{}

const maxStderrBytes = 16 << 10

func (Exec) Run(ctx context.Context, cmd Cmd) error {
	proc := exec.CommandContext(ctx, cmd.Name, cmd.Args...)
	proc.Env = append(os.Environ(), cmd.Env...)
	proc.Stdin, proc.Stdout = cmd.Stdin, cmd.Stdout
	stderr := &capped{max: maxStderrBytes}
	proc.Stderr = stderr

	err := proc.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr):
		return fmt.Errorf("%s failed (exit %d): %s", cmd.Name, exitErr.ExitCode(), strings.TrimSpace(stderr.String()))
	default:
		return fmt.Errorf("could not run %s (is it installed?): %w", cmd.Name, err)
	}
}

// capped collects up to max bytes from stderr and silently discards the rest.
// It is used to capture tool error output without letting a runaway process
// exhaust memory.
type capped struct {
	buf bytes.Buffer
	max int
}

func (limiter *capped) Write(payload []byte) (int, error) {
	remaining := limiter.max - limiter.buf.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			payload = payload[:remaining]
		}
		limiter.buf.Write(payload)
	}
	return len(payload), nil
}

func (limiter *capped) String() string { return limiter.buf.String() }

// Fake records every call and lets a test script the result.
type Fake struct {
	Calls   []Cmd
	Handler func(Cmd) error // may write to c.Stdout; nil means succeed silently
}

func (fake *Fake) Run(_ context.Context, cmd Cmd) error {
	fake.Calls = append(fake.Calls, cmd)
	if fake.Handler != nil {
		return fake.Handler(cmd)
	}
	return nil
}
