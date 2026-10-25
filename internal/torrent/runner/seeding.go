package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// seedingStates are the States in which a torrent Download is Seeding.
var seedingStates = []core.State{core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed}

// sweep looks at every torrent Download once: it adds the torrents of
// Downloads that were Fetched to the engine unless they are there already (a
// Failed Download that never was Fetched has nothing to seed), and drops the
// torrents whose Download no longer exists.
func (r *Runner) sweep(ctx context.Context) error {
	var errs []error
	seen := map[core.ID]struct{}{}
	for _, st := range append([]core.State{core.StateFetchingMetadata, core.StateQueued, core.StateFetching}, seedingStates...) {
		ds, err := r.list(ctx, st)
		if err != nil {
			return err
		}
		seeding := slices.Contains(seedingStates, st)
		for _, d := range ds {
			seen[d.ID] = struct{}{}
			if !seeding || (st == core.StateFailed && d.FetchedAt.IsZero()) {
				continue
			}
			if r.lookup(d.ID) != nil || r.isBad(d.ID) || r.q.Held(d.ID) {
				continue
			}
			if err := r.registerOne(ctx, d); err != nil {
				errs = append(errs, err)
			}
		}
	}
	r.mu.Lock()
	var missing []core.ID
	for id := range r.tors {
		if _, ok := seen[id]; !ok {
			missing = append(missing, id)
		}
	}
	r.mu.Unlock()
	for _, id := range missing {
		// The lists are not a snapshot: a Download may have changed state
		// while they were read. Only a Download that is gone is dropped.
		if _, err := r.cfg.Downloads.Get(ctx, id); errors.Is(err, core.ErrNotFound) {
			r.drop(ctx, id)
		}
	}
	return errors.Join(errs...)
}

func (r *Runner) takeSweep() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.needSweep
	r.needSweep = false
	return was
}

func (r *Runner) registerOne(ctx context.Context, d core.Download) error {
	t, err := r.ensureRegistered(ctx, d, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.log.Debug("torrent runner: not seeding, the data is gone", "id", d.ID)
			return nil
		}
		return fmt.Errorf("torrent runner: seed %s: %w", d.ID, err)
	}
	r.mu.Lock()
	t.fetched = true
	r.mu.Unlock()
	return nil
}

// drop forgets the torrent of id: the engine lets go of it and keeps its
// files.
func (r *Runner) drop(ctx context.Context, id core.ID) {
	r.mu.Lock()
	t := r.tors[id]
	delete(r.tors, id)
	delete(r.hashes, id)
	r.mu.Unlock()
	if t == nil {
		return
	}
	t.cancel()
	r.throttle.Flush(id)
	if err := r.engine.Remove(context.WithoutCancel(ctx), t.hash); err != nil && !errors.Is(err, core.ErrNotFound) {
		r.log.Warn("torrent runner: cannot remove the torrent from the engine", "id", id, "err", err)
	}
}

func (r *Runner) isBad(id core.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, bad := r.bad[id]
	return bad
}

// effectiveLimit is the Share Limit that applies: the defaults, with the
// Download's own ratio and time where it has them.
func effectiveLimit(def core.SeedingSettings, ratio *float64, seeding *time.Duration) core.ShareLimit {
	l := def.ShareLimit()
	if ratio != nil {
		l.Ratio = *ratio
	}
	if seeding != nil {
		l.SeedingTime = *seeding
	}
	return l
}

// limitReached reports whether the Share Limit of rec is reached by the given
// totals. size is the torrent's size; with an unknown size only the seeding
// time can be reached.
func (r *Runner) limitReached(def core.SeedingSettings, rec core.TorrentRecord, size, uploaded int64, seeding time.Duration) bool {
	return reached(effectiveLimit(def, rec.RatioLimit, rec.SeedingTimeLimit), size, uploaded, seeding)
}

func reached(l core.ShareLimit, size, uploaded int64, seeding time.Duration) bool {
	var ratio float64
	if size > 0 {
		ratio = float64(uploaded) / float64(size)
	}
	return l.Reached(ratio, seeding)
}

// persistAll writes the Seeding counters of every torrent.
func (r *Runner) persistAll(ctx context.Context) {
	r.mu.Lock()
	all := make([]*tor, 0, len(r.tors))
	for _, t := range r.tors {
		all = append(all, t)
	}
	r.mu.Unlock()
	for _, t := range all {
		r.persistOne(ctx, t)
	}
}

// persistOne writes the Seeding counters of t unless they have not changed.
func (r *Runner) persistOne(ctx context.Context, t *tor) {
	st, err := r.engine.Stats(t.hash)
	if err != nil {
		return
	}
	r.mu.Lock()
	up, sec := t.baseUploaded+st.Uploaded, t.baseSeeding+st.SeedingTime.Truncate(time.Second)
	prevUp, prevSeed := t.persistedUp, t.persistedSeed
	changed := up != prevUp || sec != prevSeed
	if changed {
		t.persistedUp, t.persistedSeed = up, sec
	}
	r.mu.Unlock()
	if !changed {
		return
	}
	if err := r.updateRecord(ctx, t.id, func(rec *core.TorrentRecord) { rec.Uploaded, rec.SeedingTime = up, sec }); err != nil {
		// Not written: the next call must try again.
		r.mu.Lock()
		t.persistedUp, t.persistedSeed = prevUp, prevSeed
		r.mu.Unlock()
		if ctx.Err() == nil {
			r.log.Warn("torrent runner: cannot store the seeding counters", "id", t.id, "err", err)
		}
	}
}

// evaluateLimits applies the Share Limit to every Seeding torrent.
func (r *Runner) evaluateLimits(ctx context.Context) {
	settings, err := r.cfg.Settings(ctx)
	if err != nil {
		return
	}
	r.mu.Lock()
	var seeding []*tor
	for _, t := range r.tors {
		if t.fetched {
			seeding = append(seeding, t)
		}
	}
	r.mu.Unlock()
	for _, t := range seeding {
		r.evaluateOne(ctx, settings.Seeding, t)
	}
}

// evaluateOne checks the Share Limit of t. The counters are the persisted
// ones of earlier runs plus the engine's.
func (r *Runner) evaluateOne(ctx context.Context, def core.SeedingSettings, t *tor) {
	st, err := r.engine.Stats(t.hash)
	if err != nil {
		return
	}
	r.mu.Lock()
	l := effectiveLimit(def, t.ratioLimit, t.timeLimit)
	hit := reached(l, st.Size, t.baseUploaded+st.Uploaded, t.baseSeeding+st.SeedingTime)
	paused, removing := t.limitPaused, t.removing
	r.mu.Unlock()
	switch {
	case hit && !paused && !removing:
		r.applyAction(ctx, def.EffectiveAction(), t)
	case !hit && paused:
		r.mu.Lock()
		t.limitPaused = false
		r.mu.Unlock()
		r.log.Info("torrent runner: the share limit is no longer reached, seeding again", "id", t.id)
		if err := r.engine.Resume(t.hash); err != nil {
			r.log.Warn("torrent runner: cannot resume seeding", "id", t.id, "err", err)
		}
	}
}

// applyAction does what the Share Limit action says. Every action stops the
// upload at once; the remove actions then ask the Remover, and a failure
// leaves the torrent to be tried again at the next evaluation.
func (r *Runner) applyAction(ctx context.Context, action core.SeedingAction, t *tor) {
	r.log.Info("torrent runner: share limit reached", "id", t.id, "action", string(action))
	if err := r.engine.Pause(t.hash); err != nil {
		r.log.Warn("torrent runner: cannot pause the torrent", "id", t.id, "err", err)
	}
	remove := (action == core.SeedingActionRemove || action == core.SeedingActionRemoveData) && r.cfg.Remover != nil
	if (action == core.SeedingActionRemove || action == core.SeedingActionRemoveData) && r.cfg.Remover == nil {
		r.log.Warn("torrent runner: no way to remove downloads, pausing instead", "id", t.id)
		action = core.SeedingActionPause
	}
	r.mu.Lock()
	t.limitPaused, t.removing = !remove, remove
	r.mu.Unlock()
	r.cfg.Bus.Publish(ShareLimitReached{ID: t.id, Action: action})
	if !remove {
		return
	}
	r.wg.Go(func() {
		err := r.cfg.Remover.Remove(context.WithoutCancel(ctx), t.id, action == core.SeedingActionRemoveData)
		r.mu.Lock()
		t.removing = false
		r.mu.Unlock()
		switch {
		case err == nil, errors.Is(err, core.ErrNotFound):
		case errors.Is(err, core.ErrConflict):
			// Post-processing is still unpacking it: the next check tries again.
			r.log.Debug("torrent runner: download busy, removal postponed", "id", t.id)
		default:
			r.log.Warn("torrent runner: cannot remove the download, will try again", "id", t.id, "err", err)
		}
	})
}

// SetShareLimit gives the Download its own Share Limit, replacing any earlier
// one; a negative ratio or seeding time is unlimited. It takes effect at once:
// a torrent that now has reached the limit gets the configured action, and
// one that was paused by a limit that is no longer reached seeds again.
func (r *Runner) SetShareLimit(ctx context.Context, id core.ID, l core.ShareLimit) error {
	ratio, seeding := l.Ratio, l.SeedingTime
	return r.setOverride(ctx, id, &ratio, &seeding)
}

// ClearShareLimit removes the Download's own Share Limit, so the defaults of
// the Settings apply again.
func (r *Runner) ClearShareLimit(ctx context.Context, id core.ID) error {
	return r.setOverride(ctx, id, nil, nil)
}

func (r *Runner) setOverride(ctx context.Context, id core.ID, ratio *float64, seeding *time.Duration) error {
	if _, err := r.cfg.Downloads.Get(ctx, id); err != nil {
		return fmt.Errorf("torrent runner: share limit: %w", err)
	}
	r.recMu.Lock()
	rec, err := r.loadRecord(ctx, id)
	if err == nil {
		rec.RatioLimit, rec.SeedingTimeLimit = ratio, seeding
		err = r.cfg.Records.Upsert(ctx, rec)
	}
	r.recMu.Unlock()
	if err != nil {
		return fmt.Errorf("torrent runner: store the share limit: %w", err)
	}
	if t := r.lookup(id); t != nil {
		r.mu.Lock()
		t.ratioLimit, t.timeLimit = ratio, seeding
		fetched := t.fetched
		r.mu.Unlock()
		if fetched {
			if settings, err := r.cfg.Settings(ctx); err == nil {
				r.evaluateOne(ctx, settings.Seeding, t)
			}
		}
	}
	return nil
}

// EffectiveShareLimit returns the Share Limit that applies to the Download:
// the defaults of the Settings with the Download's own limits on top. A
// negative value is unlimited.
func (r *Runner) EffectiveShareLimit(ctx context.Context, id core.ID) (core.ShareLimit, error) {
	settings, err := r.cfg.Settings(ctx)
	if err != nil {
		return core.ShareLimit{}, err
	}
	rec, err := r.loadRecord(ctx, id)
	if err != nil {
		return core.ShareLimit{}, err
	}
	return effectiveLimit(settings.Seeding, rec.RatioLimit, rec.SeedingTimeLimit), nil
}
