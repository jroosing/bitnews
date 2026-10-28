package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// PostProcessorConfig wires a PostProcessor. Downloads, Bus, Repairer,
// Unpacker, Finisher, FetchReporter and DirFor are required.
type PostProcessorConfig struct {
	Downloads DownloadRepo
	Bus       *Bus

	Repairer      Repairer
	Unpacker      Unpacker
	Finisher      Finisher
	FetchReporter FetchReporter
	// DeferredVolumes is optional; without it a recovery shortfall is
	// final.
	DeferredVolumes DeferredVolumes
	// FS is optional; without it the working directory is not checked.
	// When the directory is missing the Download is taken to be finished
	// already (see Finisher) and only Finish runs.
	FS FS

	// Concurrency is how many Downloads are post-processed at once. Zero
	// means 1.
	Concurrency int
	// PollInterval is how often Run looks for Fetched Downloads besides
	// reacting to Fetched events. Zero means 10 seconds.
	PollInterval time.Duration
	// Clock returns the current time. Zero means time.Now.
	Clock func() time.Time
	// Logger is optional.
	Logger *slog.Logger
	// DirFor returns the working directory that holds a Download's fetched
	// files. The fetch side's layout is not core's business.
	DirFor func(d Download) string
}

// PostProcessor runs the Post-processing pipeline of Downloads. Create one
// with NewPostProcessor.
type PostProcessor struct {
	cfg PostProcessorConfig
	// sem holds one token per Download being post-processed.
	sem  chan struct{}
	wake chan struct{}

	mu       sync.Mutex // guards inflight
	inflight map[ID]struct{}
}

// ErrAlreadyPostProcessing is returned by Process for a Download this
// PostProcessor is working on right now.
var ErrAlreadyPostProcessing = errors.New("download is already being post-processed")

// NewPostProcessor validates cfg and returns a PostProcessor.
func NewPostProcessor(cfg PostProcessorConfig) (*PostProcessor, error) {
	var missing []string
	for _, f := range []struct {
		name string
		nil  bool
	}{
		{"Downloads", cfg.Downloads == nil},
		{"Bus", cfg.Bus == nil},
		{"Repairer", cfg.Repairer == nil},
		{"Unpacker", cfg.Unpacker == nil},
		{"Finisher", cfg.Finisher == nil},
		{"FetchReporter", cfg.FetchReporter == nil},
		{"DirFor", cfg.DirFor == nil},
	} {
		if f.nil {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("core: post-processor config lacks %s", strings.Join(missing, ", "))
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &PostProcessor{
		cfg:      cfg,
		sem:      make(chan struct{}, cfg.Concurrency),
		wake:     make(chan struct{}, 1),
		inflight: make(map[ID]struct{}),
	}, nil
}

// KindPostProcessStage is the EventKind of PostProcessStage. It is declared
// here rather than next to the other kinds so Post-processing stays in its
// own files.
const KindPostProcessStage EventKind = "postprocess_stage"

// Stage is a step of the Post-processing pipeline, in the order they run.
// Verify and unpack are conditional.
type Stage string

// The stages of Post-processing.
const (
	// StageVerifying covers the Repairer: one pass that checks the files
	// against the PAR2 set and repairs them.
	StageVerifying Stage = "verifying"
	// StageRepairing is not entered as a stage of its own (the repair is
	// part of the verifying pass); it names the failures of the repair.
	StageRepairing Stage = "repairing"
	StageUnpacking Stage = "unpacking"
	// StageFinishing covers the Finisher: cleanup and move to complete.
	StageFinishing Stage = "finishing"
)

// PostProcessStage says a Download entered a Post-processing stage. It is
// published after the DownloadUpdated that moved the Download to
// StatePostProcessing, because Download has no stage field.
type PostProcessStage struct {
	ID    ID
	Stage Stage
}

// Kind implements Event.
func (PostProcessStage) Kind() EventKind { return KindPostProcessStage }

// unpackSubdir is where unpacked files go, under the Download's directory.
const unpackSubdir = "_unpack"

// Process post-processes the Fetched Download id to completion: it moves
// the Download to StatePostProcessing, runs the stages, and moves it to
// StateReady or StateFailed. A pipeline failure is not an error of Process:
// it is the Download's Failed state. Process returns an error only when it
// could not do its job: the Download is missing or in a state it cannot be
// post-processed from, the repository failed, or ctx was cancelled (in which
// case the Download stays in StatePostProcessing for Recover).
func (p *PostProcessor) Process(ctx context.Context, id ID) error {
	if !p.claim(id) {
		return ErrAlreadyPostProcessing
	}
	defer p.unclaim(id)
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()
	return p.start(ctx, id)
}

// start moves the Fetched Download id to StatePostProcessing and runs it.
func (p *PostProcessor) start(ctx context.Context, id ID) error {
	d, err := p.cfg.Downloads.UpdateState(ctx, id, StateChange{To: StatePostProcessing, At: p.cfg.Clock()})
	if err != nil {
		return err
	}
	p.cfg.Bus.Publish(DownloadUpdated{Download: d})
	return p.run(ctx, d)
}

// resume runs the Download id, which a previous run left in
// StatePostProcessing, again from the first stage.
func (p *PostProcessor) resume(ctx context.Context, id ID) error {
	d, err := p.cfg.Downloads.Get(ctx, id)
	if err != nil {
		return err
	}
	if d.State != StatePostProcessing {
		return fmt.Errorf("core: resume post-processing of %s: %w", d.State, ErrIllegalTransition)
	}
	return p.run(ctx, d)
}

// Recover post-processes again, from the first stage, every Download found
// in StatePostProcessing: the daemon stopped while it was being worked on.
// It respects the concurrency limit and returns when they are all done. See
// the idempotency contract of the ports.
func (p *PostProcessor) Recover(ctx context.Context) error {
	var stuck []ID
	q := DownloadQuery{State: StatePostProcessing}
	for {
		page, next, err := p.cfg.Downloads.List(ctx, q)
		if err != nil {
			return err
		}
		for _, d := range page {
			stuck = append(stuck, d.ID)
		}
		if next == "" {
			break
		}
		q.After = next
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, id := range stuck {
		if !p.claim(id) {
			continue
		}
		if err := p.acquire(ctx); err != nil {
			p.unclaim(id)
			errs = append(errs, err)
			break
		}
		wg.Go(func() {
			defer p.unclaim(id)
			err := p.resume(ctx, id)
			p.release()
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// defaultPollInterval is how often Run looks for Fetched Downloads it was
// not told about.
const defaultPollInterval = 10 * time.Second

// Run post-processes Downloads as they become Fetched, until ctx is
// cancelled, and then waits for the running ones to stop. It first recovers
// Downloads left in StatePostProcessing. At most Concurrency Downloads are
// worked on at once; fetching is a separate component and unaffected.
func (p *PostProcessor) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	sub := p.cfg.Bus.Subscribe(ctx)
	defer func() { sub.Close() }()
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	wg.Go(func() {
		if err := p.Recover(ctx); err != nil && ctx.Err() == nil {
			p.cfg.Logger.Error("post-processing: recover failed", "err", err)
		}
		p.wakeUp()
	})
	p.dispatch(ctx, &wg)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				// The Bus dropped us for being slow: we may have missed a
				// Fetched event, so subscribe again and look.
				sub = p.cfg.Bus.Subscribe(ctx)
				p.dispatch(ctx, &wg)
				continue
			}
			if ev.Kind() == KindFetched {
				p.dispatch(ctx, &wg)
			}
		case <-p.wake:
			p.dispatch(ctx, &wg)
		case <-ticker.C:
			p.dispatch(ctx, &wg)
		}
	}
}

func (p *PostProcessor) wakeUp() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// dispatch starts Fetched Downloads, oldest first, while there are free
// slots. It does not wait for them.
func (p *PostProcessor) dispatch(ctx context.Context, wg *sync.WaitGroup) {
	for ctx.Err() == nil {
		select {
		case p.sem <- struct{}{}:
		default:
			return // all slots busy
		}
		id, ok := p.claimNextFetched(ctx)
		if !ok {
			p.release()
			return
		}
		wg.Go(func() {
			err := p.start(ctx, id)
			p.unclaim(id)
			p.release()
			switch {
			case err == nil:
				p.wakeUp() // a slot is free: look for more
			case ctx.Err() == nil:
				// Left for the next poll rather than retried in a hot loop.
				p.cfg.Logger.Error("post-processing: download not processed", "id", id, "err", err)
			}
		})
	}
}

// claimNextFetched claims the oldest Fetched Download nobody is working on.
func (p *PostProcessor) claimNextFetched(ctx context.Context) (ID, bool) {
	page, _, err := p.cfg.Downloads.List(ctx, DownloadQuery{State: StateFetched, Limit: 50})
	if err != nil {
		if ctx.Err() == nil {
			p.cfg.Logger.Error("post-processing: list fetched downloads", "err", err)
		}
		return "", false
	}
	for _, d := range page {
		if p.claim(d.ID) {
			return d.ID, true
		}
	}
	return "", false
}

// claim marks id as being worked on, and reports false if it already was.
func (p *PostProcessor) claim(id ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, busy := p.inflight[id]; busy {
		return false
	}
	p.inflight[id] = struct{}{}
	return true
}

func (p *PostProcessor) unclaim(id ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inflight, id)
}

// acquire waits for a free Post-processing slot.
func (p *PostProcessor) acquire(ctx context.Context) error {
	// A select with both cases ready picks one at random, so check first:
	// a cancelled context must never start new work.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *PostProcessor) release() { <-p.sem }

// run takes a Download that is in StatePostProcessing through the pipeline.
func (p *PostProcessor) run(ctx context.Context, d Download) error {
	out, err := p.pipeline(ctx, d)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errHandoff) {
		p.cfg.Logger.Info("post-processing handed back to the fetch side for recovery volumes", "id", d.ID)
		return nil
	}
	if err != nil {
		return p.fail(ctx, d, err)
	}
	ready, err := p.cfg.Downloads.UpdateState(ctx, d.ID, StateChange{To: StateReady, At: p.cfg.Clock(), OutputPath: out})
	if err != nil {
		return p.fail(ctx, d, &stageError{StageFinishing, err})
	}
	p.cfg.Bus.Publish(DownloadUpdated{Download: ready})
	p.cfg.Bus.Publish(Ready{ID: d.ID, OutputPath: out})
	return nil
}

// pipeline runs the stages and returns the output path. Errors it returns
// are stage errors for classify.
func (p *PostProcessor) pipeline(ctx context.Context, d Download) (string, error) {
	id := d.ID
	dir := p.cfg.DirFor(d)
	if p.cfg.FS != nil {
		ok, err := p.cfg.FS.DirExists(dir)
		if err != nil {
			return "", fmt.Errorf("check working directory: %w", err)
		}
		if !ok {
			// A previous run got through Finish, which moved the
			// directory, and stopped before recording Ready. Finish is
			// idempotent and finds the moved result again, so go
			// straight to it.
			return p.finish(ctx, d, dir, "")
		}
	}
	// Torrents are verified by the torrent engine piece by piece, so only
	// Usenet Downloads go through verify and repair.
	if d.SourceKind != SourceTorrent {
		if err := p.verifyAndRepair(ctx, d, dir); err != nil {
			return "", err
		}
	}
	var unpacked string
	found, err := p.cfg.Unpacker.Detect(ctx, dir)
	if err != nil {
		return "", &stageError{StageUnpacking, err}
	}
	if found {
		p.stage(id, StageUnpacking)
		dst := filepath.Join(dir, unpackSubdir)
		if _, err := p.cfg.Unpacker.Unpack(ctx, dir, dst, UnpackOptions{DownloadID: id}); err != nil {
			return "", &stageError{StageUnpacking, err}
		}
		unpacked = dst
	}
	return p.finish(ctx, d, dir, unpacked)
}

// finish runs the finishing stage.
func (p *PostProcessor) finish(ctx context.Context, d Download, dir, unpacked string) (string, error) {
	p.stage(d.ID, StageFinishing)
	out, err := p.cfg.Finisher.Finish(ctx, d, dir, unpacked)
	if err != nil {
		return "", &stageError{StageFinishing, err}
	}
	return out, nil
}

// verifyAndRepair runs the verifying stage: one Repair that checks the
// files against the PAR2 set and repairs what it can. Without a PAR2 set
// any damage the fetch side saw is final.
func (p *PostProcessor) verifyAndRepair(ctx context.Context, d Download, dir string) error {
	p.stage(d.ID, StageVerifying)
	out, err := p.cfg.Repairer.Repair(ctx, dir)
	var short *RecoveryShortfall
	switch {
	case errors.As(err, &short):
		return p.shortfall(ctx, d, short)
	case errors.Is(err, ErrRepairFailed):
		return &stageError{StageRepairing, err}
	case err != nil:
		return &stageError{StageVerifying, err}
	}
	if out.HasPAR2 {
		return nil
	}
	rep, err := p.cfg.FetchReporter.FetchReport(ctx, d)
	if err != nil {
		return &stageError{StageVerifying, err}
	}
	if rep.Damaged() {
		return &stageError{StageVerifying, fmt.Errorf("%w: %d missing, %d damaged and no PAR2 set",
			ErrMissingArticles, rep.MissingArticles, rep.DamagedArticles)}
	}
	return nil
}

// errHandoff is the pipeline's way of saying it stopped on purpose: the fetch
// side took the Download back (deferred recovery volumes) and re-enters
// Post-processing when it is Fetched again.
var errHandoff = errors.New("handed back to the fetch side")

// shortfall handles too little recovery data: it returns errHandoff when
// the fetch side will fetch more, and the shortfall itself otherwise.
func (p *PostProcessor) shortfall(ctx context.Context, d Download, short *RecoveryShortfall) error {
	if p.cfg.DeferredVolumes == nil {
		return &stageError{StageRepairing, short}
	}
	more, err := p.cfg.DeferredVolumes.FetchDeferred(ctx, d, max(short.Needed-short.Available, 1))
	if err != nil {
		return &stageError{StageRepairing, err}
	}
	if more {
		return errHandoff
	}
	return &stageError{StageRepairing, short}
}

// stageError says which stage an error came from.
type stageError struct {
	stage Stage
	err   error
}

func (e *stageError) Error() string { return string(e.stage) + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// failureTable maps the errors the ports return to failure reasons. The
// first match wins.
var failureTable = []struct {
	err    error
	reason FailureReason
}{
	{ErrInsufficientRecovery, FailureRepairInsufficient},
	{ErrRepairFailed, FailureRepairInsufficient},
	{ErrMissingArticles, FailureMissingArticles},
	{ErrArchiveUnsupported, FailureArchiveUnsupported},
	{ErrArchivePassword, FailureArchivePassword},
	{ErrArchiveCorrupt, FailureArchiveCorrupt},
	{ErrPathViolation, FailurePathViolation},
	{ErrDiskFull, FailureDiskFull},
}

// classify returns the failure reason for a stage error: FailureInternal
// when it matches nothing in failureTable.
func classify(err error) FailureReason {
	for _, row := range failureTable {
		if errors.Is(err, row.err) {
			return row.reason
		}
	}
	return FailureInternal
}

// maxDetailRunes caps a failure detail.
const maxDetailRunes = 200

// detailer is implemented by errors that vouch for their own failure text,
// such as fsutil's permission error, which names a folder from the settings
// and the user id. Their text keeps its path; any other error is redacted.
type detailer interface{ FailureDetail() string }

// failureDetail is the stored detail of a failure: the vouched text of the
// first error in the chain that has one, else the redacted error text.
func failureDetail(err error) string {
	var d detailer
	if errors.As(err, &d) {
		return truncateDetail(d.FailureDetail())
	}
	return redactDetail(err.Error())
}

func truncateDetail(s string) string {
	if utf8.RuneCountInString(s) > maxDetailRunes {
		s = string([]rune(s)[:maxDetailRunes])
	}
	return s
}

var (
	// windowsPath matches drive-letter and UNC paths.
	windowsPath = regexp.MustCompile(`[A-Za-z]:\\[^\s"'()<>|;,]*|\\\\[^\s"'()<>|;,]+`)
	// unixPath matches an absolute path that starts a word.
	unixPath = regexp.MustCompile(`(^|[\s"'(\[=])/[^\s"'()\[\]:;,]+`)
)

// redactDetail makes an error text safe to store and show: absolute paths
// become "<dir>" (they can reveal the layout of the host or the name of a
// release) and the result is cut to maxDetailRunes.
func redactDetail(s string) string {
	s = windowsPath.ReplaceAllString(s, "<dir>")
	s = unixPath.ReplaceAllString(s, "${1}<dir>")
	return truncateDetail(s)
}

// fail moves d to StateFailed for err and publishes the events. It never
// touches the Download's data: torrents keep seeding.
func (p *PostProcessor) fail(ctx context.Context, d Download, err error) error {
	reason, detail := classify(err), failureDetail(err)
	failed, uerr := p.cfg.Downloads.UpdateState(ctx, d.ID, StateChange{
		To: StateFailed, At: p.cfg.Clock(), FailureReason: reason, FailureDetail: detail,
	})
	if uerr != nil {
		return uerr
	}
	p.cfg.Logger.Warn("post-processing failed", "id", d.ID, "reason", reason, "detail", detail)
	p.cfg.Bus.Publish(DownloadUpdated{Download: failed})
	p.cfg.Bus.Publish(Failed{ID: d.ID, Reason: reason, Detail: detail})
	return nil
}

func (p *PostProcessor) stage(id ID, s Stage) {
	p.cfg.Bus.Publish(PostProcessStage{ID: id, Stage: s})
}
