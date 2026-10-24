package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
)

// fakePortEngine is a listenPorter that refuses the ports in taken.
type fakePortEngine struct {
	mu    sync.Mutex
	port  int
	taken map[int]bool
	moves []int
}

func (f *fakePortEngine) SetListenPort(port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taken[port] {
		return errors.New("address already in use")
	}
	f.port = port
	f.moves = append(f.moves, port)
	return nil
}

func (f *fakePortEngine) ListenPortStatus() core.ListenPortStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return core.ListenPortStatus{Port: f.port}
}

// memSettings is a portSettings in memory; err makes Change fail.
type memSettings struct {
	mu  sync.Mutex
	s   core.Settings
	err error
}

func (m *memSettings) Current() core.Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

func (m *memSettings) Change(_ context.Context, f func(*core.Settings) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	next := m.s
	if err := f(&next); err != nil {
		return err
	}
	m.s = next
	return nil
}

func newPort(override int) (*torrentPort, *fakePortEngine, *memSettings) {
	eng := &fakePortEngine{port: core.DefaultTorrentPort, taken: map[int]bool{}}
	s := &memSettings{s: core.DefaultSettings()}
	return &torrentPort{engine: eng, settings: s, override: override, log: slog.New(slog.DiscardHandler)}, eng, s
}

func TestSetListenPortMovesTheEngineAndStoresThePort(t *testing.T) {
	p, eng, s := newPort(0)

	st, err := p.SetListenPort(context.Background(), 45678)

	require.NoError(t, err)
	assert.Equal(t, 45678, st.Port)
	assert.Equal(t, 45678, eng.port)
	assert.Equal(t, 45678, s.Current().Torrent.ListenPort, "a restart keeps the port")
}

func TestSetListenPortThatCannotBeOpenedChangesNothing(t *testing.T) {
	p, eng, s := newPort(0)
	eng.taken[45678] = true

	_, err := p.SetListenPort(context.Background(), 45678)

	require.ErrorIs(t, err, core.ErrConflict)
	assert.Contains(t, err.Error(), "address already in use")
	assert.Equal(t, core.DefaultTorrentPort, eng.port)
	assert.Zero(t, s.Current().Torrent.ListenPort)
}

func TestSetListenPortMovesTheEngineBackWhenTheSettingsCannotBeStored(t *testing.T) {
	p, eng, s := newPort(0)
	s.err = errors.New("disk full")

	_, err := p.SetListenPort(context.Background(), 45678)

	require.Error(t, err)
	assert.Equal(t, core.DefaultTorrentPort, eng.port)
}

func TestSetListenPortIsRefusedWhenTheOperatorFixedThePort(t *testing.T) {
	p, eng, _ := newPort(40000)
	eng.port = 40000

	_, err := p.SetListenPort(context.Background(), 45678)

	var appErr *app.Error
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, "port_fixed", appErr.Code)
	assert.Equal(t, 40000, eng.port)

	st, err := p.SetListenPort(context.Background(), 40000)
	require.NoError(t, err, "asking for the fixed port itself is fine")
	assert.Equal(t, 40000, st.Port)
}

func TestSetListenPortWithoutAnEngineIsUnavailable(t *testing.T) {
	p := &torrentPort{settings: &memSettings{s: core.DefaultSettings()}, log: slog.New(slog.DiscardHandler)}

	_, err := p.SetListenPort(context.Background(), 45678)

	require.ErrorIs(t, err, app.ErrUnavailable)
	_, ok := p.ListenPortStatus()
	assert.False(t, ok)
}

func TestApplyMovesTheEngineToTheStoredPort(t *testing.T) {
	p, eng, s := newPort(0)
	s.s.Torrent.ListenPort = 50000

	p.apply()
	p.apply()

	assert.Equal(t, 50000, eng.port)
	assert.Equal(t, []int{50000}, eng.moves, "an unchanged port is not reopened")
}

func TestApplyLeavesAFixedPortAlone(t *testing.T) {
	p, eng, s := newPort(40000)
	eng.port = 40000
	s.s.Torrent.ListenPort = 50000

	p.apply()

	assert.Equal(t, 40000, eng.port)
}
