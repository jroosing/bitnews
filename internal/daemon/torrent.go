package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/netbind"
	"github.com/jroosing/bitnews/internal/store"
	"github.com/jroosing/bitnews/internal/torrent"
)

// DefaultTorrentPort is the torrent listen port when neither
// Deps.Torrent.Port nor the Settings name one. Peers need it forwarded to
// reach bitnews: there is no UPnP.
const DefaultTorrentPort = core.DefaultTorrentPort

// TorrentOptions configures the torrent engine Build creates. The zero value
// is what production uses.
type TorrentOptions struct {
	// Port is the TCP and uTP listen port; zero means the port of the
	// Settings (torrent.listen_port, by default DefaultTorrentPort).
	Port int
	// DisableDHT and DisableUTP turn those off. They exist for tests that
	// must not touch the network.
	DisableDHT, DisableUTP bool
}

// newTorrentEngine starts the torrent engine on the real adapters: the port,
// DHT, peer exchange and encryption policy of the Settings (read once, so a
// change needs a restart), piece completion in the database, and the sockets
// bound as the Settings say. A binding that cannot
// be applied makes it fail (torrent.ErrBindUnavailable): the daemon does not
// start with traffic that may leave through the wrong route.
func newTorrentEngine(db *store.DB, settings core.Settings, o TorrentOptions, log *slog.Logger) (*torrent.Engine, error) {
	b := settings.Network.Torrent
	cfg := torrentNetwork(settings, o)
	cfg.PieceStore = store.NewTorrentPieceRepo(db)
	cfg.Binding = netbind.Binding{Interface: b.Interface, Address: b.Address}
	cfg.Logger = slog.New(warnOnly{log.Handler()})
	return torrent.New(cfg)
}

// warnOnly passes only warnings and errors on: anacrolix logs tracker URLs,
// which may carry a passkey, at lower levels.
type warnOnly struct{ slog.Handler }

func (w warnOnly) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && w.Handler.Enabled(ctx, l)
}

func (w warnOnly) WithAttrs(a []slog.Attr) slog.Handler { return warnOnly{w.Handler.WithAttrs(a)} }

func (w warnOnly) WithGroup(n string) slog.Handler { return warnOnly{w.Handler.WithGroup(n)} }

// torrentFinisher keeps the Finisher's hands off a torrent that the engine
// seeds. The torrent runner already moved the Download's directory to its
// complete location, and moving it again, or writing a marker into it,
// would pull the files from under the engine. For such a torrent Finish only
// checks that the directory is there and returns it as the output; the
// originals are never touched. Everything else, and a torrent the engine does
// not have, goes to the real Finisher.
type torrentFinisher struct {
	inner   core.Finisher
	managed func(core.ID) bool
	log     *slog.Logger
}

func (f torrentFinisher) Finish(ctx context.Context, d core.Download, dir, unpackedDir string) (string, error) {
	if d.SourceKind != core.SourceTorrent || f.managed == nil || !f.managed(d.ID) {
		return f.inner.Finish(ctx, d, dir, unpackedDir)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", os.ErrInvalid
	}
	return dir, nil
}

// shareLimitRemover carries out the remove and remove_data Share Limit
// actions with the same code as DELETE /api/v1/downloads/{id}.
type shareLimitRemover struct{ downloads *app.DownloadService }

func (r *shareLimitRemover) Remove(ctx context.Context, id core.ID, deleteData bool) error {
	return r.downloads.Delete(ctx, id, deleteData)
}

// bindingWatcher warns once per change when the network bindings in the
// Settings differ from the ones the daemon started with: sockets are not
// rebound live, a restart applies the change.
type bindingWatcher struct {
	start   core.NetworkSettings
	current func() core.Settings
	log     *slog.Logger

	mu     sync.Mutex
	warned core.NetworkSettings
}

func (w *bindingWatcher) run(ctx context.Context, bus *core.Bus) {
	for ctx.Err() == nil {
		sub := bus.Subscribe(ctx)
		for ev := range sub.Events() {
			if _, ok := ev.(core.SettingsChanged); ok {
				w.check()
			}
		}
		sub.Close()
	}
}

func (w *bindingWatcher) check() {
	now := w.current().Network
	w.mu.Lock()
	defer w.mu.Unlock()
	if now == w.start || now == w.warned {
		return
	}
	w.warned = now
	w.log.Warn("the network binding settings changed; restart bitnews to apply them (sockets are not rebound live)")
}

// closeOnce closes c the first time it is called.
type closeOnce struct {
	c    io.Closer
	once sync.Once
	err  error
}

func (c *closeOnce) Close() error {
	if c == nil || c.c == nil {
		return nil
	}
	c.once.Do(func() { c.err = c.c.Close() })
	return c.err
}
