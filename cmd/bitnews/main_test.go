package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// env builds a getenv func backed by a map.
func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func validEnv() map[string]string {
	return map[string]string{"HOME": "/home/u"}
}

func TestVersionFlagPrintsVersionAndCommit(t *testing.T) {
	version, commit = "1.2.3", "abc1234"
	var out, errOut bytes.Buffer

	code := run([]string{"--version"}, env(validEnv()), &out, &errOut)

	require.Equal(t, 0, code)
	for _, want := range []string{"bitnews", "1.2.3", "abc1234"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestVersionFlagIgnoresInvalidConfig(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--version"}, env(map[string]string{"BITNEWS_LISTEN": "nope", "UMASK": "9"}), &out, &errOut)

	assert.Equal(t, 0, code)
	assert.Empty(t, errOut.String())
}

func TestInvalidConfigPrintsErrorAndExitsTwo(t *testing.T) {
	vars := validEnv()
	vars["BITNEWS_LISTEN"] = "no-port"
	var out, errOut bytes.Buffer

	code := run(nil, env(vars), &out, &errOut)

	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "BITNEWS_LISTEN")
	assert.Empty(t, out.String())
}

func TestUnknownFlagPrintsErrorAndExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--bogus"}, env(validEnv()), &out, &errOut)

	assert.Equal(t, 2, code)
	assert.Contains(t, errOut.String(), "bogus")
}

func TestHelpPrintsUsageToStdoutAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"--help"}, env(validEnv()), &out, &errOut)

	assert.Equal(t, 0, code)
	for _, want := range []string{"--config-dir", "BITNEWS_KEY_FILE", "PUID", "UMASK", "BITNEWS_ADMIN_PASSWORD_FILE"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestStartupIsLoggedInConfiguredFormat(t *testing.T) {
	vars := validEnv()
	vars["BITNEWS_LOG_FORMAT"] = "json"
	var out, errOut bytes.Buffer

	code := run([]string{"--listen", "127.0.0.1:9000"}, env(vars), &out, &errOut)

	require.Equal(t, 0, code, errOut.String())
	var rec map[string]any
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &rec), "stderr %q is not one JSON record", errOut.String())
	assert.Equal(t, "127.0.0.1:9000", rec["listen"])
}
