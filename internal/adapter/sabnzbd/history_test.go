package sabnzbd_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

type historyResp struct {
	History struct {
		NoOfSlots int `json:"noofslots"`
		PPSlots   int `json:"ppslots"`
		Slots     []struct {
			NzoID        string `json:"nzo_id"`
			Name         string `json:"name"`
			NzbName      string `json:"nzb_name"`
			Category     string `json:"category"`
			Status       string `json:"status"`
			FailMessage  string `json:"fail_message"`
			ActionLine   string `json:"action_line"`
			Storage      string `json:"storage"`
			Bytes        int64  `json:"bytes"`
			Size         string `json:"size"`
			Completed    int64  `json:"completed"`
			DownloadTime int64  `json:"download_time"`
			Password     string `json:"password"`
		} `json:"slots"`
	} `json:"history"`
}

func (s *stack) history(query string) historyResp {
	s.t.Helper()
	var h historyResp
	body(s.t, s.call("mode=history&"+query), &h)
	return h
}

func TestAnEmptyHistoryHasEmptySlotsNotNull(t *testing.T) {
	s := newStack(t)

	h := generic(t, s.call("mode=history"))["history"].(map[string]any)

	assert.Equal(t, []any{}, h["slots"])
	assert.EqualValues(t, 0, h["noofslots"])
}

func TestHistoryReadySlotHasStorageBytesAndTimes(t *testing.T) {
	s := newStack(t)
	tv := s.category("tv")
	d := s.seed("Show.S01E01", ready("/data/usenet/complete/tv/Show.S01E01"), sized(5<<20, 5<<20), inCategory(tv.ID))

	h := s.history("")

	require.Len(t, h.History.Slots, 1)
	slot := h.History.Slots[0]
	assert.Equal(t, nzo(d), slot.NzoID)
	assert.Equal(t, "Show.S01E01", slot.Name)
	assert.Equal(t, "Show.S01E01.nzb", slot.NzbName)
	assert.Equal(t, "tv", slot.Category)
	assert.Equal(t, "Completed", slot.Status)
	assert.Equal(t, "/data/usenet/complete/tv/Show.S01E01", slot.Storage)
	assert.EqualValues(t, 5<<20, slot.Bytes)
	assert.Equal(t, "5.0 MB", slot.Size)
	assert.Equal(t, d.AddedAt.Add(3*time.Minute).Unix(), slot.Completed)
	assert.EqualValues(t, 60, slot.DownloadTime)
	assert.Empty(t, slot.FailMessage)
	assert.Empty(t, slot.Password)
}

func TestHistoryFailedSlotHasARedactedHumanFailMessage(t *testing.T) {
	s := newStack(t)
	s.seed("Bad.Release", failed(core.FailureMissingArticles, "412 articles missing, fetched http://idx.example/x?apikey=SECRET9"))

	h := s.history("")

	slot := h.History.Slots[0]
	assert.Equal(t, "Failed", slot.Status)
	assert.Contains(t, slot.FailMessage, "Aborted, cannot be completed")
	assert.Contains(t, slot.FailMessage, "412 articles missing")
	assert.NotContains(t, slot.FailMessage, "SECRET9")
	assert.Empty(t, slot.Storage, "a failed download has nothing to import")
}

func TestHistoryListsPostProcessingWithoutStorage(t *testing.T) {
	s := newStack(t)
	pp := s.seed("pp", inState(core.StatePostProcessing))
	s.seed("fetched", inState(core.StateFetched))
	s.setStage(pp.ID, core.StageUnpacking)
	h := s.history("")

	assert.Equal(t, 2, h.History.NoOfSlots)
	assert.Equal(t, 2, h.History.PPSlots)
	for _, slot := range h.History.Slots {
		assert.Empty(t, slot.Storage, slot.Name)
		assert.NotEqual(t, "Completed", slot.Status)
	}
}

func TestHistoryIsNewestFirstAndPaged(t *testing.T) {
	s := newStack(t)
	var names []string
	for i := range 6 {
		s.seed(fmt.Sprintf("d%d", i), ready("/x"))
		names = append(names, fmt.Sprintf("d%d", 5-i))
	}
	// A failed one completed after all the Ready ones.
	s.seed("failed-last", failed(core.FailureInternal, ""), func(d *core.Download) { d.UpdatedAt = epoch.Add(24 * time.Hour) })
	names = append([]string{"failed-last"}, names...)

	all := s.history("")
	page := s.history("start=2&limit=3")

	var got []string
	for _, slot := range all.History.Slots {
		got = append(got, slot.Name)
	}
	assert.Equal(t, names, got)
	require.Len(t, page.History.Slots, 3)
	assert.Equal(t, names[2:5], []string{page.History.Slots[0].Name, page.History.Slots[1].Name, page.History.Slots[2].Name})
	assert.Equal(t, 7, page.History.NoOfSlots)
}

func TestHistoryCategoryFilter(t *testing.T) {
	s := newStack(t)
	tv, movies := s.category("tv"), s.category("movies")
	s.seed("a", ready("/x"), inCategory(tv.ID))
	s.seed("b", ready("/x"), inCategory(movies.ID))
	s.seed("c", failed(core.FailureInternal, ""), inCategory(tv.ID))
	s.seed("d", ready("/x"))

	byCategory := s.history("category=tv")
	byCat := s.history("cat=movies")
	none := s.history("category=nope")
	star := s.history("category=*")

	assert.Equal(t, 2, byCategory.History.NoOfSlots)
	for _, slot := range byCategory.History.Slots {
		assert.Equal(t, "tv", slot.Category)
	}
	require.Len(t, byCat.History.Slots, 1)
	assert.Equal(t, "b", byCat.History.Slots[0].Name)
	assert.Empty(t, none.History.Slots)
	require.Len(t, star.History.Slots, 1)
	assert.Equal(t, "*", star.History.Slots[0].Category)
}

func TestHistoryDoesNotListTheQueue(t *testing.T) {
	s := newStack(t)
	s.seed("queued")
	s.seed("fetching", inState(core.StateFetching))

	assert.Empty(t, s.history("").History.Slots)
}

func TestQueueDeleteAlwaysRemovesTheWorkingDataWhateverDelFilesSays(t *testing.T) {
	s := newStack(t)
	a, b, c, keep := s.seed("a"), s.seed("b"), s.seed("c"), s.seed("keep")

	withFiles := s.call("mode=queue&name=delete&value=" + nzo(a) + "&del_files=1")
	zero := s.call("mode=queue&name=delete&value=" + nzo(b) + "&del_files=0")
	absent := s.call("mode=queue&name=delete&value=" + nzo(c))

	assert.Equal(t, []string{nzo(a)}, nzoIDs(t, withFiles))
	assert.Equal(t, []string{nzo(b)}, nzoIDs(t, zero))
	assert.Equal(t, []string{nzo(c)}, nzoIDs(t, absent))
	assert.ElementsMatch(t, []core.ID{a.ID, b.ID, c.ID}, s.remover.list(),
		"SAB removes a queued job's incomplete files with it")
	left := s.all()
	require.Len(t, left, 1)
	assert.Equal(t, keep.ID, left[0].ID)
}

func TestHistoryDeleteWithoutDelFilesKeepsTheOutput(t *testing.T) {
	s := newStack(t)
	kept := s.seed("kept", ready("/data/kept"))
	zero := s.seed("zero", ready("/data/zero"))
	deleted := s.seed("deleted", ready("/data/deleted"))

	s.call("mode=history&name=delete&value=" + nzo(kept))
	s.call("mode=history&name=delete&del_files=0&value=" + nzo(zero))
	s.call("mode=history&name=delete&del_files=1&value=" + nzo(deleted))

	assert.Equal(t, []core.ID{deleted.ID}, s.remover.list(), "only del_files=1 deletes finished output")
	assert.Empty(t, s.all())
}

func TestDeleteAcceptsCommaSeparatedIDsAndIgnoresUnknownOnes(t *testing.T) {
	s := newStack(t)
	a, b := s.seed("a"), s.seed("b")
	keep := s.seed("keep")

	rec := s.call("mode=queue&name=delete&del_files=1&value=" + joinNzo(a) + ",SABnzbd_nzo_" + string(core.NewID()) + ",garbage,," + nzo(b))

	assert.JSONEq(t, fmt.Sprintf(`{"status":true,"nzo_ids":[%q,%q]}`, nzo(a), nzo(b)), rec.Body.String())
	left := s.all()
	require.Len(t, left, 1)
	assert.Equal(t, keep.ID, left[0].ID)
	assert.ElementsMatch(t, []core.ID{a.ID, b.ID}, s.remover.list())
}

func TestDeleteOfOnlyUnknownIDsStillSucceeds(t *testing.T) {
	s := newStack(t)

	for _, q := range []string{
		"mode=queue&name=delete&value=SABnzbd_nzo_unknown",
		"mode=queue&name=delete&value=",
		"mode=queue&name=delete",
	} {
		rec := s.call(q)

		assert.JSONEq(t, `{"status":true,"nzo_ids":[]}`, rec.Body.String(), q)
	}
	assert.JSONEq(t, `{"status":true}`, s.call("mode=history&name=delete&value=SABnzbd_nzo_unknown").Body.String())
}

func TestQueueDeleteIgnoresHistoryDownloadsAndViceVersa(t *testing.T) {
	s := newStack(t)
	queued := s.seed("queued")
	done := s.seed("done", ready("/x"))

	s.call("mode=queue&name=delete&value=" + nzo(done))
	s.call("mode=history&name=delete&value=" + nzo(queued))

	assert.Len(t, s.all(), 2, "each delete only reaches its own list")
}

func TestHistoryDeleteRemovesReadyAndFailedDownloads(t *testing.T) {
	s := newStack(t)
	done := s.seed("done", ready("/data/x"))
	bad := s.seed("bad", failed(core.FailureInternal, ""))
	keep := s.seed("keep", ready("/data/keep"))

	noFiles := s.call("mode=history&name=delete&value=" + nzo(bad))
	withFiles := s.call("mode=history&name=delete&del_files=1&value=" + nzo(done))

	assert.JSONEq(t, `{"status":true}`, noFiles.Body.String())
	assert.JSONEq(t, `{"status":true}`, withFiles.Body.String())
	assert.Equal(t, []core.ID{done.ID}, s.remover.list())
	left := s.all()
	require.Len(t, left, 1)
	assert.Equal(t, keep.ID, left[0].ID)
}

func TestHistoryDeleteLeavesDownloadsThatAreBeingPostProcessed(t *testing.T) {
	s := newStack(t)
	fetched := s.seed("fetched", inState(core.StateFetched))
	pp := s.seed("pp", inState(core.StatePostProcessing))

	rec := s.call("mode=history&name=delete&del_files=1&value=" + joinNzo(fetched, pp))

	assert.JSONEq(t, `{"status":true}`, rec.Body.String())
	assert.Len(t, s.all(), 2, "their files are in use")
	assert.Empty(t, s.remover.list())
}

func TestDeleteNeverReachesTorrentDownloads(t *testing.T) {
	s := newStack(t)
	torrent := s.seed("t", func(d *core.Download) { d.SourceKind = core.SourceTorrent })

	s.call("mode=queue&name=delete&value=" + nzo(torrent))

	_, err := s.downloads.Get(context.Background(), torrent.ID)
	assert.NoError(t, err)
}

func TestHistoryUnknownSubcommandIsNotImplemented(t *testing.T) {
	s := newStack(t)

	assert.Equal(t, "not implemented", errorOf(t, s.call("mode=history&name=mark_as_completed&value=x")))
}
