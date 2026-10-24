package sabnzbd

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
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
	f, err := c.readCatFilter(filter)
	if err != nil {
		c.appFail(err)
		return
	}
	ds, err := c.historyDownloads(f)
	if err != nil {
		c.appFail(err)
		return
	}
	slices.SortFunc(ds, func(a, b core.Download) int {
		return cmp.Or(
			sortTime(b).Compare(sortTime(a)),
			strings.Compare(string(b.ID), string(a.ID)),
		)
	})
	start, limit := c.paging()
	lo, hi := page(len(ds), start, limit)

	slots := make([]historySlot, 0, hi-lo)
	for _, d := range ds[lo:hi] {
		slots = append(slots, c.historySlot(f, c.h.View.Of(c.ctx(), d)))
	}
	inProcess := 0
	for _, d := range ds {
		if d.State == core.StateFetched || d.State == core.StatePostProcessing {
			inProcess++
		}
	}
	c.json(historyBody{History: history{
		NoOfSlots: len(ds), PPSlots: inProcess,
		TotalSize: "0 B", MonthSize: "0 B", WeekSize: "0 B", DaySize: "0 B",
		LastHistoryUpdate: c.h.Clock().Unix(),
		Slots:             slots,
	}})
}

func (c *call) historySlot(f catFilter, v view.View) historySlot {
	d := v.Download
	st := status(v)
	s := historySlot{
		NzoID: nzoID(d.ID), Name: d.Name, NzbName: d.Name + ".nzb", Category: f.catName(d),
		Status: st, ActionLine: stageText(st),
		Bytes: v.Total, Size: humanBytes(v.Total), Downloaded: v.Done,
		Completed: unix(sortTime(d)), PP: "D", Script: "None",
		URL: d.Name + ".nzb",
	}
	if !d.FetchedAt.IsZero() {
		s.DownloadTime = max(int64(d.FetchedAt.Sub(d.AddedAt).Seconds()), 0)
		if !d.ReadyAt.IsZero() {
			s.PostprocTime = max(int64(d.ReadyAt.Sub(d.FetchedAt).Seconds()), 0)
		}
	}
	if d.State == core.StateReady {
		s.Storage, s.Path = d.OutputPath, d.OutputPath
	}
	if v.Failure != nil {
		s.FailMessage = failureText(*v.Failure)
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
