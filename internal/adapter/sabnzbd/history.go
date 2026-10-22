package sabnzbd

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// historyMode serves mode=history: the listing, and name=delete.
func (c *call) historyMode() {
	switch strings.ToLower(c.get("name")) {
	case "":
		c.historyList()
	case "delete":
		removed := c.deleteDownloads(inHistory)
		if removed == nil {
			return
		}
		c.json(okBody{Status: true})
	default:
		c.fail("not implemented")
	}
}

// historyList answers the history: NZB Downloads that are Fetched, in
// Post-processing, Ready or Failed, newest first. category (or cat) filters
// by Category name; start and limit page the slots.
func (c *call) historyList() {
	filter := c.get("category")
	if filter == "" {
		filter = c.get("cat")
	}
	v, err := c.load(historyStates, filter)
	if err != nil {
		c.appFail(err)
		return
	}
	slices.SortFunc(v.downloads, func(a, b core.Download) int {
		return cmp.Or(
			sortTime(b).Compare(sortTime(a)),
			strings.Compare(string(b.ID), string(a.ID)),
		)
	})
	start, limit := c.paging()
	lo, hi := page(len(v.downloads), start, limit)

	slots := make([]historySlot, 0, hi-lo)
	for _, d := range v.downloads[lo:hi] {
		slots = append(slots, c.historySlot(v, d))
	}
	inProcess := 0
	for _, d := range v.downloads {
		if d.State == core.StateFetched || d.State == core.StatePostProcessing {
			inProcess++
		}
	}
	c.json(historyBody{History: history{
		NoOfSlots: len(v.downloads), PPSlots: inProcess,
		TotalSize: "0 B", MonthSize: "0 B", WeekSize: "0 B", DaySize: "0 B",
		LastHistoryUpdate: c.h.Clock().Unix(),
		Slots:             slots,
	}})
}

func (c *call) historySlot(v view, d core.Download) historySlot {
	var stage core.Stage
	var haveStage bool
	if c.h.StageSource != nil && d.State == core.StatePostProcessing {
		stage, haveStage = c.h.StageSource(d.ID)
	}
	_, status := classify(d, stage, haveStage)

	s := historySlot{
		NzoID: nzoID(d.ID), Name: d.Name, NzbName: d.Name + ".nzb", Category: v.catName(d),
		Status: status, ActionLine: stageText(status),
		Bytes: d.TotalBytes, Size: humanBytes(d.TotalBytes), Downloaded: d.DoneBytes,
		Completed: unix(sortTime(d)), PP: "D", Script: "None",
		URL: d.Name + ".nzb",
	}
	if !d.FetchedAt.IsZero() {
		s.DownloadTime = max(int64(d.FetchedAt.Sub(d.AddedAt).Seconds()), 0)
		if !d.ReadyAt.IsZero() {
			s.PostprocTime = max(int64(d.ReadyAt.Sub(d.FetchedAt).Seconds()), 0)
		}
	}
	switch d.State {
	case core.StateReady:
		s.Storage, s.Path = d.OutputPath, d.OutputPath
	case core.StateFailed:
		s.FailMessage = failureText(d.FailureReason, d.FailureDetail)
	}
	return s
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// sortTime is when a history item "completed": it became Ready, or Failed,
// or else (still in Post-processing) when its data was Fetched.
func sortTime(d core.Download) time.Time {
	switch {
	case d.State == core.StateReady && !d.ReadyAt.IsZero():
		return d.ReadyAt
	case d.State != core.StateFailed && !d.FetchedAt.IsZero():
		return d.FetchedAt
	}
	return d.UpdatedAt
}
