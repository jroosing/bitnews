package qbit_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/qbit"
	"github.com/jroosing/bitnews/internal/core"
)

func hashes(vs ...torrent) string {
	hs := make([]string, len(vs))
	for i, v := range vs {
		hs[i] = v.InfoHash
	}
	return strings.Join(hs, "|")
}

func TestPauseAndResumeUnderBothNameGenerations(t *testing.T) {
	t.Parallel()
	for _, names := range [][2]string{{"pause", "resume"}, {"stop", "start"}} {
		s := newStack(t)
		d1, v1 := s.seed("one")
		d2, v2 := s.seed("two", inState(core.StateFetching))
		d3, _ := s.seed("three")
		sid := s.login()

		rec := s.post(sid, "/api/v2/torrents/"+names[0], url.Values{"hashes": {hashes(v1, v2)}})

		assert.Equal(t, http.StatusOK, rec.Code, names[0])
		assert.True(t, s.download(d1.ID).Paused, names[0])
		assert.True(t, s.download(d2.ID).Paused, names[0])
		assert.False(t, s.download(d3.ID).Paused, "not named")

		rec = s.post(sid, "/api/v2/torrents/"+names[1], url.Values{"hashes": {hashes(v1, v2)}})

		assert.Equal(t, http.StatusOK, rec.Code, names[1])
		assert.False(t, s.download(d1.ID).Paused, names[1])
		assert.False(t, s.download(d2.ID).Paused, names[1])
	}
}

func TestPauseAll(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d1, _ := s.seed("one")
	d2, _ := s.seed("two")
	sid := s.login()

	require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/pause", url.Values{"hashes": {"all"}}).Code)

	assert.True(t, s.download(d1.ID).Paused)
	assert.True(t, s.download(d2.ID).Paused)
}

func TestPausingWhatCannotBePausedOrIsUnknownIsNotAnError(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("done", inState(core.StateFetched))
	sid := s.login()

	rec := s.post(sid, "/api/v2/torrents/pause", url.Values{"hashes": {v.InfoHash + "|" + strings.Repeat("0", 40) + "|junk"}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, s.download(d.ID).Paused, "a Fetched torrent in Post-processing has nothing to pause")
}

func TestPausingASeedingTorrentStopsItAndResumeStartsItAgain(t *testing.T) {
	t.Parallel()
	for _, names := range [][2]string{{"pause", "resume"}, {"stop", "start"}} {
		s := newStack(t)
		d, v := s.seed("seeding", ready("/d/x"))
		sid := s.login()

		require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/"+names[0], url.Values{"hashes": {v.InfoHash}}).Code)
		assert.True(t, s.download(d.ID).Paused, names[0])
		assert.Equal(t, core.StateReady, s.download(d.ID).State, "pausing the seed does not leave Ready")
		assert.Equal(t, "pausedUP", s.info(sid, "hashes="+v.InfoHash)[0].State, names[0])

		require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/"+names[1], url.Values{"hashes": {v.InfoHash}}).Code)
		assert.False(t, s.download(d.ID).Paused, names[1])
		assert.NotEqual(t, "pausedUP", s.info(sid, "hashes="+v.InfoHash)[0].State, names[1])
	}
}

func TestDeleteWithoutFilesKeepsTheDataButDropsTheDownload(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d1, v1 := s.seed("keep files", ready("/d/a"))
	d2, v2 := s.seed("remove files", ready("/d/b"))
	d3, _ := s.seed("other", ready("/d/c"))
	sid := s.login()

	rec1 := s.post(sid, "/api/v2/torrents/delete", url.Values{"hashes": {v1.InfoHash}, "deleteFiles": {"false"}})
	rec2 := s.post(sid, "/api/v2/torrents/delete", url.Values{"hashes": {v2.InfoHash}, "deleteFiles": {"true"}})

	assert.Equal(t, http.StatusOK, rec1.Code)
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.False(t, s.exists(d1.ID))
	assert.False(t, s.exists(d2.ID))
	assert.True(t, s.exists(d3.ID))
	assert.Equal(t, []core.ID{d2.ID}, s.remover.list(), "only deleteFiles=true removes data")
}

func TestDeleteSeveralAndUnknownAtOnce(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d1, v1 := s.seed("one", ready("/d/a"))
	d2, v2 := s.seed("two")
	sid := s.login()

	rec := s.post(sid, "/api/v2/torrents/delete", url.Values{
		"hashes": {v1.InfoHash + "|" + strings.Repeat("e", 40) + "|" + v2.InfoHash}, "deleteFiles": {"false"},
	})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.False(t, s.exists(d1.ID))
	assert.False(t, s.exists(d2.ID))
	assert.Empty(t, s.info(sid, ""))
}

func TestDeleteAllAndTheMissingParameter(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	s.seed("one", ready("/d/a"))
	s.seed("two", ready("/d/b"))
	sid := s.login()

	require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/delete", url.Values{"deleteFiles": {"true"}}).Code)
	assert.Len(t, s.info(sid, ""), 2, "no hashes: nothing is deleted")

	require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/delete", url.Values{"hashes": {"all"}}).Code)
	assert.Empty(t, s.info(sid, ""))
}

func TestADownloadBeingPostProcessedIsNotDeleted(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d1, v1 := s.seed("fetched", inState(core.StateFetched))
	d2, v2 := s.seed("unpacking", inState(core.StatePostProcessing))
	sid := s.login()

	rec := s.post(sid, "/api/v2/torrents/delete", url.Values{"hashes": {hashes(v1, v2)}, "deleteFiles": {"true"}})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, s.exists(d1.ID), "its files are in use")
	assert.True(t, s.exists(d2.ID))
	assert.Empty(t, s.remover.list())
}

func TestDeleteLeavesNZBDownloadsAlone(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("nzb with a torrent view?", func(d *core.Download, _ *torrent) { d.SourceKind = core.SourceNZB })
	sid := s.login()

	s.post(sid, "/api/v2/torrents/delete", url.Values{"hashes": {v.InfoHash}, "deleteFiles": {"true"}})

	assert.True(t, s.exists(d.ID))
}

func TestSetCategory(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	tv := s.category("tv")
	d1, v1 := s.seed("one")
	d2, v2 := s.seed("two", inCategory(tv.ID))
	sid := s.login()

	rec := s.post(sid, "/api/v2/torrents/setCategory", url.Values{"hashes": {v1.InfoHash}, "category": {"TV"}})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, tv.ID, s.download(d1.ID).CategoryID, "ignoring case")

	rec = s.post(sid, "/api/v2/torrents/setCategory", url.Values{"hashes": {v2.InfoHash}, "category": {""}})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, core.ID(""), s.download(d2.ID).CategoryID, "an empty name removes the Category")
}

func TestSetCategoryToAnUnknownCategoryIs409(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("one")
	sid := s.login()

	rec := s.post(sid, "/api/v2/torrents/setCategory", url.Values{"hashes": {v.InfoHash}, "category": {"nothing"}})

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, core.ID(""), s.download(d.ID).CategoryID)
}

func TestCreateAndListCategories(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	assert.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/createCategory",
		url.Values{"category": {"radarr"}, "savePath": {"/elsewhere"}}).Code)
	assert.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/createCategory",
		url.Values{"category": {"Radarr"}}).Code, "creating one that exists (any case) is not an error")
	for _, bad := range []string{"has space", "a/b", "../x", strings.Repeat("x", 65)} {
		assert.Equal(t, http.StatusConflict, s.post(sid, "/api/v2/torrents/createCategory", url.Values{"category": {bad}}).Code, bad)
	}
	assert.Equal(t, http.StatusBadRequest, s.post(sid, "/api/v2/torrents/createCategory", url.Values{"category": {""}}).Code)

	var cats map[string]struct {
		Name     string `json:"name"`
		SavePath string `json:"savePath"`
	}
	decode(t, s.get(sid, "/api/v2/torrents/categories"), &cats)

	require.Len(t, cats, 1)
	assert.Equal(t, "radarr", cats["radarr"].Name)
	assert.Equal(t, s.torrents+"/radarr", cats["radarr"].SavePath, "below the torrent complete root; the given savePath is ignored")
}

func TestCategoriesOfNoneIsAnEmptyObject(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	sid := s.login()

	assert.JSONEq(t, `{}`, s.get(sid, "/api/v2/torrents/categories").Body.String())
}

func TestACategoryWithADirOverrideReportsIt(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	_, err := s.categories.Create(context.Background(), "movies", "/data/media/movies")
	require.NoError(t, err)
	sid := s.login()

	var cats map[string]struct {
		SavePath string `json:"savePath"`
	}
	decode(t, s.get(sid, "/api/v2/torrents/categories"), &cats)

	assert.Equal(t, "/data/media/movies", cats["movies"].SavePath)
}

func queueOrder(s *stack, sid *http.Cookie) []string { return names(s.info(sid, "sort=priority")) }

func TestQueuePositionCalls(t *testing.T) {
	t.Parallel()
	setup := func() (*stack, *http.Cookie, []torrent) {
		s := newStack(t)
		var vs []torrent
		for _, n := range []string{"a", "b", "c", "d"} {
			_, v := s.seed(n)
			vs = append(vs, v)
		}
		return s, s.login(), vs
	}
	call := func(s *stack, sid *http.Cookie, ep string, vs ...torrent) {
		t.Helper()
		require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/"+ep, url.Values{"hashes": {hashes(vs...)}}).Code)
	}

	t.Run("topPrio", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "topPrio", vs[2])
		assert.Equal(t, []string{"c", "a", "b", "d"}, queueOrder(s, sid))
	})
	t.Run("topPrio of several keeps their order", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "topPrio", vs[3], vs[2])
		assert.Equal(t, []string{"c", "d", "a", "b"}, queueOrder(s, sid))
	})
	t.Run("bottomPrio", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "bottomPrio", vs[0])
		assert.Equal(t, []string{"b", "c", "d", "a"}, queueOrder(s, sid))
	})
	t.Run("bottomPrio of several keeps their order", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "bottomPrio", vs[0], vs[1])
		assert.Equal(t, []string{"c", "d", "a", "b"}, queueOrder(s, sid))
	})
	t.Run("increasePrio", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "increasePrio", vs[2])
		assert.Equal(t, []string{"a", "c", "b", "d"}, queueOrder(s, sid))
	})
	t.Run("decreasePrio", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "decreasePrio", vs[1])
		assert.Equal(t, []string{"a", "c", "b", "d"}, queueOrder(s, sid))
	})
	t.Run("increasePrio of two adjacent ones moves them together", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "increasePrio", vs[2], vs[3])
		assert.Equal(t, []string{"a", "c", "d", "b"}, queueOrder(s, sid))
	})
	t.Run("decreasePrio of two adjacent ones moves them together", func(t *testing.T) {
		s, sid, vs := setup()
		call(s, sid, "decreasePrio", vs[0], vs[1])
		assert.Equal(t, []string{"c", "a", "b", "d"}, queueOrder(s, sid))
	})
	t.Run("a Download that left the queue is skipped", func(t *testing.T) {
		s, sid, vs := setup()
		_, done := s.seed("done", ready("/d/done"))
		call(s, sid, "topPrio", done, vs[1])
		assert.Equal(t, []string{"b", "a", "c", "d"}, names(s.info(sid, "sort=priority&filter=downloading")))
	})
}

func TestSetForceStart(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("one")
	sid := s.login()

	require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/setForceStart", url.Values{"hashes": {v.InfoHash}, "value": {"true"}}).Code)
	assert.Equal(t, core.DownloadPriorityForce, s.download(d.ID).Priority)
	got := s.info(sid, "")
	assert.True(t, got[0].ForceStart)
	assert.Equal(t, "forcedDL", got[0].State)

	require.Equal(t, http.StatusOK, s.post(sid, "/api/v2/torrents/setForceStart", url.Values{"hashes": {v.InfoHash}, "value": {"false"}}).Code)
	assert.Equal(t, core.DownloadPriorityNormal, s.download(d.ID).Priority)
}

func TestSetShareLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		ratio    string
		minutes  string
		want     limitCall
		globalOn *qbit.GlobalShare
	}{
		{"both set", "2.5", "120", limitCall{Ratio: 2.5, Seconds: 7200}, nil},
		{"both global", "-2", "-2", limitCall{UseGlobal: true}, nil},
		{"both unlimited", "-1", "-1", limitCall{Ratio: -1, Seconds: -1}, nil},
		{"only ratio", "1", "-1", limitCall{Ratio: 1, Seconds: -1}, nil},
		{"only time", "-1", "30", limitCall{Ratio: -1, Seconds: 1800}, nil},
		{"ratio global with the global limit off", "-2", "30", limitCall{Ratio: -1, Seconds: 1800}, nil},
		{"ratio global with a global ratio", "-2", "30", limitCall{Ratio: 3, Seconds: 1800},
			&qbit.GlobalShare{RatioEnabled: true, Ratio: 3}},
		{"time global with a global time", "1.5", "-2", limitCall{Ratio: 1.5, Seconds: 600},
			&qbit.GlobalShare{SeedingTimeEnabled: true, SeedingMinutes: 10}},
		{"zero is a limit of zero", "0", "0", limitCall{Ratio: 0, Seconds: 0}, nil},
		{"other negatives are unlimited", "-7", "-5", limitCall{Ratio: -1, Seconds: -1}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStack(t, withDeps(func(d *qbit.Deps) {
				if tc.globalOn != nil {
					g := *tc.globalOn
					d.GlobalShare = func() qbit.GlobalShare { return g }
				}
			}))
			d, v := s.seed("one", ready("/d/one"))
			sid := s.login()

			rec := s.post(sid, "/api/v2/torrents/setShareLimits", url.Values{
				"hashes": {v.InfoHash}, "ratioLimit": {tc.ratio}, "seedingTimeLimit": {tc.minutes}, "inactiveSeedingTimeLimit": {"-2"},
			})

			assert.Equal(t, http.StatusOK, rec.Code)
			got, ok := s.engine.limit(d.ID)
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSetShareLimitsRejectsNonsense(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("one", ready("/d/one"))
	sid := s.login()

	for _, args := range [][2]string{{"NaN", "1"}, {"Inf", "1"}, {"abc", "1"}, {"1", "abc"}, {"", "1"}, {"1", ""}, {"1", "1.5"}} {
		rec := s.post(sid, "/api/v2/torrents/setShareLimits", url.Values{"hashes": {v.InfoHash}, "ratioLimit": {args[0]}, "seedingTimeLimit": {args[1]}})

		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v", args)
	}
	_, set := s.engine.limit(d.ID)
	assert.False(t, set)
}

func TestAHugeSeedingTimeIsClampedNotOverflowed(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("one", ready("/d/one"))
	sid := s.login()

	s.post(sid, "/api/v2/torrents/setShareLimits", url.Values{"hashes": {v.InfoHash}, "ratioLimit": {"1"}, "seedingTimeLimit": {"9223372036854775807"}})

	got, _ := s.engine.limit(d.ID)
	assert.Positive(t, got.Seconds)
}

func TestMutatingCallsNeedPOST(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, v := s.seed("one", ready("/d/one"))
	sid := s.login()

	for _, ep := range []string{"pause", "resume", "stop", "start", "delete", "setCategory", "setShareLimits", "topPrio", "bottomPrio",
		"increasePrio", "decreasePrio", "setForceStart", "createCategory", "add"} {
		rec := s.get(sid, "/api/v2/torrents/"+ep+"?hashes="+v.InfoHash+"&deleteFiles=true")

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, ep)
		assert.Equal(t, "POST", rec.Header().Get("Allow"), ep)
	}
	assert.True(t, s.exists(d.ID))
}

func TestACallWithAPOSTBodyOfTheWrongTypeChangesNothing(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	d, _ := s.seed("one", ready("/d/one"))
	sid := s.login()
	r := formRequest("POST", "/api/v2/torrents/delete", nil)
	r.Body = http.NoBody
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(sid)

	rec := s.serve(r)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, s.exists(d.ID), "no hashes were found in the JSON body")
}
