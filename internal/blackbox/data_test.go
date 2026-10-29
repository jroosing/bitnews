package blackbox_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/store"
)

// These tests are about data compatibility (parity gate 2). They use routes
// of the own API that belong to M15-03 (Z2), so a binary that only has the
// Z1 skeleton fails them. Leave them out until then with
//
//	go test -skip 'TestData|TestShutdownEndsAnOpenEventStream|TestARequestBodyAboveTheRouteLimit' ./internal/blackbox/

// list decodes {"items":[...]} from a response.
func list(t *testing.T, r response) []map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, r.status, r.body)
	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &body))
	return body.Items
}

func TestDataGoWrittenServerIsListedWithItsSealedPasswordReadable(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	items := list(t, d.get("/api/v1/servers", bearer(cfg.AppKey)))

	require.Len(t, items, 1)
	s := items[0]
	assert.Equal(t, string(cfg.ServerID), s["id"])
	assert.Equal(t, serverName, s["name"])
	assert.Equal(t, serverHost, s["host"])
	assert.EqualValues(t, 563, s["port"])
	assert.Equal(t, serverUsername, s["username"])
	assert.Equal(t, true, s["has_password"])
	assert.Equal(t, false, s["password_unavailable"], "the password was sealed by Go and unseals with the same key file")
	assert.EqualValues(t, 8, s["max_connections"])
	assert.EqualValues(t, 3000, s["retention_days"])
	assert.EqualValues(t, 1, s["priority"])
	assert.Equal(t, true, s["enabled"])
	assert.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z$`, s["created_at"])
	assert.NotContains(t, s, "password", "the password is never part of an answer")
	assert.NotContains(t, d.get("/api/v1/servers", bearer(cfg.AppKey)).body, serverPassword)
}

func TestDataAServerWhoseSealedPasswordCannotBeOpenedIsMarkedUnavailable(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	// Another valid key in place of the one that sealed the password.
	otherKeyFile := filepath.Join(t.TempDir(), "other.key")
	_, err := store.LoadOrCreateKey(otherKeyFile, discard)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfg.KeyFile, readFile(t, otherKeyFile), 0o600))
	d := startDaemon(t, cfg)

	items := list(t, d.get("/api/v1/servers", bearer(cfg.AppKey)))

	require.Len(t, items, 1, "the Server stays listed")
	assert.Equal(t, serverName, items[0]["name"])
	assert.Equal(t, true, items[0]["has_password"])
	assert.Equal(t, true, items[0]["password_unavailable"], "the user has to enter the password again")
}

func TestDataGoWrittenCategoryIsListed(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	r := d.get("/api/v1/categories", bearer(cfg.AppKey))

	require.Equal(t, http.StatusOK, r.status)
	assert.JSONEq(t, `{"items":[{"id":"`+string(cfg.CategoryID)+`","name":"`+categoryName+`","usenet_dir":"`+categoryUsenetDir+`","torrent_dir":null}]}`, r.body)
}

func TestDataAServerCreatedThroughTheAPIIsReadableByGo(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	body := `{"name":"Made by the binary","host":"news.other.example","port":119,"username":"u2","password":"pässword with ünicode",` +
		`"max_connections":3,"retention_days":0,"priority":2,"enabled":false}`

	r := d.request(http.MethodPost, "/api/v1/servers", map[string]string{"Authorization": "Bearer " + cfg.AppKey, "Content-Type": "application/json"}, body)
	require.Equal(t, http.StatusCreated, r.status, r.body)
	var created map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &created))
	d.stop()

	db := openDB(t, cfg)
	defer func() { require.NoError(t, db.Close()) }()
	key, err := store.LoadOrCreateKey(cfg.KeyFile, discard)
	require.NoError(t, err)
	servers, err := store.NewServerRepo(db, key).List(t.Context())
	require.NoError(t, err)
	byName := map[string]core.Server{}
	for _, s := range servers {
		byName[s.Name] = s
	}
	require.Len(t, byName, 2, "the Go-written Server is still there")
	got := byName["Made by the binary"]
	assert.Equal(t, created["id"], string(got.ID))
	assert.Equal(t, "news.other.example", got.Host)
	assert.Equal(t, 119, got.Port)
	assert.Equal(t, "u2", got.Username)
	assert.Equal(t, "pässword with ünicode", got.Password.Reveal(), "the password sealed by the binary opens in Go")
	assert.False(t, got.PasswordUnavailable)
	assert.Equal(t, 3, got.MaxConnections)
	assert.Equal(t, 2, got.Priority)
	assert.False(t, got.Enabled)
	assert.Equal(t, serverPassword, byName[serverName].Password.Reveal(), "and the old one is untouched")
}

func TestDataACategoryCreatedThroughTheAPIIsReadableByGo(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	r := d.request(http.MethodPost, "/api/v1/categories", map[string]string{"Authorization": "Bearer " + cfg.AppKey, "Content-Type": "application/json"},
		`{"name":"movies","usenet_dir":"/media/movies","torrent_dir":""}`)
	require.Equal(t, http.StatusCreated, r.status, r.body)
	d.stop()

	db := openDB(t, cfg)
	defer func() { require.NoError(t, db.Close()) }()
	cats, err := store.NewCategoryRepo(db).List(t.Context())
	require.NoError(t, err)
	names := map[string]core.Category{}
	for _, c := range cats {
		names[c.Name] = c
	}
	require.Contains(t, names, "movies")
	assert.Equal(t, "/media/movies", names["movies"].UsenetDir)
	assert.Empty(t, names["movies"].TorrentDir)
	assert.Contains(t, names, categoryName)
}

func TestDataDefaultSettingsOfAnEmptyDatabase(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	r := d.get("/api/v1/settings", bearer(cfg.AppKey))

	require.Equal(t, http.StatusOK, r.status)
	requireAPIHeaders(t, r)
	assert.JSONEq(t, defaultSettings, r.body)
	assert.True(t, strings.HasSuffix(r.body, "}\n"))
}

// defaultSettings is GET /api/v1/settings on a database without a settings
// row, with BITNEWS_TORRENT_PORT set (which the answer does not show).
const defaultSettings = `{"queues":{"usenet":1,"torrent":5,"postproc_concurrency":1,"min_free":5368709120},` +
	`"auth":{"disable_auth":false,"lan_bypass_cidrs":[],"trusted_proxies":[],"allowed_hosts":[]},` +
	`"paths":{"usenet":{"incomplete":"/data/usenet/incomplete","complete":"/data/usenet/complete"},"torrent":{"incomplete":"/data/torrents","complete":"/data/torrents"}},` +
	`"network":{"usenet":{"interface":"","address":""},"torrent":{"interface":"","address":""}},` +
	`"seeding":{"ratio_limit":0,"seeding_time_minutes":0,"action":"pause"},` +
	`"speed":{"global":{"down_kbps":0,"up_kbps":0},"usenet":{"down_kbps":0,"up_kbps":0},"torrent":{"down_kbps":0,"up_kbps":0}},` +
	`"torrent":{"listen_port":0,"disable_dht":false,"disable_pex":false,"lsd":false,"mse":""},` +
	`"postprocess":{"clean_names":false,"underscores_to_dots":false}}`
