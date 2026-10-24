package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/bootstrap"
)

func TestNewLoggerJSONFormatWritesJSONLines(t *testing.T) {
	var out bytes.Buffer
	log := bootstrap.NewLogger(&out, bootstrap.Config{LogFormat: bootstrap.LogJSON, LogLevel: slog.LevelInfo})

	log.Info("started", "listen", "0.0.0.0:9494")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &rec), "output %q is not JSON", out.String())
	assert.Equal(t, "started", rec["msg"])
	assert.Equal(t, "INFO", rec["level"])
	assert.Equal(t, "0.0.0.0:9494", rec["listen"])
}

func TestNewLoggerTextFormatWritesKeyValuePairs(t *testing.T) {
	var out bytes.Buffer
	log := bootstrap.NewLogger(&out, bootstrap.Config{LogFormat: bootstrap.LogText, LogLevel: slog.LevelInfo})

	log.Info("started", "listen", "0.0.0.0:9494")

	for _, want := range []string{"level=INFO", "msg=started", "listen=0.0.0.0:9494"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestNewLoggerDropsRecordsBelowLevel(t *testing.T) {
	var out bytes.Buffer
	log := bootstrap.NewLogger(&out, bootstrap.Config{LogFormat: bootstrap.LogText, LogLevel: slog.LevelWarn})

	log.Info("hidden")
	log.Warn("shown")

	assert.NotContains(t, out.String(), "hidden")
	assert.Contains(t, out.String(), "shown")
}
