package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Default limits for Run.
const (
	// DefaultMaxOutput caps how much of stdout and of stderr is kept.
	DefaultMaxOutput = 4 << 20
	// DefaultTimeout stops a tool that runs for longer than this.
	DefaultTimeout = 12 * time.Hour
	// killGrace is how long a cancelled tool has to exit before it is killed
	// and its output pipes are closed.
	killGrace = 5 * time.Second
)

// ErrNotFound is returned by Locate when the program is not on PATH.
var ErrNotFound = errors.New("tool: program not found")

// Locate returns the absolute path of the program named name, searching
// PATH, or an error wrapping ErrNotFound.
func Locate(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s (install it, or run `make tools` and add bin/tools to PATH; see docs/deploy.md)", ErrNotFound, name)
	}
	return p, nil
}

// Spec describes one run.
type Spec struct {
	// Path is the program, from Locate. Args does not include it.
	Path string
	Args []string
	// Dir is the working directory.
	Dir string
	// Stdin is fed to the program, or nil for an empty stdin. Passwords go
	// here, never into Args.
	Stdin io.Reader
	// TempDir becomes TMPDIR; empty uses the system default.
	TempDir string
	// MaxOutput caps the kept stdout and stderr each; zero is
	// DefaultMaxOutput. Output beyond the cap is read and dropped.
	MaxOutput int
	// Timeout stops the run; zero is DefaultTimeout.
	Timeout time.Duration
}

// Result is what a finished run produced.
type Result struct {
	// ExitCode is the program's exit status, or -1 when it was killed by a
	// signal.
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// Truncated reports that stdout or stderr was longer than the cap.
	Truncated bool
}

// ErrTimeout is returned when the run exceeded Spec.Timeout.
var ErrTimeout = errors.New("tool: timed out")

// Run starts the program and waits for it. A non-zero exit status is not an
// error: the caller reads Result.ExitCode. An error means the program could
// not be started, the context was cancelled (the error wraps ctx.Err()) or
// the timeout expired (ErrTimeout). On cancellation the whole process group
// is killed.
func Run(ctx context.Context, s Spec) (Result, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := s.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, s.Path, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = scrubbedEnv(s.TempDir)
	cmd.Stdin = s.Stdin
	var stdout, stderr capped
	stdout.limit, stderr.limit = limit, limit
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	setProcessGroup(cmd)
	cmd.WaitDelay = killGrace

	err := cmd.Run()
	res := Result{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), Truncated: stdout.dropped || stderr.dropped}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case ctx.Err() != nil:
		res.ExitCode = -1
		return res, fmt.Errorf("tool: %s cancelled: %w", filepath.Base(s.Path), ctx.Err())
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.ExitCode = -1
		return res, fmt.Errorf("%w: %s after %s", ErrTimeout, filepath.Base(s.Path), timeout)
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	default:
		res.ExitCode = -1
		return res, fmt.Errorf("tool: start %s: %w", filepath.Base(s.Path), err)
	}
}

// scrubbedEnv is the whole environment of a tool: nothing from the daemon's
// own environment leaks in, and the output language is fixed so the parsers
// see English.
func scrubbedEnv(tmp string) []string {
	env := []string{"LC_ALL=C", "LANG=C", "HOME=/nonexistent"}
	if tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	} else if t := os.Getenv("TMPDIR"); t != "" {
		env = append(env, "TMPDIR="+t)
	}
	return env
}

// capped keeps the first limit bytes written to it and drops the rest, while
// still accepting every write so the program never blocks on a full pipe.
type capped struct {
	buf     bytes.Buffer
	limit   int
	dropped bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		n := min(room, len(p))
		c.buf.Write(p[:n])
		if n < len(p) {
			c.dropped = true
		}
	} else if len(p) > 0 {
		c.dropped = true
	}
	return len(p), nil
}
