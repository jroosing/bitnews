package privdrop_test

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/bootstrap"
	"gitlab.com/kyura4k/gitnews/internal/privdrop"
)

// fakeSys simulates the kernel's credential rules closely enough to test
// Apply: only euid 0 may change ids freely, and every call is recorded.
type fakeSys struct {
	euid, egid int
	calls      []string
	failOn     map[string]error // call name -> error to return
	// reescalate lets Setuid(0)/Setgid(0) succeed after the drop, which a
	// real kernel never does; it models a failed drop.
	reescalate bool
}

func (f *fakeSys) record(name string, arg any) error {
	f.calls = append(f.calls, fmt.Sprintf("%s(%v)", name, arg))
	return f.failOn[name]
}

func (f *fakeSys) Geteuid() int { return f.euid }
func (f *fakeSys) Getegid() int { return f.egid }

func (f *fakeSys) Umask(mask int) int {
	f.calls = append(f.calls, fmt.Sprintf("Umask(%03o)", mask))
	return 0o22
}

func (f *fakeSys) Setgroups(gids []int) error { return f.record("Setgroups", gids) }

func (f *fakeSys) Setgid(gid int) error {
	if err := f.record("Setgid", gid); err != nil {
		return err
	}
	if f.euid != 0 && !f.reescalate && gid != f.egid {
		return fmt.Errorf("EPERM")
	}
	f.egid = gid
	return nil
}

func (f *fakeSys) Setuid(uid int) error {
	if err := f.record("Setuid", uid); err != nil {
		return err
	}
	if f.euid != 0 && !f.reescalate && uid != f.euid {
		return fmt.Errorf("EPERM")
	}
	f.euid = uid
	return nil
}

// logBuf collects log output so tests can count records.
type logBuf struct{ strings.Builder }

func (l *logBuf) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, nil))
}

func (l *logBuf) warnings() int { return strings.Count(l.String(), "level=WARN") }

func TestApplyAppliesUmaskWithoutRootOrRunAs(t *testing.T) {
	sys := &fakeSys{euid: 1000, egid: 1000}
	var logs logBuf

	err := privdrop.Apply(privdrop.Options{Umask: 0o027}, sys, logs.logger())

	require.NoError(t, err)
	assert.Equal(t, []string{"Umask(027)"}, sys.calls)
	assert.Zero(t, logs.warnings())
}

func TestApplyWarnsExactlyOnceWhenRootWithoutRunAs(t *testing.T) {
	sys := &fakeSys{}
	var logs logBuf

	err := privdrop.Apply(privdrop.Options{Umask: 0o002}, sys, logs.logger())

	require.NoError(t, err)
	assert.Equal(t, 1, logs.warnings())
	assert.Contains(t, logs.String(), "running as root: a parser bug would run with root privileges")
	assert.Equal(t, []string{"Umask(002)"}, sys.calls, "no id changes without RunAs")
}

func TestApplyDropsRootInOrderAndVerifiesNoWayBack(t *testing.T) {
	sys := &fakeSys{}
	var logs logBuf

	err := privdrop.Apply(privdrop.Options{
		RunAs: &bootstrap.Identity{UID: 1000, GID: 100},
		Umask: 0o022,
	}, sys, logs.logger())

	require.NoError(t, err)
	assert.Equal(t, []string{
		"Umask(022)",
		"Setgroups([100])",
		"Setgid(100)",
		"Setuid(1000)",
		"Setuid(0)", // verification: must fail
		"Setgid(0)", // verification: must fail
	}, sys.calls)
	assert.Equal(t, 1000, sys.euid)
	assert.Equal(t, 100, sys.egid)
	assert.Zero(t, logs.warnings(), "a successful drop is not a root run")
}

func TestApplyAbortsWhenSetuidZeroStillSucceeds(t *testing.T) {
	sys := &fakeSys{reescalate: true}

	err := privdrop.Apply(privdrop.Options{RunAs: &bootstrap.Identity{UID: 1000, GID: 100}}, sys, discard())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "setuid(0)")
}

func TestApplyAbortsWhenSetgidZeroStillSucceeds(t *testing.T) {
	// Setuid(0) fails but Setgid(0) succeeds: a half-dropped process.
	half := &halfDropSys{fakeSys: &fakeSys{}}

	err := privdrop.Apply(privdrop.Options{RunAs: &bootstrap.Identity{UID: 1000, GID: 100}}, half, discard())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "setgid(0)")
}

// halfDropSys lets Setgid(0) succeed, as if the gid drop did not stick.
type halfDropSys struct{ *fakeSys }

func (h *halfDropSys) Setgid(gid int) error {
	if gid == 0 {
		h.calls = append(h.calls, "Setgid(0)")
		return nil
	}
	return h.fakeSys.Setgid(gid)
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestApplyAbortsWhenIdentityDoesNotMatchAfterDrop(t *testing.T) {
	sys := &lyingSys{fakeSys: &fakeSys{}}

	err := privdrop.Apply(privdrop.Options{RunAs: &bootstrap.Identity{UID: 1000, GID: 100}}, sys, discard())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "euid")
}

// lyingSys ignores Setuid(uid) for a non-zero uid but rejects Setuid(0), so
// the process stays euid 0 while the verification calls look fine.
type lyingSys struct{ *fakeSys }

func (l *lyingSys) Setuid(uid int) error {
	l.calls = append(l.calls, fmt.Sprintf("Setuid(%v)", uid))
	if uid == 0 {
		return fmt.Errorf("EPERM")
	}
	return nil
}

func TestApplyStopsAtTheFirstFailingSyscall(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		fail      string
		wantCalls []string
	}{
		{"Setgroups", []string{"Umask(022)", "Setgroups([100])"}},
		{"Setgid", []string{"Umask(022)", "Setgroups([100])", "Setgid(100)"}},
		{"Setuid", []string{"Umask(022)", "Setgroups([100])", "Setgid(100)", "Setuid(1000)"}},
	}
	for _, tt := range tests {
		t.Run(tt.fail, func(t *testing.T) {
			sys := &fakeSys{failOn: map[string]error{tt.fail: boom}}

			err := privdrop.Apply(privdrop.Options{
				RunAs: &bootstrap.Identity{UID: 1000, GID: 100},
				Umask: 0o022,
			}, sys, discard())

			require.ErrorIs(t, err, boom)
			assert.Contains(t, err.Error(), strings.ToLower(tt.fail))
			assert.Equal(t, tt.wantCalls, sys.calls)
		})
	}
}

func TestApplyRejectsRunAsWhenNotRoot(t *testing.T) {
	sys := &fakeSys{euid: 1000, egid: 1000}

	err := privdrop.Apply(privdrop.Options{
		RunAs: &bootstrap.Identity{UID: 1001, GID: 1000},
		Umask: 0o022,
	}, sys, discard())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PUID")
	assert.Equal(t, []string{"Umask(022)"}, sys.calls, "no id calls without root")
}

func TestApplyIsNoopWhenRunAsEqualsCurrentIdentity(t *testing.T) {
	sys := &fakeSys{euid: 1000, egid: 100}
	var logs logBuf

	err := privdrop.Apply(privdrop.Options{
		RunAs: &bootstrap.Identity{UID: 1000, GID: 100},
		Umask: 0o077,
	}, sys, logs.logger())

	require.NoError(t, err)
	assert.Equal(t, []string{"Umask(077)"}, sys.calls)
	assert.Empty(t, logs.String(), "nothing to warn about")
}

func TestApplyAppliesUmaskBeforeAnythingElseOnEveryPath(t *testing.T) {
	boom := errors.New("boom")
	tests := map[string]struct {
		sys  *fakeSys
		opts privdrop.Options
	}{
		"root with drop":      {&fakeSys{}, privdrop.Options{RunAs: &bootstrap.Identity{UID: 1, GID: 2}}},
		"root without runas":  {&fakeSys{}, privdrop.Options{}},
		"unprivileged":        {&fakeSys{euid: 5, egid: 5}, privdrop.Options{}},
		"unprivileged runas":  {&fakeSys{euid: 5, egid: 5}, privdrop.Options{RunAs: &bootstrap.Identity{UID: 5, GID: 5}}},
		"rejected runas":      {&fakeSys{euid: 5, egid: 5}, privdrop.Options{RunAs: &bootstrap.Identity{UID: 6, GID: 5}}},
		"failing setgroups":   {&fakeSys{failOn: map[string]error{"Setgroups": boom}}, privdrop.Options{RunAs: &bootstrap.Identity{UID: 1, GID: 2}}},
		"failed verification": {&fakeSys{reescalate: true}, privdrop.Options{RunAs: &bootstrap.Identity{UID: 1, GID: 2}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tt.opts.Umask = 0o027

			_ = privdrop.Apply(tt.opts, tt.sys, discard())

			require.NotEmpty(t, tt.sys.calls)
			assert.Equal(t, "Umask(027)", tt.sys.calls[0])
		})
	}
}

func TestApplyTreatsRootRunAsAsRootAndWarnsOnce(t *testing.T) {
	sys := &fakeSys{}
	var logs logBuf

	err := privdrop.Apply(privdrop.Options{RunAs: &bootstrap.Identity{UID: 0, GID: 0}}, sys, logs.logger())

	require.NoError(t, err)
	assert.Equal(t, 1, logs.warnings())
	assert.Equal(t, []string{"Umask(000)"}, sys.calls, "nothing to drop to")
}

func TestApplyAcceptsRootAsTargetGroup(t *testing.T) {
	sys := &fakeSys{}

	err := privdrop.Apply(privdrop.Options{RunAs: &bootstrap.Identity{UID: 1000, GID: 0}}, sys, discard())

	require.NoError(t, err, "gid 0 may be kept: Setgid(0) is not a way back to root")
	assert.Equal(t, []string{"Umask(000)", "Setgroups([0])", "Setgid(0)", "Setuid(1000)", "Setuid(0)"}, sys.calls,
		"no gid probe after the drop when the target gid is 0")
}

func TestApplyWithNilLoggerDoesNotPanicWhenRoot(t *testing.T) {
	sys := &fakeSys{euid: 0, egid: 0}

	require.NotPanics(t, func() {
		require.NoError(t, privdrop.Apply(privdrop.Options{}, sys, nil))
	})
}
