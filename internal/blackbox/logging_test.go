package blackbox_test

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupIsLoggedInTextFormatWithTheSameFields(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)

	lines := splitLines(d.stop().stderr)

	require.NotEmpty(t, lines)
	assert.Regexp(t, regexp.MustCompile(
		`^time=\S+ level=INFO msg="starting bitnews" version=\S+ commit=\S+ listen=`+regexp.QuoteMeta(d.addr)+
			` config_dir=`+regexp.QuoteMeta(cfg.Dir)+` key_file=`+regexp.QuoteMeta(cfg.KeyFile)+` torrent_port=\d+$`), lines[0])
	if binary.isGo {
		assert.Contains(t, lines[0], "version="+builtVersion+" commit="+builtCommit)
	}
	var listening, done bool
	for _, l := range lines {
		listening = listening || regexp.MustCompile(`^time=\S+ level=INFO msg=listening addr=`+regexp.QuoteMeta(d.addr)+`$`).MatchString(l)
		done = done || regexp.MustCompile(`^time=\S+ level=INFO msg="shutdown complete"$`).MatchString(l)
	}
	assert.True(t, listening, "a line says where it listens")
	assert.True(t, done, "a line says the shutdown is complete")
	assert.Regexp(t, `^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}(Z|[+-]\d\d:\d\d) `, lines[0], "time is RFC 3339 with milliseconds")
}

func TestStartupIsLoggedInJSONFormatWithTheSameFields(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	torrentPort := freePort(t)
	d := startDaemon(t, cfg, withEnv("BITNEWS_LOG_FORMAT", "json", "BITNEWS_TORRENT_PORT", fmt.Sprint(torrentPort)))

	lines := jsonLines(t, d.stop().stderr)

	require.NotEmpty(t, lines)
	for _, l := range lines {
		assert.Contains(t, l, "time")
		assert.Contains(t, l, "level")
		assert.Contains(t, l, "msg")
	}
	start := lines[0]
	assert.Equal(t, "INFO", start["level"])
	assert.Equal(t, "starting bitnews", start["msg"])
	assert.Equal(t, d.addr, start["listen"])
	assert.Equal(t, cfg.Dir, start["config_dir"])
	assert.Equal(t, cfg.KeyFile, start["key_file"])
	assert.EqualValues(t, torrentPort, start["torrent_port"], "a number, not a string")
	assert.IsType(t, "", start["version"])
	assert.IsType(t, "", start["commit"])
	assert.Equal(t, "listening", lines[len(lines)-2]["msg"])
	assert.Equal(t, "shutdown complete", lines[len(lines)-1]["msg"])
	assert.Equal(t, d.addr, lines[len(lines)-2]["addr"])
}

func TestLogLevelFiltersWhatIsLogged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level string
		// present and absent are substrings of stderr.
		present []string
		absent  []string
	}{
		{"debug", []string{`level=INFO msg="starting bitnews"`, `level=DEBUG msg=request`, "url=/api/v1/health status=200"}, nil},
		{"info", []string{`level=INFO msg="starting bitnews"`, `level=INFO msg=request`, "url=/api/v1/version"}, []string{"url=/api/v1/health"}},
		{"warn", []string{`level=WARN msg="generated a new master key`}, []string{`level=INFO`, "starting bitnews", "msg=request"}},
		{"error", nil, []string{`level=INFO`, `level=WARN`, `level=DEBUG`}},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			t.Parallel()
			d := startDaemon(t, freshConfigDir(t), withEnv("BITNEWS_LOG_LEVEL", tt.level))
			d.get("/api/v1/health", nil)
			d.get("/api/v1/version", nil)

			out := d.stop().stderr

			for _, s := range tt.present {
				assert.Contains(t, out, s)
			}
			for _, s := range tt.absent {
				assert.NotContains(t, out, s)
			}
		})
	}
}

// The text handler quotes a value that has special characters, such as the
// "?" of a query; the pattern accepts both forms.
func TestEveryRequestIsLoggedWithMethodURLStatusBytesDurationAndRemote(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg)
	r := d.get("/api/v1/version?x=1", bearer(cfg.AppKey))
	require.Equal(t, http.StatusOK, r.status)
	id := r.header.Get("X-Request-Id")

	out := d.stop().stderr

	assert.Regexp(t, regexp.MustCompile(
		`(?m)^time=\S+ level=INFO msg=request request_id=`+id+` method=GET url="?/api/v1/version\?x=1"? status=200 bytes=`+
			fmt.Sprint(len(r.body))+` duration=\S+ remote=127\.0\.0\.1:\d+$`), out)
}

func TestRequestsAreLoggedAsJSONWithAnIntegerDuration(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg, withEnv("BITNEWS_LOG_FORMAT", "json"))
	r := d.get("/api/v1/nope", nil)
	id := r.header.Get("X-Request-Id")

	var line map[string]any
	for _, l := range jsonLines(t, d.stop().stderr) {
		if l["request_id"] == id {
			line = l
		}
	}

	require.NotNil(t, line, "the request is in the log")
	assert.Equal(t, "INFO", line["level"])
	assert.Equal(t, "request", line["msg"])
	assert.Equal(t, "GET", line["method"])
	assert.Equal(t, "/api/v1/nope", line["url"])
	assert.EqualValues(t, 404, line["status"])
	assert.EqualValues(t, len(r.body), line["bytes"])
	assert.Regexp(t, `^127\.0\.0\.1:\d+$`, line["remote"])
	duration, ok := line["duration"].(float64)
	assert.True(t, ok && duration == float64(int64(duration)), "duration is a whole number of nanoseconds: %v", line["duration"])
}

func TestSecretsAreNeverLogged(t *testing.T) {
	t.Parallel()
	cfg := prepareConfigDir(t)
	d := startDaemon(t, cfg, withEnv("BITNEWS_LOG_LEVEL", "debug"))
	d.get("/api/v1/servers", bearer(cfg.AppKey))
	out := d.stop().stderr

	assert.NotContains(t, out, serverPassword)
	assert.NotContains(t, out, adminPassword)
	assert.NotContains(t, out, string(readFile(t, cfg.KeyFile))[:40], "the master key")
	assert.NotContains(t, out, cfg.AppKey)
}
