package sabnzbd_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/adapter/sabnzbd"
)

func TestGetCatsListsTheDefaultThenTheCategories(t *testing.T) {
	s := newStack(t)
	s.category("tv")
	s.category("movies")

	rec := s.call("mode=get_cats")

	assert.JSONEq(t, `{"categories":["*","movies","tv"]}`, rec.Body.String())
}

func TestGetConfigReportsTheCompleteDirAndCategoryDirsRelativeToIt(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	s.category("tv")
	inside, err := s.categories.Create(ctx, "movies", filepath.Join(s.complete, "films", "hd"))
	require.NoError(t, err)
	outside, err := s.categories.Create(ctx, "music", "/mnt/music")
	require.NoError(t, err)
	_, _ = inside, outside

	m := generic(t, s.call("mode=get_config"))

	cfg := m["config"].(map[string]any)
	misc := cfg["misc"].(map[string]any)
	assert.Equal(t, s.complete, misc["complete_dir"])
	assert.Equal(t, s.incomplete, misc["download_dir"])
	for _, flag := range []string{"enable_tv_sorting", "enable_movie_sorting", "enable_date_sorting", "pre_check"} {
		assert.Equal(t, false, misc[flag], flag)
	}
	assert.Equal(t, []any{}, misc["tv_categories"])
	assert.Equal(t, "all", misc["history_retention_option"])
	dirs := map[string]any{}
	for _, c := range cfg["categories"].([]any) {
		cat := c.(map[string]any)
		dirs[cat["name"].(string)] = cat["dir"]
	}
	assert.Equal(t, map[string]any{"*": "", "tv": "tv", "movies": "films/hd", "music": "/mnt/music"}, dirs)
	assert.Equal(t, "*", cfg["categories"].([]any)[0].(map[string]any)["name"], "the default category comes first")
}

func TestGetConfigNeverContainsAnyKey(t *testing.T) {
	s := newStack(t)

	raw := s.call("mode=get_config").Body.String()

	assert.NotContains(t, raw, s.key)
	assert.NotContains(t, raw, "api_key")
	assert.NotContains(t, raw, "nzb_key")
}

func TestFullStatusIsMinimalButValid(t *testing.T) {
	s := newStack(t, withDeps(func(d *sabnzbd.Deps) {
		d.DiskSpace = func(string) (int64, int64, bool) { return 5 << 30, 10 << 30, true }
	}))
	s.setSpeed(0, true)

	for _, mode := range []string{"fullstatus", "status"} {
		m := generic(t, s.call("mode="+mode))["status"].(map[string]any)

		assert.Equal(t, s.complete, m["completedir"], mode)
		assert.Equal(t, s.incomplete, m["downloaddir"], mode)
		assert.Equal(t, sabnzbd.DefaultVersion, m["version"], mode)
		assert.Equal(t, true, m["paused"], mode)
		assert.Equal(t, "5.00", m["diskspace1"], mode)
		assert.Equal(t, "0", m["have_warnings"], mode)
	}
}

func TestRetryIsNotSupported(t *testing.T) {
	s := newStack(t)

	rec := s.call("mode=retry&value=SABnzbd_nzo_abc")

	assert.JSONEq(t, `{"status":false,"error":"retry is not supported"}`, rec.Body.String())
}
