package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func validEnv() map[string]string {
	return map[string]string{"HOME": "/home/u"}
}

func TestVersionFlagPrintsVersionAndCommit(t *testing.T) {
	version, commit = "1.2.3", "abc1234"
	var out, errOut bytes.Buffer

	code := run([]string{"--version"}, env(validEnv()), unprivileged(), &out, &errOut)

	require.Equal(t, 0, code)
	for _, want := range []string{"bitnews", "1.2.3", "abc1234"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestVersionFlagIgnoresInvalidConfig(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--version"}, env(map[string]string{"BITNEWS_LISTEN": "nope", "UMASK": "9"}), unprivileged(), &out, &errOut)

	assert.Equal(t, 0, code)
	assert.Empty(t, errOut.String())
}

func TestInvalidConfigPrintsErrorAndExitsTwo(t *testing.T) {
	vars := validEnv()
	vars["BITNEWS_LISTEN"] = "no-port"
	var out, errOut bytes.Buffer

	code := run(nil, env(vars), unprivileged(), &out, &errOut)

	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "BITNEWS_LISTEN")
	assert.Empty(t, out.String())
}

func TestUnknownFlagPrintsErrorAndExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--bogus"}, env(validEnv()), unprivileged(), &out, &errOut)

	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "bogus")
}

func TestHelpPrintsUsageToStdoutAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--help"}, env(validEnv()), unprivileged(), &out, &errOut)

	assert.Equal(t, 0, code)
	for _, want := range []string{"--config-dir", "BITNEWS_KEY_FILE", "PUID", "UMASK", "BITNEWS_ADMIN_PASSWORD_FILE"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestStartupIsLoggedInConfiguredFormat(t *testing.T) {
	vars := validEnv()
	vars["BITNEWS_LOG_FORMAT"] = "json"
	var out, errOut bytes.Buffer

	code := run([]string{"--listen", "127.0.0.1:9000"}, env(vars), unprivileged(), &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	var rec map[string]any
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &rec), "stderr %q is not one JSON record", errOut.String())
	assert.Equal(t, "127.0.0.1:9000", rec["listen"])
}

func TestPrivilegeDropFailureExitsOneBeforeStartingAnything(t *testing.T) {
	vars := validEnv()
	vars["PUID"], vars["PGID"] = "1001", "1001"
	var out, errOut bytes.Buffer

	code := run(nil, env(vars), unprivileged(), &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "bitnews: ")
	assert.Contains(t, errOut.String(), "PUID")
	assert.NotContains(t, errOut.String(), "starting bitnews", "nothing starts after a failed drop")
	assert.Empty(t, out.String())
}

func TestRootWithPUIDDropsBeforeStartingAndSucceeds(t *testing.T) {
	vars := validEnv()
	vars["PUID"], vars["PGID"] = "1001", "100"
	vars["UMASK"] = "027"
	sys := &fakeSys{}
	var out, errOut bytes.Buffer

	code := run(nil, env(vars), sys, &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Equal(t, []string{"Umask(027)", "Setgroups([100])", "Setgid(100)", "Setuid(1001)", "Setuid(0)", "Setgid(0)"}, sys.calls)
	assert.Contains(t, errOut.String(), "starting bitnews")
	assert.NotContains(t, errOut.String(), "running as root")
}

func TestRootWithoutPUIDWarnsOnceAndContinues(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run(nil, env(validEnv()), &fakeSys{}, &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Equal(t, 1, strings.Count(errOut.String(), "running as root: a parser bug would run with root privileges"))
	assert.Contains(t, errOut.String(), "starting bitnews")
}

func TestUmaskIsAppliedOnStartup(t *testing.T) {
	vars := validEnv()
	vars["UMASK"] = "077"
	sys := unprivileged()
	var out, errOut bytes.Buffer

	code := run(nil, env(vars), sys, &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	assert.Equal(t, []string{"Umask(077)"}, sys.calls)
}

func TestVersionAndHelpNeverTouchPrivileges(t *testing.T) {
	vars := validEnv()
	vars["PUID"], vars["PGID"] = "1001", "1001"
	for _, flag := range []string{"--version", "--help"} {
		t.Run(flag, func(t *testing.T) {
			sys := unprivileged()
			var out, errOut bytes.Buffer

			code := run([]string{flag}, env(vars), sys, &out, &errOut)

			assert.Equal(t, 0, code)
			assert.Empty(t, sys.calls)
		})
	}
}
