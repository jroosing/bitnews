package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/usenet/filewriter"
	"github.com/jroosing/bitnews/internal/usenet/nntp"
	"github.com/jroosing/bitnews/internal/usenet/nzb"
	"github.com/jroosing/bitnews/internal/usenet/scheduler"
)

var errReschedule = errors.New("runner: servers changed")

// job is the control handle of one running fetch.
type job struct {
	id       core.ID
	cancelFn context.CancelCauseFunc
	// done is closed when the job's goroutine has ended.
	done chan struct{}

	mu       sync.Mutex
	sched    context.CancelCauseFunc // cancels the current scheduler run
	finished bool                    // the Download is about to leave Fetching
}

func newJob(id core.ID, cancel context.CancelCauseFunc) *job {
	return &job{id: id, cancelFn: cancel, done: make(chan struct{})}
}

// cancel stops the fetch for cause, unless it is already finishing.
func (j *job) cancel(cause error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.finished {
		j.cancelFn(cause)
	}
}

// reschedule restarts the scheduler with the current Servers.
func (j *job) reschedule() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.sched != nil {
		j.sched(errReschedule)
	}
}

func (j *job) setSched(c context.CancelCauseFunc) {
	j.mu.Lock()
	j.sched = c
	j.mu.Unlock()
}

// finish marks the job as past the point of cancellation and reports
// whether it could (false when it was cancelled first).
func (j *job) finish() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finished = true
	return true
}

// jobState is the shared state of one fetch, updated from the scheduler's
// worker goroutines.
type jobState struct {
	mu    sync.Mutex
	seg   map[segKey]segStatus
	done  int64 // decoded bytes written, including restored ones
	fatal error
	// nextSave is when the resume blob is saved next.
	nextSave time.Time
	rate     *rateWindow
	// total is the Download's size in bytes.
	total int64

	// Counted for this run only, to recognize unusable Servers.
	runDone, runMissing int
	answered            atomic.Int64
	// finalSent is set when a progress event with done == total went out.
	finalSent atomic.Bool
	// supplement marks a run that only fetches un-deferred recovery
	// volumes (indexes in volumes) of a Download fetched before. Its
	// progress, report and totals are not published: they would replace
	// those of the Download's main fetch.
	supplement bool
	volumes    []int
}

func newJobState() *jobState {
	return &jobState{seg: map[segKey]segStatus{}, rate: newRateWindow(rateWindowSpan)}
}

func (st *jobState) fatalErr() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.fatal
}

// unusable reports that this run resolved Articles only as Missing and no
// Server ever answered a request.
func (st *jobState) unusable() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.runDone == 0 && st.runMissing > 0 && st.answered.Load() == 0
}

// bgCtx returns a context for writes that must happen even though the job's
// context is cancelled.
func bgCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// loadNZB parses the stored NZB of d.
func (r *Runner) loadNZB(ctx context.Context, d core.Download) (*nzb.NZB, error) {
	path, _, err := r.cfg.Sources.Source(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("find nzb: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open nzb: %w", err)
	}
	defer func() { _ = f.Close() }()
	n, err := nzb.Parse(f, nzb.DefaultLimits())
	if err != nil {
		return nil, fmt.Errorf("parse nzb: %w", err)
	}
	return n, nil
}

// fetchOne runs the whole fetch of one active Download d, which is Fetching.
// It returns when the Download is Fetched, Failed or requeued, or when ctx
// is cancelled (see the err* causes).
func (r *Runner) fetchOne(ctx context.Context, j *job, d core.Download) {
	n, err := r.loadNZB(ctx, d)
	if err != nil {
		if ctx.Err() == nil {
			r.fail(ctx, j, d, core.FailureSourceInvalid, err.Error())
		}
		return
	}
	und, err := r.volumes.Undeferred(ctx, d.ID)
	if err != nil {
		if ctx.Err() == nil {
			r.fail(ctx, j, d, core.FailureInternal, fmt.Sprintf("read un-deferred volumes: %v", err))
		}
		return
	}
	// A Download with un-deferred volumes was fetched before: only the
	// volumes that are not fetched yet are left to do (a supplement run).
	var supplement map[int]bool
	if len(und) > 0 {
		supplement = map[int]bool{}
		for fi, fetched := range und {
			if !fetched {
				supplement[fi] = true
			}
		}
	}
	r.pools.revive()
	p := buildPlan(n, supplement)
	if len(p.articles) == 0 && supplement == nil {
		r.fail(ctx, j, d, core.FailureSourceInvalid, "the NZB has nothing to fetch")
		return
	}

	dir := r.cfg.DirFor(d)
	w, st, err := r.openWriter(ctx, d, p, dir, supplement != nil)
	if err != nil {
		r.fail(ctx, j, d, core.FailureInternal, err.Error())
		return
	}
	defer func() { _ = w.Close() }()
	if supplement != nil {
		st.supplement = true
		st.volumes = slices.Sorted(maps.Keys(supplement))
	}
	now := r.clock.Now()
	st.nextSave = now.Add(r.cfg.SaveInterval)
	st.rate.add(now, st.done)
	st.total = d.TotalBytes
	if st.total == 0 {
		st.total = p.encoded
	}
	for {
		remaining := p.remaining(st.seg)
		if len(remaining) == 0 {
			break
		}
		servers := r.pools.active()
		if len(servers) == 0 {
			r.requeue(ctx, j, d, 0)
			return
		}
		sctx, scancel := context.WithCancelCause(ctx)
		j.setSched(scancel)
		sch, err := scheduler.New(scheduler.Config{
			Servers:  r.schedulerServers(servers, st),
			Articles: remaining,
			OnResult: func(res scheduler.Result) { r.onResult(ctx, j, d.ID, scancel, w, st, res) },
			OnServerDown: func(name string, err error) {
				r.log.Warn("usenet server down", "server", name, "download", d.ID, "err", err)
			},
			Now:     r.clock.Now,
			Backoff: r.cfg.Backoff,
		})
		if err != nil {
			scancel(nil)
			r.fail(ctx, j, d, core.FailureInternal, err.Error())
			return
		}
		runErr := sch.Run(sctx)
		cause := context.Cause(sctx)
		j.setSched(nil)
		scancel(nil)
		if runErr == nil || st.fatalErr() != nil {
			break
		}
		if ctx.Err() == nil && errors.Is(cause, errReschedule) {
			continue
		}
		if ctx.Err() != nil {
			r.stopped(ctx, d, w, st)
			return
		}
		r.fail(ctx, j, d, core.FailureInternal, fmt.Sprintf("fetch stopped: %v", runErr))
		return
	}
	if err := st.fatalErr(); err != nil {
		if errors.Is(err, syscall.ENOSPC) {
			// The disk filled up behind the guard's back.
			r.stopped(ctx, d, w, st)
			r.q.DiskFull("the disk is full: " + err.Error())
			r.requeue(ctx, j, d, 0)
			return
		}
		r.fail(ctx, j, d, core.FailureInternal, err.Error())
		return
	}
	if st.unusable() {
		// No Server answered a single request: wrong password or a provider
		// outage. Marking everything Missing would burn through the Queue.
		r.log.Warn("usenet: no server answered, download waits before retrying", "id", d.ID, "wait", r.cfg.FailureBackoff)
		r.requeue(ctx, j, d, r.cfg.FailureBackoff)
		return
	}
	r.complete(ctx, j, d, p, w, st)
}

// schedulerServers adapts the pools for the scheduler.
func (r *Runner) schedulerServers(entries []poolEntry, st *jobState) []scheduler.Server {
	out := make([]scheduler.Server, len(entries))
	for i, e := range entries {
		out[i] = scheduler.Server{
			Name: e.spec.Name, Priority: e.spec.Priority, RetentionDays: e.spec.RetentionDays,
			Workers: e.spec.MaxConnections, Pool: &countingFetcher{inner: e.pool, st: st, lim: r.lim},
		}
	}
	return out
}

// countingFetcher records that a Server answered a request and applies the
// speed limit to the bytes it receives.
type countingFetcher struct {
	inner scheduler.Fetcher
	st    *jobState
	lim   *limiter
}

func (c *countingFetcher) Body(ctx context.Context, id string, sink func([]byte) error) error {
	err := c.inner.Body(ctx, id, func(line []byte) error {
		if err := c.lim.wait(ctx, len(line)+2); err != nil {
			return err
		}
		return sink(line)
	})
	if err == nil || errors.Is(err, nntp.ErrArticleNotFound) {
		c.st.answered.Add(1)
	}
	return err
}

// onResult records one Article's outcome. It runs on the scheduler's worker
// goroutines.
func (r *Runner) onResult(ctx context.Context, j *job, id core.ID, stop context.CancelCauseFunc, w *filewriter.Writer, st *jobState, res scheduler.Result) {
	key := segKey{res.Article.FileKey, res.Article.SegmentNumber}
	status := segMissing
	var written int64
	if res.Status == scheduler.Done && res.Part != nil {
		err := w.WritePart(fileKey(key.File), *res.Part)
		switch {
		case err == nil:
			status = segDone
			if res.Damaged {
				status = segDamaged
			}
			written = int64(len(res.Part.Data))
		case errors.Is(err, filewriter.ErrInvalidPart), errors.Is(err, filewriter.ErrSizeMismatch), errors.Is(err, filewriter.ErrTooLarge):
			// A hostile or inconsistent Article counts as missing.
			r.log.Warn("usenet article rejected by the file writer", "message_id", res.Article.MessageID, "err", err)
		default:
			st.mu.Lock()
			st.fatal = err
			st.mu.Unlock()
			stop(err)
			return
		}
	}
	st.mu.Lock()
	st.seg[key] = status
	st.done += written
	if status == segMissing {
		st.runMissing++
	} else {
		st.runDone++
	}
	now := r.clock.Now()
	saveDue := now.After(st.nextSave)
	if saveDue {
		st.nextSave = now.Add(r.cfg.SaveInterval)
	}
	st.rate.add(now, st.done)
	done, total, rate := st.done, st.total, st.rate.rate(now)
	remaining := max(total-done, 0)
	st.mu.Unlock()

	if st.supplement {
		// A supplement run reports no progress, so it asks the disk guard
		// itself. A Queue paused for the disk does not stop it: it is not
		// one of the Queue's fetches.
		if !r.q.CheckDisk(ctx, id, remaining) {
			j.cancel(errDiskFull)
			return
		}
	} else if r.q.Progress(ctx, id, done, total, rate) && total > 0 && done == total {
		st.finalSent.Store(true)
	}
	if saveDue {
		bctx, cancel := bgCtx(ctx)
		defer cancel()
		if err := r.saveBlob(bctx, id, w, st); err != nil {
			r.log.Warn("usenet runner: cannot save resume state", "id", id, "err", err)
		}
	}
}

// fail moves the Download to Failed.
func (r *Runner) fail(ctx context.Context, j *job, d core.Download, reason core.FailureReason, detail string) {
	j.finish()
	bctx, cancel := bgCtx(ctx)
	defer cancel()
	_ = r.store.Delete(bctx, d.ID)
	_, err := r.q.SetState(bctx, d.ID, core.StateChange{
		To: core.StateFailed, At: r.clock.Now(), FailureReason: reason, FailureDetail: detail,
	})
	if err != nil {
		r.log.Warn("usenet runner: cannot fail download", "id", d.ID, "err", err)
		return
	}
	r.cfg.Bus.Publish(core.Failed{ID: d.ID, Reason: reason, Detail: detail})
}

// requeue puts the Download back to Queued and, when wait is positive, keeps
// it from being activated again for that long.
func (r *Runner) requeue(ctx context.Context, j *job, d core.Download, wait time.Duration) {
	j.finish()
	bctx, cancel := bgCtx(ctx)
	defer cancel()
	if wait > 0 {
		r.mu.Lock()
		r.cooldown[d.ID] = r.clock.Now().Add(wait)
		r.mu.Unlock()
	}
	if _, err := r.q.SetState(bctx, d.ID, core.StateChange{To: core.StateQueued, At: r.clock.Now()}); err != nil {
		r.log.Warn("usenet runner: cannot requeue download", "id", d.ID, "err", err)
	}
}

// complete finishes the files and moves the Download to Fetched.
func (r *Runner) complete(ctx context.Context, j *job, d core.Download, p plan, w *filewriter.Writer, st *jobState) {
	detail := FetchDetail{}
	st.mu.Lock()
	for k, s := range st.seg {
		switch s {
		case segMissing:
			detail.MissingArticles++
			detail.MissingBytes += p.byKey[k].ExpectedBytes
		case segDamaged:
			detail.DamagedArticles++
		}
	}
	doneBytes := st.done
	st.mu.Unlock()

	var errs []error
	for _, fi := range p.fileKeys {
		fs, ok := w.Ranges(fileKey(fi))
		if !ok {
			detail.MissingFiles++
			continue
		}
		detail.DamagedBytes += fs.DamagedBytes()
		if err := w.Finish(fileKey(fi)); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, w.Sync(), w.Close())
	if err := errors.Join(errs...); err != nil {
		r.fail(ctx, j, d, core.FailureInternal, fmt.Sprintf("finish files: %v", err))
		return
	}

	j.finish()
	bctx, cancel := bgCtx(ctx)
	defer cancel()
	_ = r.store.Delete(bctx, d.ID)
	if st.supplement {
		// Mark the volumes before the state change: a crash in between
		// leaves a Fetching Download with nothing to do, which completes.
		if err := r.volumes.MarkFetched(bctx, d.ID, st.volumes); err != nil {
			r.fail(ctx, j, d, core.FailureInternal, fmt.Sprintf("record fetched volumes: %v", err))
			return
		}
	} else {
		r.storeDoneBytes(bctx, d.ID, doneBytes, p.encoded)
		if !st.finalSent.Load() {
			r.cfg.Bus.Publish(core.DownloadProgress{ID: d.ID, DoneBytes: doneBytes, TotalBytes: max(st.total, doneBytes)})
		}
		r.q.ForgetProgress(d.ID)
		r.mu.Lock()
		r.reports[d.ID] = detail
		r.mu.Unlock()
		if r.cfg.Reports != nil {
			if err := r.cfg.Reports.SaveReport(bctx, d.ID, detail); err != nil {
				r.log.Warn("usenet runner: cannot store fetch report", "id", d.ID, "err", err)
			}
		}
	}
	if _, err := r.q.SetState(bctx, d.ID, core.StateChange{To: core.StateFetched, At: r.clock.Now()}); err != nil {
		r.log.Warn("usenet runner: cannot mark download fetched", "id", d.ID, "err", err)
		return
	}
	r.cfg.Bus.Publish(core.Fetched{ID: d.ID})
}

// openWriter creates the Writer for d's directory and the job state. When
// the ProgressStore holds a valid blob, the files and the resolved Articles
// are taken over; otherwise the directory is cleared (see the package
// documentation) and the fetch starts from scratch.
//
// For a supplement run (only un-deferred recovery volumes), the files of the
// earlier fetch are not touched: a blob is used only if it is about the
// volumes, and without one just leftovers of the volumes are removed.
func (r *Runner) openWriter(ctx context.Context, d core.Download, p plan, dir string, supplement bool) (*filewriter.Writer, *jobState, error) {
	st := newJobState()
	if blob, err := r.store.Load(ctx, d.ID); err == nil {
		rb, derr := decodeBlob(blob)
		if derr == nil && supplement && !blobIsAbout(rb, p) {
			derr = errors.New("the blob is not about the recovery volumes")
		}
		if derr == nil {
			w, err := filewriter.New(dir)
			if err != nil {
				return nil, nil, err
			}
			if reset, rerr := w.Restore(rb.Writer); rerr == nil {
				r.adoptBlob(w, st, p, rb, reset)
				return w, st, nil
			} else {
				derr = rerr
			}
			_ = w.Close()
		}
		r.log.Warn("usenet resume state unusable, starting over", "id", d.ID, "err", derr)
	} else if !errors.Is(err, core.ErrNotFound) {
		r.log.Warn("usenet resume state unreadable, starting over", "id", d.ID, "err", err)
	}
	var werr error
	if supplement {
		werr = removeNamed(dir, p.names)
	} else {
		werr = wipeDir(dir)
	}
	if werr != nil {
		return nil, nil, werr
	}
	w, err := filewriter.New(dir)
	if err != nil {
		return nil, nil, err
	}
	return w, st, nil
}

// adoptBlob loads the resolved Articles of rb into st, except those of files
// the writer had to reset or forget, and recomputes the done bytes from the
// writer.
func (r *Runner) adoptBlob(w *filewriter.Writer, st *jobState, p plan, rb resumeBlob, reset []filewriter.FileKey) {
	resetSet := map[int]bool{}
	for _, k := range reset {
		resetSet[int(k)] = true
	}
	for _, rec := range rb.Segments {
		if _, known := p.byKey[rec.Key]; !known || resetSet[rec.Key.File] {
			continue
		}
		if rec.Status != segMissing {
			if _, ok := w.Ranges(fileKey(rec.Key.File)); !ok {
				continue
			}
		}
		st.seg[rec.Key] = rec.Status
	}
	for _, fi := range p.fileKeys {
		if fs, ok := w.Ranges(fileKey(fi)); ok {
			st.done += fs.DoneBytes() + fs.DamagedBytes()
		}
	}
}

// saveBlob stores the resume state. The resolved Articles are copied before
// the writer snapshot is taken, so the snapshot always covers them.
func (r *Runner) saveBlob(ctx context.Context, id core.ID, w *filewriter.Writer, st *jobState) error {
	st.mu.Lock()
	recs := make([]segRecord, 0, len(st.seg))
	for k, s := range st.seg {
		recs = append(recs, segRecord{Key: k, Status: s})
	}
	st.mu.Unlock()
	slices.SortFunc(recs, func(a, b segRecord) int {
		return cmp.Or(cmp.Compare(a.Key.File, b.Key.File), cmp.Compare(a.Key.Seg, b.Key.Seg))
	})
	snap, err := w.Snapshot()
	if err != nil {
		return err
	}
	return r.store.Save(ctx, id, encodeBlob(resumeBlob{Writer: snap, Segments: recs}))
}

// stopped handles a fetch whose context ended: the resume state is saved so
// the work is not lost, unless the Download was removed.
func (r *Runner) stopped(ctx context.Context, d core.Download, w *filewriter.Writer, st *jobState) {
	bctx, cancel := bgCtx(ctx)
	defer cancel()
	if errors.Is(context.Cause(ctx), errRemoved) {
		_ = r.store.Delete(bctx, d.ID)
		return
	}
	if err := r.saveBlob(bctx, d.ID, w, st); err != nil {
		r.log.Warn("usenet runner: cannot save resume state", "id", d.ID, "err", err)
	}
}

// storeDoneBytes writes the Download's DoneBytes, and its TotalBytes when
// they are still unknown (zero), to the repository. It uses UpdateProgress,
// which writes only those columns: a whole-row Update here could overwrite a
// concurrent edit of the Priority, Category or Paused flag. The Get only
// decides whether the total is unknown.
func (r *Runner) storeDoneBytes(ctx context.Context, id core.ID, done, total int64) {
	cur, err := r.cfg.Downloads.Get(ctx, id)
	if err != nil {
		return
	}
	if cur.TotalBytes != 0 {
		total = 0 // known already: keep the stored total
	}
	if err := r.cfg.Downloads.UpdateProgress(ctx, id, done, total); err != nil && !errors.Is(err, core.ErrNotFound) {
		r.log.Warn("usenet runner: cannot store done bytes", "id", id, "err", err)
	}
}
