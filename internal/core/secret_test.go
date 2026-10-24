package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

const secretValue = "hunter2-very-secret"

func TestSecretIsRedactedByEveryFmtVerb(t *testing.T) {
	s := core.Secret(secretValue)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		out := fmt.Sprintf(verb, s)
		assert.NotContains(t, out, secretValue, verb)
		assert.Contains(t, out, "[redacted]", verb)
	}
}

func TestSecretInsideAStructIsRedactedByFmt(t *testing.T) {
	type server struct {
		Host     string
		Password core.Secret
	}
	v := server{Host: "news.example.com", Password: core.Secret(secretValue)}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		out := fmt.Sprintf(verb, v)
		assert.NotContains(t, out, secretValue, verb)
		assert.Contains(t, out, "news.example.com", verb)
	}
}

func TestSecretRevealReturnsTheValue(t *testing.T) {
	assert.Equal(t, secretValue, core.Secret(secretValue).Reveal())
}

func TestSecretIsRedactedByJSON(t *testing.T) {
	type server struct {
		Host     string       `json:"host"`
		Password core.Secret  `json:"password"`
		Optional *core.Secret `json:"optional"`
	}
	s := core.Secret(secretValue)
	out, err := json.Marshal(server{Host: "h", Password: s, Optional: &s})
	require.NoError(t, err)
	assert.NotContains(t, string(out), secretValue)
	assert.JSONEq(t, `{"host":"h","password":"[redacted]","optional":"[redacted]"}`, string(out))
}

func TestSecretIsRedactedByTextMarshaling(t *testing.T) {
	out, err := core.Secret(secretValue).MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "[redacted]", string(out))
}

func TestSecretIsRedactedBySlog(t *testing.T) {
	for name, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(newHandler(&buf))
			s := core.Secret(secretValue)
			logger.Info("connecting", "password", s, slog.Group("server", slog.Any("pw", s)))
			assert.NotContains(t, buf.String(), secretValue)
			assert.Contains(t, buf.String(), "[redacted]")
		})
	}
}

func TestSecretUnmarshalsFromAPIInput(t *testing.T) {
	var in struct {
		Password core.Secret `json:"password"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"password":"`+secretValue+`"}`), &in))
	assert.Equal(t, secretValue, in.Password.Reveal())
}

func TestSecretRejectsNonStringJSON(t *testing.T) {
	var s core.Secret
	assert.Error(t, json.Unmarshal([]byte(`123`), &s))
	assert.Error(t, json.Unmarshal([]byte(`{"a":1}`), &s))
}

func TestSecretNullLeavesItUnchanged(t *testing.T) {
	s := core.Secret("keep")
	require.NoError(t, json.Unmarshal([]byte(`null`), &s))
	assert.Equal(t, "keep", s.Reveal())
}

// A custom slog handler that does not use text or JSON encoding sees the
// resolved value, so the Secret must redact itself at that level too.
type attrCollector struct{ attrs []slog.Attr }

func (*attrCollector) Enabled(context.Context, slog.Level) bool { return true }
func (c *attrCollector) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool { c.attrs = append(c.attrs, a); return true })
	return nil
}
func (c *attrCollector) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *attrCollector) WithGroup(string) slog.Handler      { return c }

func TestSecretResolvesToRedactedSlogValue(t *testing.T) {
	c := &attrCollector{}
	slog.New(c).Info("x", "password", core.Secret(secretValue))
	require.Len(t, c.attrs, 1)
	v := c.attrs[0].Value.Resolve()
	assert.Equal(t, slog.KindString, v.Kind())
	assert.Equal(t, "[redacted]", v.String())
}
