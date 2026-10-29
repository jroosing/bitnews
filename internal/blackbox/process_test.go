package blackbox_test

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	// readyTimeout is how long a started daemon gets to answer the health route.
	readyTimeout = 20 * time.Second
	// exitTimeout is how long a daemon gets to exit after SIGTERM before the
	// harness kills it and fails the test.
	exitTimeout = 15 * time.Second
)

// syncBuffer is a bytes.Buffer that the process copier and the test can use
// at the same time.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// baseEnv is the environment every run gets: a scrubbed one, with PATH (the
// daemon finds 7zz and par2 there) and a HOME that is not a real one.
func baseEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"PATH": os.Getenv("PATH"),
		"HOME": t.TempDir(),
	}
}

// result is the outcome of a process that ran to its end.
type result struct {
	code   int
	stdout string
	stderr string
}

// proc is a started binary.
type proc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan struct{} // closed when the process has exited
}

// startProc starts the binary under test with exactly this environment.
func startProc(t *testing.T, args []string, env map[string]string) *proc {
	t.Helper()
	p := &proc{t: t, stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(binary.path, args...)
	p.cmd.Env = nil
	for _, k := range sortedKeys(env) {
		p.cmd.Env = append(p.cmd.Env, k+"="+env[k])
	}
	p.cmd.Stdout, p.cmd.Stderr = p.stdout, p.stderr
	p.cmd.WaitDelay = 5 * time.Second
	require.NoError(t, p.cmd.Start())
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		if !p.exited() {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("%s\n--- stdout ---\n%s--- stderr ---\n%s", p.describe(), p.stdout.String(), p.stderr.String())
		}
	})
	return p
}

func (p *proc) describe() string {
	return fmt.Sprintf("%s %s", binary.path, strings.Join(p.cmd.Args[1:], " "))
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// wait waits for the process to exit by itself.
func (p *proc) wait(timeout time.Duration) result {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
		p.t.Fatalf("%s did not exit within %s\n--- stderr ---\n%s", p.describe(), timeout, p.stderr.String())
	}
	return p.result()
}

func (p *proc) result() result {
	return result{code: p.cmd.ProcessState.ExitCode(), stdout: p.stdout.String(), stderr: p.stderr.String()}
}

// terminate sends SIGTERM and waits for the exit. It reports how long the
// shutdown took.
func (p *proc) terminate() (result, time.Duration) {
	p.t.Helper()
	require.NoError(p.t, p.cmd.Process.Signal(syscall.SIGTERM))
	start := time.Now()
	r := p.wait(exitTimeout)
	return r, time.Since(start)
}

// runOnce runs the binary to its end, for invocations that must not serve:
// --help, --version, start-up failures.
func runOnce(t *testing.T, args []string, env map[string]string) result {
	t.Helper()
	return startProc(t, args, env).wait(30 * time.Second)
}

// daemon is a running binary with its HTTP address.
type daemon struct {
	*proc
	addr   string // host:port
	port   int
	cfg    configDir
	client *http.Client
}

// startOptions tune startDaemon.
type startOptions struct {
	env  map[string]string
	args []string
	// drop names environment variables to leave out of the base environment.
	drop []string
	// listen overrides the address the harness polls for readiness.
	listen string
}

type startOption func(*startOptions)

// withEnv adds or overrides environment variables.
func withEnv(kv ...string) startOption {
	return func(o *startOptions) {
		for i := 0; i+1 < len(kv); i += 2 {
			o.env[kv[i]] = kv[i+1]
		}
	}
}

// withFlagAddr tells startDaemon which address the daemon will listen on
// when a command line flag, not BITNEWS_LISTEN, decides it.
func withFlagAddr(addr string) startOption {
	return func(o *startOptions) { o.listen = addr }
}

// withArgs adds command line arguments.
func withArgs(args ...string) startOption {
	return func(o *startOptions) { o.args = append(o.args, args...) }
}

// serveEnv is the environment of a serving daemon on cfg, listening on
// port. The torrent port is a free one too, because the Go daemon opens it at
// start-up and parallel tests must not share the default.
func serveEnv(t *testing.T, cfg configDir, port int) map[string]string {
	env := baseEnv(t)
	env["BITNEWS_CONFIG_DIR"] = cfg.Dir
	env["BITNEWS_LISTEN"] = fmt.Sprintf("127.0.0.1:%d", port)
	env["BITNEWS_TORRENT_PORT"] = fmt.Sprint(freePort(t))
	return env
}

// startDaemon starts the binary on cfg and returns once GET /api/v1/health
// answers 200. A start that loses a race for the port is retried.
func startDaemon(t *testing.T, cfg configDir, opts ...startOption) *daemon {
	t.Helper()
	var last *daemon
	for range 3 {
		port := freePort(t)
		o := startOptions{env: serveEnv(t, cfg, port)}
		for _, f := range opts {
			f(&o)
		}
		for _, k := range o.drop {
			delete(o.env, k)
		}
		d := &daemon{
			proc: startProc(t, o.args, o.env),
			addr: cmp.Or(o.listen, o.env["BITNEWS_LISTEN"]),
			port: port,
			cfg:  cfg,
			client: &http.Client{
				Timeout:       15 * time.Second,
				Transport:     &http.Transport{DisableKeepAlives: true},
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
		}
		last = d
		err := d.waitReady()
		if err == nil {
			return d
		}
		if d.exited() && strings.Contains(d.stderr.String(), "address already in use") {
			continue
		}
		t.Fatalf("daemon not ready: %v\n--- stderr ---\n%s", err, d.stderr.String())
	}
	t.Fatalf("daemon could not get a free port\n--- stderr ---\n%s", last.stderr.String())
	return nil
}

// waitReady polls the health route until it answers 200.
func (d *daemon) waitReady() error {
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if d.exited() {
			return errors.New("the process exited before it was ready")
		}
		resp, err := d.client.Get("http://" + d.addr + "/api/v1/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("no healthy answer within %s", readyTimeout)
}

// stop shuts the daemon down with SIGTERM and asserts a clean exit.
func (d *daemon) stop() result {
	d.t.Helper()
	r, _ := d.terminate()
	require.Equal(d.t, 0, r.code, "exit code after SIGTERM\n--- stderr ---\n%s", r.stderr)
	return r
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
