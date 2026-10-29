package api_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

type downloadJSON struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	SourceKind    string     `json:"source_kind"`
	CategoryID    *string    `json:"category_id"`
	State         string     `json:"state"`
	Priority      string     `json:"priority"`
	QueuePosition int        `json:"queue_position"`
	Paused        bool       `json:"paused"`
	FailureReason string     `json:"failure_reason"`
	FailureDetail string     `json:"failure_detail"`
	TotalBytes    int64      `json:"total_bytes"`
	DoneBytes     int64      `json:"done_bytes"`
	AddedAt       time.Time  `json:"added_at"`
	FetchedAt     *time.Time `json:"fetched_at"`
	ReadyAt       *time.Time `json:"ready_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	OutputPath    string     `json:"output_path"`
}

type listJSON struct {
	Items []downloadJSON `json:"items"`
	Next  *string        `json:"next"`
}

func idsOf(l listJSON) []string {
	out := []string{}
	for _, d := range l.Items {
		out = append(out, d.ID)
	}
	return out
}

// subscribe returns a function that drains the events published since.
func (e *env) subscribe() func() []core.Event {
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	sub := e.bus.Subscribe(ctx)
	return func() []core.Event {
		var out []core.Event
		for {
			select {
			case ev := <-sub.Events():
				out = append(out, ev)
			default:
				return out
			}
		}
	}
}

func TestListDownloadsNeedsTheKeyAndNothingElse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sess := e.login()

	anonymous := e.do("GET", "/api/v1/downloads", "")
	viaSession := e.do("GET", "/api/v1/downloads", "", sess.mods()...)
	viaKey := e.do("GET", "/api/v1/downloads", "", withBearer(e.apiKey()))
	viaBadKey := e.do("GET", "/api/v1/downloads", "", withBearer("bn_nope"))

	assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
	assert.Equal(t, http.StatusUnauthorized, viaSession.Code, "a UI session opens nothing in the API")
	assert.Equal(t, http.StatusOK, viaKey.Code)
	assert.Equal(t, http.StatusUnauthorized, viaBadKey.Code)
}

func TestListDownloadsReturnsDownloadJSONAndNullNextOnTheLastPage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	d := e.addDownload("movie", 0)

	rec := e.do("GET", "/api/v1/downloads", "", withBearer(key))

	require.Equal(t, http.StatusOK, rec.Code)
	var list listJSON
	decode(t, rec, &list)
	require.Len(t, list.Items, 1)
	assert.Nil(t, list.Next)
	got := list.Items[0]
	assert.Equal(t, string(d.ID), got.ID)
	assert.Equal(t, "movie", got.Name)
	assert.Equal(t, "nzb", got.SourceKind)
	assert.Equal(t, "queued", got.State)
	assert.Equal(t, "normal", got.Priority)
	assert.Nil(t, got.CategoryID)
	assert.Nil(t, got.FetchedAt)
	assert.Nil(t, got.ReadyAt)
	assert.True(t, d.AddedAt.Equal(got.AddedAt))
}

func TestListDownloadsOfNothingIsAnEmptyArray(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("GET", "/api/v1/downloads", "", withBearer(e.apiKey()))

	assert.JSONEq(t, `{"items":[],"next":null}`, rec.Body.String())
}

func TestListDownloadsFiltersAndPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, e.cats.Create(context.Background(), cat))
	a := e.addDownload("a", 0)
	b := e.addDownload("b", time.Second)
	c := e.addDownload("c", 2*time.Second)
	b.CategoryID = cat.ID
	require.NoError(t, e.downloads.Update(context.Background(), b))
	_, err := e.downloads.UpdateState(context.Background(), c.ID, core.StateChange{To: core.StateFetching, At: epoch})
	require.NoError(t, err)
	get := func(query string) listJSON {
		rec := e.do("GET", "/api/v1/downloads?"+query, "", withBearer(key))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var l listJSON
		decode(t, rec, &l)
		return l
	}

	first := get("limit=2")
	require.NotNil(t, first.Next)
	second := get("limit=2&after=" + url.QueryEscape(*first.Next))

	assert.Equal(t, []string{string(b.ID)}, idsOf(get("category_id="+string(cat.ID))))
	assert.Equal(t, []string{string(c.ID)}, idsOf(get("state=fetching")))
	assert.Empty(t, idsOf(get("kind=torrent")))
	assert.Equal(t, []string{string(a.ID), string(b.ID)}, idsOf(first))
	assert.Equal(t, []string{string(c.ID)}, idsOf(second))
	assert.Nil(t, second.Next)
}

func TestListDownloadsRejectsBadQueryParameters(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	for query, code := range map[string]string{
		"state=bogus":  "invalid_state",
		"kind=bogus":   "invalid_kind",
		"limit=abc":    "invalid_limit",
		"limit=-3":     "invalid_limit",
		"after=!!!":    "invalid_cursor",
		"unknown=what": "unknown_parameter",
	} {
		t.Run(query, func(t *testing.T) {
			rec := e.do("GET", "/api/v1/downloads?"+query, "", withBearer(key))

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, code, errorOf(t, rec))
		})
	}
}

func TestGetDownloadReturnsItOr404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	d := e.addDownload("movie", 0)

	found := e.do("GET", "/api/v1/downloads/"+string(d.ID), "", withBearer(key))
	missing := e.do("GET", "/api/v1/downloads/"+string(core.NewID()), "", withBearer(key))

	require.Equal(t, http.StatusOK, found.Code)
	var got downloadJSON
	decode(t, found, &got)
	assert.Equal(t, string(d.ID), got.ID)
	require.Equal(t, http.StatusNotFound, missing.Code)
	assert.Equal(t, "not_found", errorOf(t, missing))
}

func TestPauseAndResumeToggleThePausedFlagAndPublishEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	d := e.addDownload("movie", 0)
	events := e.subscribe()

	paused := e.do("POST", "/api/v1/downloads/"+string(d.ID)+"/pause", "", withBearer(key))
	resumed := e.do("POST", "/api/v1/downloads/"+string(d.ID)+"/resume", "", withBearer(key))

	var p, r downloadJSON
	require.Equal(t, http.StatusOK, paused.Code, paused.Body.String())
	decode(t, paused, &p)
	require.Equal(t, http.StatusOK, resumed.Code)
	decode(t, resumed, &r)
	assert.True(t, p.Paused)
	assert.False(t, r.Paused)
	assert.Len(t, events(), 2)
}

func TestPauseOfAFetchedDownloadIs409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := e.addDownload("movie", 0)
	for _, to := range []core.State{core.StateFetching, core.StateFetched} {
		_, err := e.downloads.UpdateState(context.Background(), d.ID, core.StateChange{To: to, At: epoch})
		require.NoError(t, err)
	}

	rec := e.do("POST", "/api/v1/downloads/"+string(d.ID)+"/pause", "", withBearer(e.apiKey()))

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "not_pausable", errorOf(t, rec))
}

func TestWriteRoutesRefuseUnauthenticatedCallersAndAWrongKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := e.addDownload("movie", 0)
	id := string(d.ID)
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/downloads/" + id + "/pause", ""},
		{"POST", "/api/v1/downloads/" + id + "/resume", ""},
		{"PATCH", "/api/v1/downloads/" + id, `{"priority":"high"}`},
		{"DELETE", "/api/v1/downloads/" + id, ""},
		{"POST", "/api/v1/categories", `{"name":"tv"}`},
		{"PATCH", "/api/v1/categories/x", `{"name":"tv"}`},
		{"DELETE", "/api/v1/categories/x", ""},
		{"PUT", "/api/v1/settings", `{}`},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			wrongKey := e.do(rt.method, rt.path, rt.body, withBearer("bitnews_wrong"))
			anonymous := e.do(rt.method, rt.path, rt.body)

			assert.Equal(t, http.StatusUnauthorized, wrongKey.Code)
			assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
		})
	}
	stored, err := e.downloads.Get(context.Background(), d.ID)
	require.NoError(t, err)
	assert.Equal(t, d, stored)
}

func TestWriteRoutesNeedTheKeyAndNoCSRFToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d := e.addDownload("movie", 0)
	sess := e.login()
	path := "/api/v1/downloads/" + string(d.ID) + "/pause"

	viaSession := e.do("POST", path, "", sess.mods()...)
	viaKey := e.do("POST", path, "", withBearer(e.apiKey()))

	assert.Equal(t, http.StatusUnauthorized, viaSession.Code)
	assert.Equal(t, http.StatusOK, viaKey.Code)
}

func patchPath(d core.Download) string { return "/api/v1/downloads/" + string(d.ID) }

func TestPatchChangesPriorityAndReturnsTheDownload(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	d := e.addDownload("movie", 0)
	events := e.subscribe()

	rec := e.do("PATCH", patchPath(d), `{"priority":"force"}`, withBearer(key))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got downloadJSON
	decode(t, rec, &got)
	assert.Equal(t, "force", got.Priority)
	assert.Len(t, events(), 1)
}

func TestPatchSetsAndClearsTheCategory(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	cat := core.Category{ID: core.NewID(), Name: "tv"}
	require.NoError(t, e.cats.Create(context.Background(), cat))
	d := e.addDownload("movie", 0)

	set := e.do("PATCH", patchPath(d), `{"category_id":"`+string(cat.ID)+`"}`, withBearer(key))
	var afterSet downloadJSON
	decode(t, set, &afterSet)
	cleared := e.do("PATCH", patchPath(d), `{"category_id":null}`, withBearer(key))
	var afterClear downloadJSON
	decode(t, cleared, &afterClear)

	require.Equal(t, http.StatusOK, set.Code)
	require.NotNil(t, afterSet.CategoryID)
	assert.Equal(t, string(cat.ID), *afterSet.CategoryID)
	require.Equal(t, http.StatusOK, cleared.Code)
	assert.Nil(t, afterClear.CategoryID)
}

func TestPatchMovesTheDownloadWithinItsQueue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	e.addDownload("a", 0)
	b := e.addDownload("b", time.Second)

	rec := e.do("PATCH", patchPath(b), `{"queue_position":{"move":"top"}}`, withBearer(key))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got downloadJSON
	decode(t, rec, &got)
	assert.Equal(t, 0, got.QueuePosition)
}

func TestPatchRejectsInvalidChanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	d := e.addDownload("movie", 0)
	for name, tc := range map[string]struct{ body, code string }{
		"state transitions are not allowed": {`{"state":"ready"}`, "invalid_json"},
		"other unknown field":               {`{"name":"x"}`, "invalid_json"},
		"unknown priority":                  {`{"priority":"urgent"}`, "invalid_priority"},
		"unknown category":                  {`{"category_id":"nope"}`, "invalid_category"},
		"unknown move":                      {`{"queue_position":{"move":"sideways"}}`, "invalid_move"},
		"position without move":             {`{"queue_position":{}}`, "invalid_move"},
		"absolute position":                 {`{"queue_position":3}`, "invalid_json"},
		"empty patch":                       {`{}`, "empty_patch"},
		"category of the wrong type":        {`{"category_id":5}`, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.do("PATCH", patchPath(d), tc.body, withBearer(key))

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, errorOf(t, rec))
		})
	}
	stored, err := e.downloads.Get(context.Background(), d.ID)
	require.NoError(t, err)
	assert.Equal(t, d, stored)
}

func TestPatchOfUnknownDownloadIs404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	rec := e.do("PATCH", "/api/v1/downloads/nope", `{"priority":"high"}`, withBearer(e.apiKey()))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

type fakeRemover struct{ removed []core.ID }

func (*fakeRemover) Hold(context.Context, core.Download) (func(), error) { return func() {}, nil }

func (f *fakeRemover) RemoveData(_ context.Context, d core.Download) error {
	f.removed = append(f.removed, d.ID)
	return nil
}

func TestDeleteRemovesTheRowAndPublishesRemovedWithoutDeletingData(t *testing.T) {
	t.Parallel()
	remover := &fakeRemover{}
	e := newEnv(t, withDataRemover(remover))
	key := e.apiKey()
	d := e.addDownload("movie", 0)
	events := e.subscribe()

	rec := e.do("DELETE", patchPath(d), "", withBearer(key))

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	_, err := e.downloads.Get(context.Background(), d.ID)
	assert.ErrorIs(t, err, core.ErrNotFound)
	assert.Equal(t, []core.Event{core.Removed{ID: d.ID}}, events())
	assert.Empty(t, remover.removed)
}

func TestDeleteWithDeleteDataTrueAsksTheDataRemover(t *testing.T) {
	t.Parallel()
	remover := &fakeRemover{}
	e := newEnv(t, withDataRemover(remover))
	d := e.addDownload("movie", 0)

	rec := e.do("DELETE", patchPath(d)+"?delete_data=true", "", withBearer(e.apiKey()))

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []core.ID{d.ID}, remover.removed)
}

func TestDeleteOfADownloadBeingPostProcessedIsAConflictAndTouchesNothing(t *testing.T) {
	t.Parallel()
	remover := &fakeRemover{}
	e := newEnv(t, withDataRemover(remover))
	d := e.addDownload("movie", 0)
	d.State = core.StatePostProcessing
	require.NoError(t, e.downloads.Delete(context.Background(), d.ID))
	require.NoError(t, e.downloads.Create(context.Background(), d))

	rec := e.do("DELETE", patchPath(d)+"?delete_data=true", "", withBearer(e.apiKey()))

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "download_busy")
	assert.Empty(t, remover.removed)
	_, err := e.downloads.Get(context.Background(), d.ID)
	assert.NoError(t, err)
}

func TestDeleteRejectsBadDeleteDataAndUnknownDownloads(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	key := e.apiKey()
	d := e.addDownload("movie", 0)

	bad := e.do("DELETE", patchPath(d)+"?delete_data=maybe", "", withBearer(key))
	missing := e.do("DELETE", "/api/v1/downloads/nope", "", withBearer(key))

	require.Equal(t, http.StatusBadRequest, bad.Code)
	assert.Equal(t, "invalid_delete_data", errorOf(t, bad))
	assert.Equal(t, http.StatusNotFound, missing.Code)
	_, err := e.downloads.Get(context.Background(), d.ID)
	assert.NoError(t, err, "a rejected request must not delete anything")
}
