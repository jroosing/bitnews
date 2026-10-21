package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSafeNextOnlyAllowsKnownLocalPaths(t *testing.T) {
	known := func(p string) bool { return p == "/queue" || p == "/history" || strings.HasPrefix(p, "/history/") }
	cases := map[string]string{
		"/queue":                  "/queue",
		"/history?x=1&y=2":        "/history?x=1&y=2",
		"/history#frag":           "/history#frag",
		"":                        "",
		"queue":                   "",
		"/unknown":                "",
		"/api/v1/downloads":       "",
		"https://evil.test/queue": "",
		"//evil.test/queue":       "",
		"///evil.test":            "",
		"/\\evil.test":            "",
		"\\\\evil.test":           "",
		"javascript:alert(1)":     "",
		"/queue\r\nX-Evil: 1":     "",
		"/queue\x00":              "",
		"/queue\t":                "",
		"/queue\u007f":            "",
		"/%2f%2fevil.test":        "",
		"/queue/../../etc":        "",
		" /queue":                 "",
		"/history/abc":            "/history/abc",
		"/history/../queue":       "",
		"/history//x":             "",
		"/queue@evil.test":        "",
	}
	for in, want := range cases {
		assert.Equal(t, want, safeNext(in, known), "%q", in)
	}
}

func TestWaitTextWordsTheBackoff(t *testing.T) {
	cases := map[string]string{
		"1s":     "1 s",
		"400ms":  "1 s",
		"59.2s":  "60 s",
		"2m":     "120 s",
		"121s":   "3 min",
		"15m":    "15 min",
		"14m59s": "15 min",
	}
	for in, want := range cases {
		assert.Equal(t, want, waitText(mustDuration(t, in)), in)
	}
}

func mustDuration(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
