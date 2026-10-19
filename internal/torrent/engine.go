package torrent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/dht/v2"
	anacrolix "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"golang.org/x/time/rate"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
)

// ErrClosed is returned by an Engine after Close.
var ErrClosed = errors.New("torrent: engine closed")

// ErrNoMetadata is returned by operations that need a torrent's metadata
// before a magnet link has delivered it.
var ErrNoMetadata = errors.New("torrent: metadata not received yet")

const (
	defaultMaxConns = 50 // anacrolix's EstablishedConnsPerTorrent default
	dhtAnnounceFor  = 15 * time.Minute
	dhtRetryAfter   = time.Minute
	rateSampleEvery = time.Second
)

// Engine implements core.TorrentEngine over anacrolix/torrent.
//
// Every torrent gets its own file storage rooted at its storage directory;
// piece completion is shared and kept in SQLite. Private torrents (BEP 27)
// never use the DHT or PEX: anacrolix/torrent v1.61.0 does not enforce this,
// so the engine does.
type Engine struct {
	cfg    Config
	logger *slog.Logger
	now    func() time.Time

	cl     *anacrolix.Client
	pieces *SQLitePieceCompletion
	down   *rate.Limiter
	up     *rate.Limiter

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	torrents map[core.InfoHash]*entry
	closed   bool

	// handshakes, when set, sees every extended handshake: the info hash,
	// whether the peer offers ut_pex (as the engine lets it count) and whether
	// we do. Tests use it.
	handshakes atomic.Pointer[func(h core.InfoHash, remoteOffersPEX, weOfferPEX bool)]

	private sync.Map // core.InfoHash -> struct{}: torrents with metadata flagged private

	evMu     sync.Mutex
	events   chan core.TorrentEvent
	evClosed bool
}

var _ core.TorrentEngine = (*Engine)(nil)

// entry is the engine's record of one torrent.
type entry struct {
	hash   core.InfoHash
	magnet bool // added from a magnet link

	// opMu serialises the long operations: Pause, Resume, Relocate, Remove.
	opMu sync.Mutex

	failure atomic.Pointer[error] // set once by fail; readable without locks

	mu             sync.Mutex // guards everything below
	t              *anacrolix.Torrent
	dir            string
	paused         bool
	ready          chan struct{} // closed once t is set the first time
	cancel         context.CancelFunc
	dhtCancel      context.CancelFunc
	metaEmitted    bool
	fetchedEmitted bool

	limit         core.ShareLimit
	limitNotified bool
	uploadedBase  int64 // uploaded by earlier incarnations (see Relocate)
	seedSince     time.Time
	seedAccum     time.Duration

	rate rateSample

	progress int64 // bytes received as of the last sample, for stall detection
	stalled  int   // consecutive samples without progress while connected
}

// A torrent that has peers but receives nothing is first nudged after
// nudgeTicks sampler ticks (one second each), then, if that did not help,
// its connections are cycled after cycleTicks.
const (
	nudgeTicks = 4
	cycleTicks = 10
)

type rateSample struct {
	at          time.Time
	read, wrote int64
	down, up    int64
}

// New starts an engine: it opens the listen sockets and starts the DHT if
// enabled. Call Close to release them.
func New(cfg Config) (*Engine, error) {
	e := &Engine{cfg: cfg, torrents: make(map[core.InfoHash]*entry)}
	cc, err := e.clientConfig(cfg)
	if err != nil {
		return nil, err
	}
	e.logger = cc.Slogger
	e.now = cfg.Now
	if e.now == nil {
		e.now = time.Now
	}
	size := cfg.EventBuffer
	if size <= 0 {
		size = defaultEventBuffer
	}
	e.events = make(chan core.TorrentEvent, size)
	e.ctx, e.cancel = context.WithCancel(context.Background())

	popts := []PieceCompletionOption{WithPieceCompletionLogger(e.logger)}
	if cfg.FlushInterval > 0 {
		popts = append(popts, WithFlushInterval(cfg.FlushInterval))
	}
	e.pieces = NewSQLitePieceCompletion(cfg.PieceStore, popts...)
	cl, err := anacrolix.NewClient(cc)
	if err != nil {
		_ = e.pieces.Close()
		return nil, fmt.Errorf("torrent: start client: %w", err)
	}
	e.cl = cl
	if cfg.LSD {
		e.logger.Warn("torrent: local service discovery is not supported by anacrolix/torrent v1.61.0")
	}

	interval := cfg.ShareLimitInterval
	if interval <= 0 {
		interval = defaultShareLimitInterval
	}
	e.wg.Add(2)
	go e.loop(interval, e.checkShareLimits)
	go e.loop(rateSampleEvery, e.sampleRates)
	return e, nil
}

func (e *Engine) loop(every time.Duration, f func()) {
	defer e.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

// Events implements core.TorrentEngine.
func (e *Engine) Events() <-chan core.TorrentEvent { return e.events }

func (e *Engine) emit(ev core.TorrentEvent) {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	if e.evClosed {
		return
	}
	select {
	case e.events <- ev:
	default:
		e.logger.Warn("torrent: event dropped, consumer too slow", "kind", string(ev.Kind), "info_hash", ev.Hash.String())
	}
}

// Close stops all torrents, flushes piece completion and closes the
// sockets and the Events channel. It is safe to call twice.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()

	e.cancel()
	e.wg.Wait()

	var errs []error
	errs = append(errs, e.cl.Close()...)
	errs = append(errs, e.pieces.Close())

	e.evMu.Lock()
	e.evClosed = true
	close(e.events)
	e.evMu.Unlock()
	return errors.Join(errs...)
}

func (e *Engine) lookup(h core.InfoHash) (*entry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	ent, ok := e.torrents[h]
	if !ok {
		return nil, fmt.Errorf("torrent: %s: %w", h, core.ErrNotFound)
	}
	return ent, nil
}

// Add implements core.TorrentEngine.
func (e *Engine) Add(ctx context.Context, spec core.TorrentSpec) (core.InfoHash, error) {
	if err := spec.Validate(); err != nil {
		return core.InfoHash{}, err
	}
	if !filepath.IsAbs(spec.StorageDir) {
		return core.InfoHash{}, fmt.Errorf("%w: storage dir must be absolute", core.ErrInvalidTorrentSpec)
	}
	aspec, magnet, err := e.parseSpec(spec)
	if err != nil {
		return core.InfoHash{}, err
	}
	h := core.InfoHash(aspec.InfoHash)

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return h, ErrClosed
	}
	if _, dup := e.torrents[h]; dup {
		e.mu.Unlock()
		return h, fmt.Errorf("torrent: add %s: %w", h, core.ErrConflict)
	}
	ent := &entry{hash: h, magnet: magnet, dir: filepath.Clean(spec.StorageDir), paused: spec.Paused,
		ready: make(chan struct{}), limit: core.UnlimitedShareLimit()}
	e.torrents[h] = ent // reserve the hash; removed again on failure
	e.mu.Unlock()

	if err := e.attach(ctx, ent, aspec, spec.Paused); err != nil {
		e.mu.Lock()
		delete(e.torrents, h)
		e.mu.Unlock()
		return h, err
	}
	return h, nil
}

// parseSpec turns a core.TorrentSpec into an anacrolix spec, refusing
// everything the engine does not want from hostile input: sources, web seeds
// and DHT nodes are dropped.
func (e *Engine) parseSpec(spec core.TorrentSpec) (aspec *anacrolix.TorrentSpec, magnet bool, err error) {
	if spec.Magnet != "" {
		aspec, err = anacrolix.TorrentSpecFromMagnetUri(spec.Magnet)
		if err != nil {
			return nil, true, fmt.Errorf("%w: magnet: %v", core.ErrInvalidTorrentSpec, err)
		}
		magnet = true
	} else {
		mi, info, err := parseMetainfo(spec.Metainfo)
		if err != nil {
			return nil, false, err
		}
		// Refuse unsafe names before anything is created on disk.
		if _, err := torrentPaths(spec.StorageDir, info); err != nil {
			return nil, false, err
		}
		aspec, err = anacrolix.TorrentSpecFromMetaInfoErr(mi)
		if err != nil {
			return nil, false, fmt.Errorf("%w: %v", ErrInvalidMetainfo, err)
		}
	}
	if aspec.InfoHash == (metainfo.Hash{}) {
		return nil, magnet, fmt.Errorf("%w: no v1 info hash", core.ErrInvalidTorrentSpec)
	}
	aspec.Sources = nil // would make us fetch metainfo from arbitrary URLs
	aspec.Webseeds = nil
	aspec.DhtNodes = nil
	return aspec, magnet, nil
}

// attach adds the torrent to the anacrolix client with storage in ent.dir and
// starts its watchers. For a metainfo it validates every file path first.
func (e *Engine) attach(ctx context.Context, ent *entry, aspec *anacrolix.TorrentSpec, paused bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ent.mu.Lock()
	dir := ent.dir
	ent.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("torrent: create storage dir: %w", err)
	}
	spec := *aspec
	spec.Storage = &torrentStorage{dir: dir, pieces: e.pieces, logger: e.logger, onError: func(err error) { e.fail(ent, err) }}
	spec.DisallowDataDownload = paused
	spec.DisallowDataUpload = paused

	t, isNew, err := e.cl.AddTorrentSpec(&spec)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMetainfo, err)
	}
	if !isNew {
		return fmt.Errorf("torrent: add %s: %w", ent.hash, core.ErrConflict)
	}
	t.SetOnWriteChunkError(func(err error) { e.fail(ent, fmt.Errorf("write: %w", err)) })
	if paused {
		t.SetMaxEstablishedConns(0)
	}

	wctx, cancel := context.WithCancel(e.ctx)
	ent.mu.Lock()
	first := ent.t == nil
	ent.t, ent.cancel = t, cancel
	ent.mu.Unlock()
	if first {
		close(ent.ready)
	}
	e.wg.Add(1)
	go e.watch(wctx, ent, t)
	e.startDHT(ent)
	return nil
}

// watch reports a torrent's metadata and completion.
func (e *Engine) watch(ctx context.Context, ent *entry, t *anacrolix.Torrent) {
	defer e.wg.Done()
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return
	}
	e.onMetadata(ent, t)
	select {
	case <-t.Complete().On():
	case <-ctx.Done():
		return
	}
	e.onComplete(ent)
}

func isPrivate(t *anacrolix.Torrent) bool {
	info := t.Info()
	return info != nil && info.Private != nil && *info.Private
}

func (e *Engine) onMetadata(ent *entry, t *anacrolix.Torrent) {
	if info := t.Info(); info != nil {
		if err := checkInfo(info); err != nil {
			e.fail(ent, err)
			return
		}
	}
	// anacrolix downloads nothing until pieces are wanted.
	t.DownloadAll()
	if isPrivate(t) {
		e.private.Store(ent.hash, struct{}{})
		e.stopDHT(ent)
		// Connections made before the metadata arrived may have negotiated
		// PEX; cycle them so they come back under the private rules.
		if old := t.SetMaxEstablishedConns(0); old > 0 {
			t.SetMaxEstablishedConns(old)
		}
	}
	ent.mu.Lock()
	emit := ent.magnet && !ent.metaEmitted
	ent.metaEmitted = true
	ent.mu.Unlock()
	if emit {
		e.emit(core.TorrentEvent{Kind: core.TorrentMetadataReceived, Hash: ent.hash})
	}
}

func (e *Engine) onComplete(ent *entry) {
	ent.mu.Lock()
	e.touchSeeding(ent, e.now())
	emit := !ent.fetchedEmitted
	ent.fetchedEmitted = true
	ent.mu.Unlock()
	if emit {
		e.emit(core.TorrentEvent{Kind: core.TorrentFetched, Hash: ent.hash})
	}
}

// fail records a torrent error and emits TorrentErrored. It may be called
// from inside anacrolix callbacks that hold the client lock, so anything that
// calls back into anacrolix happens in a goroutine.
func (e *Engine) fail(ent *entry, err error) {
	// No ent.mu here: storage callbacks arrive with the anacrolix client lock
	// held, and Stats takes ent.mu before the client lock.
	if !ent.failure.CompareAndSwap(nil, &err) {
		return
	}
	e.emit(core.TorrentEvent{Kind: core.TorrentErrored, Hash: ent.hash, Err: err})
	go func() {
		select {
		case <-ent.ready:
		case <-e.ctx.Done():
			return
		}
		ent.mu.Lock()
		t := ent.t
		ent.mu.Unlock()
		t.DisallowDataDownload()
		t.SetMaxEstablishedConns(0)
	}()
}

// --- DHT -----------------------------------------------------------------

// startDHT announces ent's torrent on every DHT server until stopDHT, unless
// DHT is off, the torrent is paused, or its metadata says it is private.
func (e *Engine) startDHT(ent *entry) {
	if !e.cfg.DHT {
		return
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()
	if ent.dhtCancel != nil || ent.paused || ent.t == nil || isPrivate(ent.t) {
		return
	}
	ctx, cancel := context.WithCancel(e.ctx)
	ent.dhtCancel = cancel
	for _, s := range e.cl.DhtServers() {
		e.wg.Add(1)
		go e.announceLoop(ctx, ent.t, s)
	}
}

func (e *Engine) stopDHT(ent *entry) {
	ent.mu.Lock()
	cancel := ent.dhtCancel
	ent.dhtCancel = nil
	ent.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *Engine) announceLoop(ctx context.Context, t *anacrolix.Torrent, s anacrolix.DhtServer) {
	defer e.wg.Done()
	for ctx.Err() == nil {
		wait := dhtAnnounceFor
		_, stop, err := t.AnnounceToDht(s)
		if err != nil {
			wait = dhtRetryAfter
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if stop != nil {
			stop()
		}
	}
}

// configureDHTServer makes the DHT server ignore announce_peer for private
// torrents, so nobody can inject peers into them through the DHT.
func (e *Engine) configureDHTServer(cfg *dht.ServerConfig) {
	next := cfg.OnAnnouncePeer
	cfg.OnAnnouncePeer = func(ih metainfo.Hash, ip net.IP, port int, portOk bool) {
		if _, private := e.private.Load(core.InfoHash(ih)); private {
			return
		}
		if next != nil {
			next(ih, ip, port, portOk)
		}
	}
}

// --- PEX -----------------------------------------------------------------

// privateExtensions is what a connection of a private torrent offers: the
// metadata exchange only. No ut_pex, no hole punching.
var privateExtensions = anacrolix.LocalLtepProtocolMap{
	Index:      []pp.ExtensionName{pp.ExtensionNameMetadata},
	NumBuiltin: 1,
}

func (e *Engine) onPeerConnAdded(pc *anacrolix.PeerConn) {
	if isPrivate(pc.Torrent()) {
		pc.LocalLtepProtocolMap = &privateExtensions
	}
}

// onExtendedHandshake hides the peer's ut_pex and ut_holepunch from a
// private torrent's connection, so neither is ever initialised for it.
func (e *Engine) onExtendedHandshake(pc *anacrolix.PeerConn, msg *pp.ExtendedHandshakeMessage) {
	if isPrivate(pc.Torrent()) {
		delete(msg.M, pp.ExtensionNamePex)
		delete(msg.M, "ut_holepunch")
	}
	if obs := e.handshakes.Load(); obs != nil {
		_, remote := msg.M[pp.ExtensionNamePex]
		local := pc.LocalLtepProtocolMap != nil && slices.Contains(pc.LocalLtepProtocolMap.Index, pp.ExtensionNamePex)
		(*obs)(core.InfoHash(pc.Torrent().InfoHash()), remote, local)
	}
}

// --- control -------------------------------------------------------------

// Pause implements core.TorrentEngine.
func (e *Engine) Pause(h core.InfoHash) error {
	ent, err := e.lookup(h)
	if err != nil {
		return err
	}
	ent.opMu.Lock()
	defer ent.opMu.Unlock()
	return e.pause(ent)
}

func (e *Engine) pause(ent *entry) error {
	ent.mu.Lock()
	t := ent.t
	ent.paused = true
	e.touchSeeding(ent, e.now())
	ent.mu.Unlock()
	e.stopDHT(ent)
	t.DisallowDataDownload()
	t.DisallowDataUpload()
	t.SetMaxEstablishedConns(0)
	return nil
}

// Resume implements core.TorrentEngine.
func (e *Engine) Resume(h core.InfoHash) error {
	ent, err := e.lookup(h)
	if err != nil {
		return err
	}
	ent.opMu.Lock()
	defer ent.opMu.Unlock()
	return e.resume(ent)
}

func (e *Engine) resume(ent *entry) error {
	ent.mu.Lock()
	t := ent.t
	ent.paused = false
	e.touchSeeding(ent, e.now())
	failed := ent.failure.Load() != nil
	ent.mu.Unlock()
	t.SetMaxEstablishedConns(defaultMaxConns)
	t.AllowDataUpload()
	if !failed {
		t.AllowDataDownload()
	}
	e.startDHT(ent)
	return nil
}

// SetShareLimits implements core.TorrentEngine.
func (e *Engine) SetShareLimits(h core.InfoHash, l core.ShareLimit) error {
	ent, err := e.lookup(h)
	if err != nil {
		return err
	}
	ent.mu.Lock()
	ent.limit, ent.limitNotified = l, false
	ent.mu.Unlock()
	return nil
}

// SetRateLimits implements core.TorrentEngine.
func (e *Engine) SetRateLimits(down, up int64) error {
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return ErrClosed
	}
	setRateLimit(e.down, down)
	setRateLimit(e.up, up)
	return nil
}

// Remove implements core.TorrentEngine.
func (e *Engine) Remove(ctx context.Context, h core.InfoHash, deleteData bool) error {
	ent, err := e.lookup(h)
	if err != nil {
		return err
	}
	ent.opMu.Lock()
	defer ent.opMu.Unlock()

	e.mu.Lock()
	delete(e.torrents, h)
	e.mu.Unlock()

	e.stopDHT(ent)
	ent.mu.Lock()
	t, dir, cancel := ent.t, ent.dir, ent.cancel
	ent.mu.Unlock()
	var info *metainfo.Info
	if t != nil {
		info = t.Info()
	}
	if cancel != nil {
		cancel()
	}
	if t != nil {
		t.Drop()
	}
	e.private.Delete(h)
	errs := []error{e.pieces.Forget(ctx, h)}
	if deleteData && info != nil {
		errs = append(errs, deleteFiles(dir, info))
	}
	return errors.Join(errs...)
}

// deleteFiles removes the torrent's own files below dir, then the
// directories that are left empty, never dir itself and never anything the
// torrent does not list.
func deleteFiles(dir string, info *metainfo.Info) error {
	rels, err := torrentPaths(dir, info)
	if err != nil {
		return err
	}
	var errs []error
	for _, rel := range rels {
		p, err := fsutil.SafeJoin(dir, rel)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		for parent := filepath.Dir(p); parent != dir && isWithin(dir, parent); parent = filepath.Dir(parent) {
			if os.Remove(parent) != nil { // not empty (or gone): stop climbing
				break
			}
		}
	}
	return errors.Join(errs...)
}

func isWithin(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && rel != ".." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[:2] == ".." && os.IsPathSeparator(rel[2])
}

// Relocate implements core.TorrentEngine. It pauses the torrent, closes its
// storage, moves the storage directory with fsutil.MoveDir and re-adds the
// torrent there. Piece completion is keyed by info hash, so nothing is
// rehashed. If any step fails the torrent is put back where it was.
func (e *Engine) Relocate(ctx context.Context, h core.InfoHash, newDir string) error {
	ent, err := e.lookup(h)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(newDir) {
		return fmt.Errorf("%w: new dir must be absolute", core.ErrInvalidTorrentSpec)
	}
	newDir = filepath.Clean(newDir)
	ent.opMu.Lock()
	defer ent.opMu.Unlock()

	ent.mu.Lock()
	t, oldDir, wasPaused := ent.t, ent.dir, ent.paused
	ent.mu.Unlock()
	if newDir == oldDir {
		return nil
	}
	if t.Info() == nil {
		return fmt.Errorf("torrent: relocate %s: %w", h, ErrNoMetadata)
	}
	mi := t.Metainfo()
	aspec, err := anacrolix.TorrentSpecFromMetaInfoErr(&mi)
	if err != nil {
		return fmt.Errorf("torrent: relocate %s: %w", h, err)
	}
	aspec.Sources, aspec.Webseeds, aspec.DhtNodes = nil, nil, nil
	aspec.PieceLayers = nil // v1 only; anacrolix would complain about missing v2 piece roots
	peers := dialablePeers(t)
	uploaded := uploadedBytes(t)

	// 1. Stop the torrent and release its files.
	e.stopDHT(ent)
	ent.mu.Lock()
	e.touchSeeding(ent, e.now())
	ent.paused = true
	cancel := ent.cancel
	ent.mu.Unlock()
	cancel()
	t.Drop()
	ent.mu.Lock()
	ent.uploadedBase += uploaded
	ent.mu.Unlock()

	// 2. Move the directory; 3. re-add it there. Roll back on failure.
	moveErr := e.moveAndReadd(ctx, ent, aspec, oldDir, newDir, peers)
	if moveErr != nil {
		rollbackErr := e.readd(ctx, ent, aspec, oldDir, peers)
		if rollbackErr != nil {
			e.fail(ent, fmt.Errorf("relocate rollback: %w", rollbackErr))
		}
		e.finishRelocate(ent, wasPaused)
		return errors.Join(fmt.Errorf("torrent: relocate %s: %w", h, moveErr), rollbackErr)
	}
	e.finishRelocate(ent, wasPaused)
	return nil
}

func (e *Engine) moveAndReadd(ctx context.Context, ent *entry, aspec *anacrolix.TorrentSpec, oldDir, newDir string, peers []anacrolix.PeerInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fsutil.MoveDir(oldDir, newDir); err != nil {
		return err
	}
	if err := e.readd(ctx, ent, aspec, newDir, peers); err != nil {
		// Put the files back before the caller re-adds at the old place.
		if _, backErr := fsutil.MoveDir(newDir, oldDir); backErr != nil {
			return errors.Join(err, fmt.Errorf("move back: %w", backErr))
		}
		return err
	}
	return nil
}

// readd attaches the torrent again with storage in dir (paused; the caller
// resumes it) and gives it back its peers.
func (e *Engine) readd(ctx context.Context, ent *entry, aspec *anacrolix.TorrentSpec, dir string, peers []anacrolix.PeerInfo) error {
	ent.mu.Lock()
	ent.dir = dir
	ent.mu.Unlock()
	if err := e.attach(context.WithoutCancel(ctx), ent, aspec, true); err != nil {
		return err
	}
	ent.mu.Lock()
	t := ent.t
	ent.mu.Unlock()
	t.AddPeers(peers)
	return nil
}

// finishRelocate puts the torrent back into the state it had before
// Relocate: running unless it was paused.
func (e *Engine) finishRelocate(ent *entry, wasPaused bool) {
	if wasPaused {
		_ = e.pause(ent)
		return
	}
	_ = e.resume(ent)
}

// dialablePeers lists the peers of t that we can reconnect to: the remote
// address with the port the peer said it listens on.
func dialablePeers(t *anacrolix.Torrent) []anacrolix.PeerInfo {
	var out []anacrolix.PeerInfo
	for _, pc := range t.PeerConns() {
		ap, err := netip.ParseAddrPort(pc.RemoteAddr.String())
		if err != nil {
			continue
		}
		port := ap.Port()
		if p := pc.PeerListenPort; p != 0 {
			port = uint16(p)
		}
		out = append(out, anacrolix.PeerInfo{Addr: anacrolix.StringAddr(net.JoinHostPort(ap.Addr().String(), strconv.Itoa(int(port)))), Source: pc.Discovery})
	}
	return out
}

// --- stats and share limits ----------------------------------------------

// touchSeeding advances the seeding clock to now. The caller holds ent.mu.
func (e *Engine) touchSeeding(ent *entry, now time.Time) {
	seeding := ent.t != nil && !ent.paused && ent.failure.Load() == nil && ent.t.Complete().Bool()
	switch {
	case seeding && ent.seedSince.IsZero():
		ent.seedSince = now
	case !seeding && !ent.seedSince.IsZero():
		ent.seedAccum += now.Sub(ent.seedSince)
		ent.seedSince = time.Time{}
	}
}

func (ent *entry) seedingTime(now time.Time) time.Duration {
	d := ent.seedAccum
	if !ent.seedSince.IsZero() {
		d += now.Sub(ent.seedSince)
	}
	return d
}

// Stats implements core.TorrentEngine.
func (e *Engine) Stats(h core.InfoHash) (core.TorrentStats, error) {
	ent, err := e.lookup(h)
	if err != nil {
		return core.TorrentStats{}, err
	}
	return e.stats(ent, h), nil
}

func (e *Engine) stats(ent *entry, h core.InfoHash) core.TorrentStats {
	now := e.now()
	ent.mu.Lock()
	defer ent.mu.Unlock()
	t := ent.t
	e.touchSeeding(ent, now)
	st := t.Stats()
	info := t.Info()

	out := core.TorrentStats{
		Name:        t.Name(),
		Private:     isPrivate(t),
		DownRate:    ent.rate.down,
		UpRate:      ent.rate.up,
		Peers:       st.ActivePeers,
		Seeds:       st.ConnectedSeeders,
		Uploaded:    ent.uploadedBase + st.BytesWrittenData.Int64(),
		SeedingTime: ent.seedingTime(now),
	}
	if info != nil {
		out.Size = t.Length()
		out.DoneBytes = t.BytesCompleted()
		out.Files = ent.files(info)
		if out.Size > 0 {
			out.Ratio = float64(out.Uploaded) / float64(out.Size)
		}
	}
	switch {
	case ent.failure.Load() != nil:
		out.State = core.TorrentError
	case ent.paused:
		out.State = core.TorrentPaused
	case info == nil:
		out.State = core.TorrentMetadataPending
	case t.Complete().Bool():
		out.State = core.TorrentSeeding
	default:
		out.State = core.TorrentDownloading
	}
	return out
}

// files lists the torrent's files with the same safe paths used on disk.
func (ent *entry) files(info *metainfo.Info) []core.TorrentFile {
	rels, err := torrentPaths(ent.dir, info)
	if err != nil {
		return nil
	}
	files := info.UpvertedFiles()
	out := make([]core.TorrentFile, len(rels))
	for i, rel := range rels {
		out[i] = core.TorrentFile{Path: rel, Size: files[i].Length}
	}
	return out
}

// sampleRates updates every torrent's transfer rates.
func (e *Engine) sampleRates() {
	e.mu.Lock()
	all := make([]*entry, 0, len(e.torrents))
	for _, ent := range e.torrents {
		all = append(all, ent)
	}
	e.mu.Unlock()
	now := time.Now()
	for _, ent := range all {
		ent.mu.Lock()
		if t := ent.t; t != nil {
			st := t.Stats()
			read, wrote := st.BytesReadUsefulData.Int64(), st.BytesWrittenData.Int64()
			if dt := now.Sub(ent.rate.at).Seconds(); !ent.rate.at.IsZero() && dt > 0 {
				ent.rate.down = max(0, int64(float64(read-ent.rate.read)/dt))
				ent.rate.up = max(0, int64(float64(wrote-ent.rate.wrote)/dt))
			}
			ent.rate.at, ent.rate.read, ent.rate.wrote = now, read, wrote
			e.nudgeIfStalled(ent, st.ActivePeers, read+t.BytesCompleted())
		}
		ent.mu.Unlock()
	}
}

// nudgeIfStalled works around stalls in anacrolix/torrent v1.61.0, seen
// under load in local swarms: a peer connection can get stuck with its
// "update requests" flag set, so it never expresses interest or sends a
// request again, or holds a request the seeder never answers. When a torrent
// has peers but has received nothing for nudgeTicks ticks, the download is
// cancelled and re-allowed, which re-issues requests; if that does not help
// within cycleTicks, the connections are dropped and the peers offered again,
// which replaces the stuck PeerConn. A torrent that is merely choked by every
// peer is cycled at most every cycleTicks seconds. The caller holds ent.mu,
// which also keeps this from racing with Pause.
func (e *Engine) nudgeIfStalled(ent *entry, activePeers int, progress int64) {
	t := ent.t
	if ent.paused || ent.failure.Load() != nil || activePeers == 0 || t.Info() == nil || t.Complete().Bool() ||
		progress != ent.progress {
		ent.progress, ent.stalled = progress, 0
		return
	}
	ent.stalled++
	switch ent.stalled {
	case nudgeTicks:
		t.DisallowDataDownload()
		t.AllowDataDownload()
	case cycleTicks:
		ent.stalled = 0
		peers := dialablePeers(t)
		old := t.SetMaxEstablishedConns(0)
		t.SetMaxEstablishedConns(old)
		t.AddPeers(peers)
	}
}

// checkShareLimits emits TorrentShareLimitReached, once per torrent and
// limit setting, for seeding torrents that reached their Share Limit.
func (e *Engine) checkShareLimits() {
	e.mu.Lock()
	all := make([]*entry, 0, len(e.torrents))
	for _, ent := range e.torrents {
		all = append(all, ent)
	}
	e.mu.Unlock()
	for _, ent := range all {
		if e.shareLimitReached(ent) {
			e.emit(core.TorrentEvent{Kind: core.TorrentShareLimitReached, Hash: ent.hash})
		}
	}
}

func (e *Engine) shareLimitReached(ent *entry) bool {
	now := e.now()
	ent.mu.Lock()
	defer ent.mu.Unlock()
	e.touchSeeding(ent, now)
	if ent.limitNotified || ent.seedSince.IsZero() {
		return false // not seeding, or already reported
	}
	size := ent.t.Length()
	if size <= 0 {
		return false
	}
	uploaded := ent.uploadedBase + uploadedBytes(ent.t)
	if !ent.limit.Reached(float64(uploaded)/float64(size), ent.seedingTime(now)) {
		return false
	}
	ent.limitNotified = true
	return true
}

// uploadedBytes is how much piece data t has sent since it was added.
func uploadedBytes(t *anacrolix.Torrent) int64 {
	st := t.Stats()
	return st.BytesWrittenData.Int64()
}
