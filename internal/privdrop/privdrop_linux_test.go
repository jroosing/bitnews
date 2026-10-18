//go:build linux

package privdrop_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
	"gitlab.com/kyura4k/gitnews/internal/privdrop"
)

// The integration test below changes the ids of a real process, which needs
// root. It skips otherwise and runs in the Linux CI container, which starts
// as root. A process cannot go back to root, so the privilege drop runs in a
// child: the test binary re-executes itself with helperEnv set, and the
// child (TestHelperProcess) applies the drop and reports what it ended up
// as. Spawning a subprocess is fine in tests; ADR-0002 only forbids it in
// production code.

const (
	helperEnv    = "BITNEWS_PRIVDROP_HELPER_DIR"
	resultPrefix = "RESULT "
	nobody       = 65534
)

// TestHelperProcess is the child half of TestApplyDropsRealProcessToPUIDAndPGID.
// It is a no-op in a normal test run.
func TestHelperProcess(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("helper for TestApplyDropsRealProcessToPUIDAndPGID")
	}

	err := privdrop.Apply(privdrop.Options{
		RunAs: &bootstrap.Identity{UID: nobody, GID: nobody},
		Umask: 0o077,
	}, privdrop.OS(), discard())
	if err != nil {
		fmt.Printf("%serror=%v\n", resultPrefix, err)
		return
	}

	groups, _ := syscall.Getgroups()
	// Go applies the change to all threads; look at every one of them.
	var threadUIDs []string
	tasks, _ := filepath.Glob("/proc/self/task/*/status")
	for _, task := range tasks {
		status, _ := os.ReadFile(task)
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				threadUIDs = append(threadUIDs, strings.Join(strings.Fields(line)[1:], ","))
			}
		}
	}
	path := filepath.Join(dir, "created")
	createErr := os.WriteFile(path, nil, 0o666)
	mode := os.FileMode(0)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	fmt.Printf("%suid=%d euid=%d gid=%d egid=%d groups=%v setuid0=%v setgid0=%v create=%v mode=%o threads=%s\n",
		resultPrefix, syscall.Getuid(), syscall.Geteuid(), syscall.Getgid(), syscall.Getegid(), groups,
		syscall.Setuid(0), syscall.Setgid(0), createErr, mode, strings.Join(threadUIDs, ";"))
}

func TestApplyDropsRealProcessToPUIDAndPGID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to change ids; runs in the Linux CI container")
	}
	// Not t.TempDir: its parents are 0700 and the dropped child must reach it.
	dir, err := os.MkdirTemp("", "privdrop")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	require.NoError(t, os.Chmod(dir, 0o777))

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	var result string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, resultPrefix); ok {
			result = rest
		}
	}
	require.NotEmpty(t, result, "helper reported nothing: %s", out)
	require.NotContains(t, result, "error=", "Apply failed in the helper")

	fields := strings.Fields(result)
	for _, want := range []string{"uid=65534", "euid=65534", "gid=65534", "egid=65534", "groups=[65534]"} {
		assert.Contains(t, fields, want, result)
	}
	assert.True(t, slices.ContainsFunc(fields, func(f string) bool {
		return strings.HasPrefix(f, "setuid0=") && f != "setuid0=<nil>"
	}), "setuid(0) must fail after the drop: %s", result)
	assert.True(t, slices.ContainsFunc(fields, func(f string) bool {
		return strings.HasPrefix(f, "setgid0=") && f != "setgid0=<nil>"
	}), "setgid(0) must fail after the drop: %s", result)
	assert.Contains(t, fields, "create=<nil>", "the dropped process must be able to write its own files")
	assert.Contains(t, fields, "mode=600", "files created after the drop must respect UMASK 077")
	for _, f := range fields {
		if threads, ok := strings.CutPrefix(f, "threads="); ok {
			for _, uids := range strings.Split(threads, ";") {
				assert.Equal(t, "65534,65534,65534,65534", uids, "every thread must have dropped")
			}
		}
	}
}
