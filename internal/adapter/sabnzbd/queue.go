package sabnzbd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/app"
	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/view"
)

// queueMode serves mode=queue: the listing, and the name=... actions on one
// Download.
func (c *call) queueMode() {
	switch strings.ToLower(c.get("name")) {
	case "":
		c.queueList()
	case "delete":
		c.queueDelete()
	case "priority":
		c.queuePriority()
	case "pause":
		c.queuePause(true)
	case "resume":
		c.queuePause(false)
	case "switch":
		c.queueSwitch()
	default:
		c.fail("not implemented")
	}
}

// queueList answers the queue: the not-yet-Fetched NZB Downloads, which
// includes a Download that went back to Fetching to get deferred PAR2
// volumes. cat filters by Category name; start and limit page the slots.
func (c *call) queueList() {
	f, err := c.readCatFilter(c.get("cat"))
	if err != nil {
		c.appFail(err)
		return
	}
	all, err := c.queueViews()
	if err != nil {
		c.appFail(err)
		return
	}
	index := make(map[core.ID]int, len(all))
	shown := make([]view.View, 0, len(all))
	for i, v := range all {
		index[v.Download.ID] = i
		if f.matches(v.Download) {
			shown = append(shown, v)
		}
	}

	bps, paused := c.h.speed()
	start, limit := c.paging()
	lo, hi := page(len(shown), start, limit)

	slots := make([]queueSlot, 0, hi-lo)
	for _, v := range shown[lo:hi] {
		slots = append(slots, c.queueSlot(f, v, index[v.Download.ID]))
	}

	var total, left int64
	downloading := false
	for _, v := range shown {
		total += v.Total
		left += max(v.Total-v.Done, 0)
		downloading = downloading || v.Phase == view.PhaseDownloading
	}
	state := "Idle"
	switch {
	case paused:
		state = "Paused"
	case downloading:
		state = "Downloading"
	}
	free, diskTotal := c.h.disk(c.h.Settings.Current().Paths.Usenet.Complete)
	c.json(queueBody{Queue: queue{
		Version: c.h.Version, Paused: paused, PausedAll: false, PauseInt: "0", Status: state,
		Speed: humanSpeed(bps), KBPerSec: fmt.Sprintf("%.2f", float64(bps)/1024),
		Size: humanBytes(total), SizeLeft: humanBytes(left), MB: formatMB(total), MBLeft: formatMB(left),
		TimeLeft:  formatTimeLeft(queueTimeLeft(left, bps)),
		NoOfSlots: len(shown), NoOfSlotsTotal: len(all), Start: lo, Limit: limit,
		Finish: 0, HaveWarnings: "0", CacheArt: "0", CacheSize: "0 B", Quota: "0 ", LeftQuota: "0 ",
		DiskSpace1: gigabytes(free), DiskSpace2: gigabytes(free),
		DiskSpaceTotal1: gigabytes(diskTotal), DiskSpaceTotal2: gigabytes(diskTotal),
		DiskSpace1Norm: normGigabytes(free), DiskSpace2Norm: normGigabytes(free),
		SpeedLimit: "", SpeedLimitAbs: "",
		Slots: slots,
	}})
}

func normGigabytes(n int64) string { return fmt.Sprintf("%.1f G", float64(n)/(1<<30)) }

// queueTimeLeft is the queue-wide time left: the bytes left of the whole
// queue at the global rate, in seconds; 0 when the rate is unknown. A slot's
// own time left is its View's ETA.
func queueTimeLeft(left, bps int64) int64 {
	if bps <= 0 || left <= 0 {
		return 0
	}
	return left / bps
}

func (c *call) queueSlot(f catFilter, v view.View, index int) queueSlot {
	d := v.Download
	left := max(v.Total-v.Done, 0)
	return queueSlot{
		Status: status(v), Index: index, NzoID: nzoID(d.ID), Filename: d.Name, Cat: f.catName(d),
		Priority: view.PriorityLabel(d.Priority), Script: "None",
		MB: formatMB(v.Total), MBLeft: formatMB(left), MBMissing: "0.0",
		Size: humanBytes(v.Total), SizeLeft: humanBytes(left),
		Percentage: percentage(v), TimeLeft: etaText(v),
		AvgAge: formatAge(c.h.Clock().Sub(d.AddedAt).Seconds()), Password: "", UnpackOpts: "3",
		Labels: []string{},
	}
}

// formatAge renders an age like SAB's avg_age: "5m", "3h", "2d".
func formatAge(sec float64) string {
	switch s := int64(max(sec, 0)); {
	case s >= 86400:
		return strconv.FormatInt(s/86400, 10) + "d"
	case s >= 3600:
		return strconv.FormatInt(s/3600, 10) + "h"
	default:
		return strconv.FormatInt(s/60, 10) + "m"
	}
}

// queueDelete removes the named Downloads from the queue. Unknown IDs, and
// Downloads that are no longer in the queue, are ignored like SAB does.
// Their working data (the incomplete files) always goes with them, whatever
// del_files says, as in SAB.
func (c *call) queueDelete() {
	removed := c.deleteDownloads(inQueue)
	if removed == nil {
		return
	}
	c.json(idsBody{Status: true, NzoIDs: removed})
}

// deleteDownloads deletes the Downloads named by value that are in the given
// list, and returns their nzo_ids (empty, never nil). It answers the request
// itself and returns nil on a failure.
func (c *call) deleteDownloads(from listing) []string {
	// A job leaving the queue takes its incomplete files with it. del_files
	// only decides whether the finished output of a history item goes.
	deleteData := from == inQueue || c.deleteData()
	removed := []string{}
	for _, id := range c.ids() {
		d, err := c.h.Downloads.Get(c.ctx(), id)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			c.appFail(err)
			return nil
		}
		if d.SourceKind != core.SourceNZB || !deletable(d, from) {
			continue
		}
		if err := c.h.Downloads.Delete(c.ctx(), id, deleteData); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			c.appFail(err)
			return nil
		}
		removed = append(removed, nzoID(id))
	}
	return removed
}

// deletable says whether a delete aimed at one list may remove d. A Download
// that is being post-processed is in neither list's reach: SAB refuses to
// delete a job while it is processing, and the data is in use.
func deletable(d core.Download, from listing) bool {
	if listingOf(d) != from {
		return false
	}
	return d.State != core.StateFetched && d.State != core.StatePostProcessing
}

// queuePriority serves name=priority&value=<nzo_id>&value2=<priority>.
func (c *call) queuePriority() {
	id, ok := parseNzoID(c.get("value"))
	if !ok {
		c.fail("expect one parameter")
		return
	}
	prio, paused := parsePriority(c.get("value2"))
	d, err := c.h.Downloads.Update(c.ctx(), id, app.Patch{Priority: &prio})
	if err != nil {
		c.queueActionFailed(err)
		return
	}
	if paused {
		if d, err = c.h.Downloads.Pause(c.ctx(), id); err != nil {
			c.queueActionFailed(err)
			return
		}
	}
	c.json(positionBody{Status: true, Position: c.positionOf(d)})
}

// queuePause serves name=pause|resume&value=<nzo_ids>.
func (c *call) queuePause(pause bool) {
	done := []string{}
	for _, id := range c.ids() {
		var err error
		if pause {
			_, err = c.h.Downloads.Pause(c.ctx(), id)
		} else {
			_, err = c.h.Downloads.Resume(c.ctx(), id)
		}
		if err != nil {
			if isIgnorable(err) {
				continue
			}
			c.appFail(err)
			return
		}
		done = append(done, nzoID(id))
	}
	c.json(idsBody{Status: true, NzoIDs: done})
}

// switchSteps caps how far name=switch moves a Download by single steps.
const switchSteps = 25

// queueSwitch serves name=switch&value=<nzo_id>&value2=<position>: move a
// Download to a queue position. The top and the bottom are one move; other
// positions are reached in single steps, up to switchSteps of them.
func (c *call) queueSwitch() {
	id, ok := parseNzoID(c.get("value"))
	target, err := strconv.Atoi(c.get("value2"))
	if !ok || err != nil {
		c.fail("expect two parameters")
		return
	}
	q, err := c.queueViews()
	if err != nil {
		c.appFail(err)
		return
	}
	cur := slices.IndexFunc(q, func(v view.View) bool { return v.Download.ID == id })
	if cur < 0 {
		c.json(positionBody{Status: true, Position: 0})
		return
	}
	target = min(max(target, 0), len(q)-1)

	var moves []app.Move
	switch {
	case target == cur:
	case target == 0:
		moves = []app.Move{app.MoveTop}
	case target == len(q)-1:
		moves = []app.Move{app.MoveBottom}
	case abs(target-cur) <= switchSteps:
		step := app.MoveDown
		if target < cur {
			step = app.MoveUp
		}
		for range abs(target - cur) {
			moves = append(moves, step)
		}
	default:
		c.fail("not supported: move further than " + strconv.Itoa(switchSteps) + " places")
		return
	}
	for _, m := range moves {
		if _, err := c.h.Downloads.Update(c.ctx(), id, app.Patch{Move: m}); err != nil {
			c.queueActionFailed(err)
			return
		}
	}
	c.json(positionBody{Status: true, Position: target})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// positionOf is the index of d in the queue order, or 0.
func (c *call) positionOf(d core.Download) int {
	q, err := c.queueViews()
	if err != nil {
		return 0
	}
	return max(slices.IndexFunc(q, func(v view.View) bool { return v.Download.ID == d.ID }), 0)
}

// isIgnorable says whether an action on one Download failed in a way SAB
// would not report: the Download is gone, or cannot take the action now.
func isIgnorable(err error) bool {
	return errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrConflict)
}

func (c *call) queueActionFailed(err error) {
	if isIgnorable(err) {
		c.json(okBody{Status: false})
		return
	}
	c.appFail(err)
}
