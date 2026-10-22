package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/privdrop"
)

// env builds a getenv func backed by a map.
func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

// fakeSys is a privdrop.Syscalls that records calls and never touches the
// real process. It behaves like an unprivileged process unless euid is 0.
type fakeSys struct {
	euid, egid int
	calls      []string
}

func (f *fakeSys) Geteuid() int { return f.euid }
func (f *fakeSys) Getegid() int { return f.egid }

func (f *fakeSys) Umask(mask int) int {
	f.calls = append(f.calls, fmt.Sprintf("Umask(%03o)", mask))
	return 0
}

func (f *fakeSys) Setgroups(gids []int) error {
	f.calls = append(f.calls, fmt.Sprintf("Setgroups(%v)", gids))
	return nil
}

func (f *fakeSys) Setgid(gid int) error {
	f.calls = append(f.calls, fmt.Sprintf("Setgid(%d)", gid))
	if f.euid != 0 && gid != f.egid {
		return errors.New("EPERM")
	}
	f.egid = gid
	return nil
}

func (f *fakeSys) Setuid(uid int) error {
	f.calls = append(f.calls, fmt.Sprintf("Setuid(%d)", uid))
	if f.euid != 0 && uid != f.euid {
		return errors.New("EPERM")
	}
	f.euid = uid
	return nil
}

// unprivileged is the default test process: not root, nothing to drop.
func unprivileged() *fakeSys { return &fakeSys{euid: 1000, egid: 1000} }

// validEnv is a complete environment whose config dir and key file live in
// the test's temp dir, so a run that gets as far as the store touches nothing
// else.
func validEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	return map[string]string{
		"HOME":               "/home/u",
		"BITNEWS_CONFIG_DIR": filepath.Join(dir, "config"),
		"BITNEWS_KEY_FILE":   filepath.Join(dir, "keys", "bitnews.key"),
		"BITNEWS_LISTEN":     "127.0.0.1:0",
	}
}

// runTest is run without a network: the server "runs" and exits at once.
func runTest(args []string, getenv func(string) (string, bool), sys privdrop.Syscalls, stdout, stderr io.Writer) int {
	return run(context.Background(), args, getenv, sys, stdout, stderr, func(context.Context, *http.Server, *slog.Logger) error { return nil })
}

func TestVersionFlagPrintsVersionAndCommit(t *testing.T) {
	version, commit = "1.2.3", "abc1234"
	var out, errOut bytes.Buffer

	code := runTest([]string{"--version"}, env(validEnv(t)), unprivileged(), &out, &errOut)

	require.Equal(t, 0, code)
	for _, want := range []string{"bitnews", "1.2.3", "abc1234"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestVersionFlagIgnoresInvalidConfig(t *testing.T) {
	var out, errOut bytes.Buffer

	code := runTest([]string{"--version"}, env(map[string]string{"BITNEWS_LISTEN": "nope", "UMASK": "9"}), unprivileged(), &out, &errOut)

	assert.Equal(t, 0, code)
	assert.Empty(t, errOut.String())
}

func TestInvalidConfigPrintsErrorAndExitsTwo(t *testing.T) {
	vars := validEnv(t)
	vars["BITNEWS_LISTEN"] = "no-port"
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "BITNEWS_LISTEN")
	assert.Empty(t, out.String())
}

func TestUnknownFlagPrintsErrorAndExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer

	code := runTest([]string{"--bogus"}, env(validEnv(t)), unprivileged(), &out, &errOut)

	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "bogus")
}

func TestHelpPrintsUsageToStdoutAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer

	code := runTest([]string{"--help"}, env(validEnv(t)), unprivileged(), &out, &errOut)

	assert.Equal(t, 0, code)
	for _, want := range []string{"--config-dir", "BITNEWS_KEY_FILE", "PUID", "UMASK", "BITNEWS_ADMIN_PASSWORD_FILE"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestStartupIsLoggedInConfiguredFormat(t *testing.T) {
	vars := validEnv(t)
	vars["BITNEWS_LOG_FORMAT"] = "json"
	var out, errOut bytes.Buffer

	code := runTest([]string{"--listen", "127.0.0.1:9000"}, env(vars), unprivileged(), &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	var starting map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(errOut.String()), "\n") {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line %q is not a JSON record", line)
		if rec["msg"] == "starting bitnews" {
			starting = rec
		}
	}
	require.NotNil(t, starting, "no starting record in %q", errOut.String())
	assert.Equal(t, "127.0.0.1:9000", starting["listen"])
}

func TestPrivilegeDropFailureExitsOneBeforeStartingAnything(t *testing.T) {
	vars := validEnv(t)
	vars["PUID"], vars["PGID"] = "1001", "1001"
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "bitnews: ")
	assert.Contains(t, errOut.String(), "PUID")
	assert.NotContains(t, errOut.String(), "starting bitnews", "nothing starts after a failed drop")
	assert.Empty(t, out.String())
}

func TestRootWithPUIDDropsBeforeStartingAndSucceeds(t *testing.T) {
	vars := validEnv(t)
	vars["PUID"], vars["PGID"] = "1001", "100"
	vars["UMASK"] = "027"
	sys := &fakeSys{}
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), sys, &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Equal(t, []string{"Umask(027)", "Setgroups([100])", "Setgid(100)", "Setuid(1001)", "Setuid(0)", "Setgid(0)"}, sys.calls)
	assert.Contains(t, errOut.String(), "starting bitnews")
	assert.NotContains(t, errOut.String(), "running as root")
}

func TestRootWithoutPUIDWarnsOnceAndContinues(t *testing.T) {
	var out, errOut bytes.Buffer

	code := runTest(nil, env(validEnv(t)), &fakeSys{}, &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Equal(t, 1, strings.Count(errOut.String(), "running as root: a parser bug would run with root privileges"))
	assert.Contains(t, errOut.String(), "starting bitnews")
}

func TestUmaskIsAppliedOnStartup(t *testing.T) {
	vars := validEnv(t)
	vars["UMASK"] = "077"
	sys := unprivileged()
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), sys, &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Equal(t, []string{"Umask(077)"}, sys.calls)
}

func TestVersionAndHelpNeverTouchPrivileges(t *testing.T) {
	vars := validEnv(t)
	vars["PUID"], vars["PGID"] = "1001", "1001"
	for _, flag := range []string{"--version", "--help"} {
		t.Run(flag, func(t *testing.T) {
			sys := unprivileged()
			var out, errOut bytes.Buffer

			code := runTest([]string{flag}, env(vars), sys, &out, &errOut)

			assert.Equal(t, 0, code)
			assert.Empty(t, sys.calls)
		})
	}
}

// syncBuffer is a bytes.Buffer that a test can read while run still logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStartupCreatesTheKeyAndTheDatabaseWithPrivateModes(t *testing.T) {
	vars := validEnv(t)
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	for _, path := range []string{vars["BITNEWS_KEY_FILE"], filepath.Join(vars["BITNEWS_CONFIG_DIR"], "bitnews.db")} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), path)
	}
}

func TestFailedPrivilegeDropTouchesNeitherKeyNorDatabase(t *testing.T) {
	vars := validEnv(t)
	vars["PUID"], vars["PGID"] = "1001", "1001"
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	require.Equal(t, 1, code)
	assert.NoFileExists(t, vars["BITNEWS_KEY_FILE"])
	assert.NoDirExists(t, vars["BITNEWS_CONFIG_DIR"])
}

func TestInvalidConfigTouchesNeitherKeyNorDatabase(t *testing.T) {
	vars := validEnv(t)
	vars["BITNEWS_LISTEN"] = "no-port"
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	require.Equal(t, 2, code)
	assert.NoFileExists(t, vars["BITNEWS_KEY_FILE"])
	assert.NoDirExists(t, vars["BITNEWS_CONFIG_DIR"])
}

func TestStartupWithoutAdminPasswordFileLogsTheSetupToken(t *testing.T) {
	var out, errOut bytes.Buffer

	code := runTest(nil, env(validEnv(t)), unprivileged(), &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Regexp(t, `first-run setup required.*setup_token=\S+`, errOut.String())
}

func TestAdminPasswordFileCreatesTheAdminBeforeSetupIsDecided(t *testing.T) {
	vars := validEnv(t)
	pwFile := filepath.Join(t.TempDir(), "admin-password")
	require.NoError(t, os.WriteFile(pwFile, []byte("correct horse battery staple\n"), 0o600))
	vars["BITNEWS_ADMIN_PASSWORD_FILE"] = pwFile
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Contains(t, errOut.String(), "admin account created from the admin password file")
	assert.NotContains(t, errOut.String(), "setup_token", "no setup is needed once the admin exists")
	assert.NotContains(t, errOut.String(), "correct horse")
}

func TestUnusableAdminPasswordFileExitsOne(t *testing.T) {
	vars := validEnv(t)
	vars["BITNEWS_ADMIN_PASSWORD_FILE"] = filepath.Join(t.TempDir(), "missing")
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "bitnews: ")
}

func TestUnopenableDatabaseExitsOne(t *testing.T) {
	vars := validEnv(t)
	require.NoError(t, os.WriteFile(vars["BITNEWS_CONFIG_DIR"], nil, 0o600)) // a file where the directory should be
	var out, errOut bytes.Buffer

	code := runTest(nil, env(vars), unprivileged(), &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "bitnews: ")
}

func TestServerFailureExitsOne(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(context.Background(), nil, env(validEnv(t)), unprivileged(), &out, &errOut,
		func(context.Context, *http.Server, *slog.Logger) error { return errors.New("boom") })

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "boom")
}

func TestServerGetsTheHardenedHTTPServerOnTheConfiguredAddress(t *testing.T) {
	vars := validEnv(t)
	vars["BITNEWS_LISTEN"] = "127.0.0.1:9123"
	var got *http.Server
	var out, errOut bytes.Buffer

	code := run(context.Background(), nil, env(vars), unprivileged(), &out, &errOut,
		func(_ context.Context, srv *http.Server, _ *slog.Logger) error { got = srv; return nil })

	require.Equal(t, 0, code, errOut.String())
	require.NotNil(t, got)
	assert.Equal(t, "127.0.0.1:9123", got.Addr)
	assert.Equal(t, 10*time.Second, got.ReadHeaderTimeout)
}

var listeningRE = regexp.MustCompile(`listening addr=(\S+)`)

func TestRunServesOnARealListenerAndShutsDownGracefullyWhenTheContextEnds(t *testing.T) {
	vars := validEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out io.Writer = &bytes.Buffer{}
	errOut := &syncBuffer{}
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, nil, env(vars), unprivileged(), out, errOut, listenAndServe) }()

	var addr string
	require.Eventually(t, func() bool {
		m := listeningRE.FindStringSubmatch(errOut.String())
		if m != nil {
			addr = m[1]
		}
		return m != nil
	}, 10*time.Second, 10*time.Millisecond, "server never reported its address: %s", errOut.String())
	resp, err := http.Get("http://" + addr + "/api/v1/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	gated, err := http.Get("http://" + addr + "/api/v1/downloads")
	require.NoError(t, err)
	_ = gated.Body.Close()

	cancel()

	select {
	case code := <-exit:
		assert.Equal(t, 0, code, errOut.String())
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after the context ended")
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, http.StatusServiceUnavailable, gated.StatusCode, "setup is required on a fresh database")
	_, dialErr := net.DialTimeout("tcp", addr, time.Second)
	assert.Error(t, dialErr, "the listener must be closed")
	assert.Contains(t, errOut.String(), "shutdown complete")
}
