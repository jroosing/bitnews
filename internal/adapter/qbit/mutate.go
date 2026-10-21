package qbit

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
)

// each acts on several torrents. Errors that only mean "that torrent is gone
// or cannot do this" are skipped, as qBittorrent does; anything else fails the
// call, and each reports whether it went through.
func (c *call) each(es []entry, act func(entry) error) bool {
	for _, e := range es {
		if err := act(e); err != nil && !ignorable(err) {
			c.appFail(err)
			return false
		}
	}
	return true
}

// pause serves torrents/pause and torrents/stop. Only a Download that is not
// Fetched yet can be paused; for the others there is nothing left to pause
// and the call does nothing.
func (c *call) pause() { c.setPaused(true) }

// resume serves torrents/resume and torrents/start.
func (c *call) resume() { c.setPaused(false) }

func (c *call) setPaused(paused bool) {
	es, ok := c.targets()
	if !ok {
		return
	}
	if c.each(es, func(e entry) error {
		var err error
		if paused {
			_, err = c.h.Downloads.Pause(c.ctx(), e.d.ID)
		} else {
			_, err = c.h.Downloads.Resume(c.ctx(), e.d.ID)
		}
		return err
	}) {
		c.ok()
	}
}

// delete serves torrents/delete. The Download goes either way; deleteFiles
// decides whether its data goes with it. For a torrent the data is the seed:
// without deleteFiles the files stay on disk and nothing seeds them any more.
// A Download that is being Post-processed is not deleted, because its files
// are in use.
func (c *call) delete() {
	es, ok := c.targets()
	if !ok {
		return
	}
	deleteFiles := isTrue(c.get("deleteFiles"))
	if c.each(es, func(e entry) error {
		if e.d.State == core.StateFetched || e.d.State == core.StatePostProcessing {
			return nil
		}
		return c.h.Downloads.Delete(c.ctx(), e.d.ID, deleteFiles)
	}) {
		c.ok()
	}
}

// setCategory serves torrents/setCategory. The Category must exist (409
// otherwise, like qBittorrent); an empty name removes it.
func (c *call) setCategory() {
	es, ok := c.targets()
	if !ok {
		return
	}
	var id core.ID
	if name := c.get("category"); name != "" {
		cats, err := c.h.Categories.List(c.ctx())
		if err != nil {
			c.appFail(err)
			return
		}
		cat, found := findCategory(cats, name)
		if !found {
			c.text(409, "Category name does not exist")
			return
		}
		id = cat.ID
	}
	if c.each(es, func(e entry) error {
		_, err := c.h.Downloads.Update(c.ctx(), e.d.ID, app.Patch{Category: app.SetCategory(id)})
		return err
	}) {
		c.ok()
	}
}

// byQueue orders entries the way their Queue runs them.
func byQueue(a, b entry) int {
	if a.d.QueuePosition != b.d.QueuePosition {
		return a.d.QueuePosition - b.d.QueuePosition
	}
	if c := a.d.AddedAt.Compare(b.d.AddedAt); c != 0 {
		return c
	}
	return strings.Compare(string(a.d.ID), string(b.d.ID))
}

// move serves topPrio, bottomPrio, increasePrio and decreasePrio. Several
// torrents keep their order relative to each other. A Download that is not in
// its Queue (Fetched on) is skipped.
func (c *call) move(m app.Move) {
	es, ok := c.targets()
	if !ok {
		return
	}
	slices.SortFunc(es, byQueue)
	// Moving to the top one by one reverses the order, so start at the end;
	// the same goes for moving down one place.
	if m == app.MoveTop || m == app.MoveDown {
		slices.Reverse(es)
	}
	if c.each(es, func(e entry) error {
		_, err := c.h.Downloads.Update(c.ctx(), e.d.ID, app.Patch{Move: m})
		return err
	}) {
		c.ok()
	}
}

func (c *call) topPrio() { c.move(app.MoveTop) }

func (c *call) bottomPrio() { c.move(app.MoveBottom) }

func (c *call) increasePrio() { c.move(app.MoveUp) }

func (c *call) decreasePrio() { c.move(app.MoveDown) }

// setForceStart serves torrents/setForceStart: value=true gives the Download
// the priority force, false takes it back to normal.
func (c *call) setForceStart() {
	es, ok := c.targets()
	if !ok {
		return
	}
	p := core.DownloadPriorityNormal
	if isTrue(c.get("value")) {
		p = core.DownloadPriorityForce
	}
	if c.each(es, func(e entry) error {
		if e.d.Priority == p {
			return nil
		}
		_, err := c.h.Downloads.Update(c.ctx(), e.d.ID, app.Patch{Priority: &p})
		return err
	}) {
		c.ok()
	}
}

// Longest seeding time accepted: a hundred years, in minutes. It keeps the
// conversion to seconds from overflowing.
const maxLimitMinutes = 100 * 365 * 24 * 60

// shareArg reads one share limit parameter: qBittorrent's codes are -2 for
// the global default, -1 for none, or the value.
func shareArg(raw string, float bool) (v float64, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if float {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(min(max(n, -2), maxLimitMinutes)), true
}

// resolveShare turns qBittorrent's limit codes into one call of the port. If
// both are -2 the global default applies. If only one is, that one takes the
// global value now (unlimited when the global limit is off), because the port
// has a single "use global" flag.
func (h *handler) resolveShare(ratio, minutes float64) (r float64, seconds int64, global bool) {
	if ratio == -2 && minutes == -2 {
		return 0, 0, true
	}
	var g GlobalShare
	if h.GlobalShare != nil {
		g = h.GlobalShare()
	}
	if ratio == -2 {
		ratio = -1
		if g.RatioEnabled {
			ratio = g.Ratio
		}
	}
	if minutes == -2 {
		minutes = -1
		if g.SeedingTimeEnabled {
			minutes = float64(g.SeedingMinutes)
		}
	}
	r, seconds = -1, -1
	if ratio >= 0 {
		r = ratio
	}
	if minutes >= 0 {
		seconds = int64(minutes) * 60
	}
	return r, seconds, false
}

// setShareLimits serves torrents/setShareLimits. inactiveSeedingTimeLimit is
// ignored.
func (c *call) setShareLimits() {
	ratio, ok1 := shareArg(c.params.Get("ratioLimit"), true)
	minutes, ok2 := shareArg(c.params.Get("seedingTimeLimit"), false)
	if !ok1 || !ok2 {
		c.text(400, "Bad Request")
		return
	}
	es, ok := c.targets()
	if !ok {
		return
	}
	r, secs, global := c.h.resolveShare(ratio, minutes)
	if c.each(es, func(e entry) error {
		return c.h.ShareLimits.SetShareLimit(c.ctx(), e.d.ID, r, secs, global)
	}) {
		c.ok()
	}
}
