package web_test

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokens reads the design tokens ("--name: #rrggbb;") of the three TOKENS
// blocks of src/app.css: light, dark by media query, dark by attribute.
func tokens(t *testing.T) (light, darkMedia, darkAttr map[string]string) {
	t.Helper()
	b, err := os.ReadFile("src/app.css")
	require.NoError(t, err)
	parts := strings.Split(string(b), "/* TOKENS: ")
	require.Len(t, parts, 4, "expected three TOKENS blocks")
	decl := regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\s*;`)
	parse := func(s string) map[string]string {
		m := map[string]string{}
		if i := strings.Index(s, "/* ----"); i >= 0 {
			s = s[:i]
		}
		if i := strings.Index(s, "/* Map the tokens"); i >= 0 {
			s = s[:i]
		}
		for _, d := range decl.FindAllStringSubmatch(s, -1) {
			m[d[1]] = strings.ToLower(d[2])
		}
		return m
	}
	return parse(parts[1]), parse(parts[2]), parse(parts[3])
}

func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	require.Regexp(t, `^#[0-9a-f]{6}$`, hex)
	ch := func(s string) float64 {
		v, err := strconv.ParseUint(s, 16, 8)
		require.NoError(t, err)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[1:3]) + 0.7152*ch(hex[3:5]) + 0.0722*ch(hex[5:7])
}

// contrast is the WCAG 2.x contrast ratio of two colours.
func contrast(t *testing.T, a, b string) float64 {
	la, lb := luminance(t, a), luminance(t, b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestContrastRatioMath(t *testing.T) {
	assert.InDelta(t, 21.0, contrast(t, "#000000", "#ffffff"), 0.001)
	assert.InDelta(t, 1.0, contrast(t, "#777777", "#777777"), 0.001)
	assert.InDelta(t, 4.48, contrast(t, "#777777", "#ffffff"), 0.01)
}

// Text pairs need 4.5:1 (WCAG AA, normal text); non-text pairs (icons,
// control borders, the focus ring, progress fills) need 3:1.
var (
	textPairs = [][2]string{
		{"fg", "bg"}, {"fg", "surface"}, {"fg", "surface-2"},
		{"fg-muted", "bg"}, {"fg-muted", "surface"}, {"fg-muted", "surface-2"}, {"fg-muted", "surface-3"},
		{"fg-subtle", "bg"}, {"fg-subtle", "surface"}, {"fg-subtle", "surface-2"},
		{"accent-text", "bg"}, {"accent-text", "surface"}, {"accent-text", "surface-2"},
		{"accent-fg", "accent"}, {"accent-fg", "accent-hover"},
		{"danger-fg", "danger"}, {"danger-fg", "danger-hover"},
		{"accent-soft-fg", "accent-soft"},
		{"neutral-fg", "neutral-soft"}, {"info-fg", "info-soft"}, {"ok-fg", "ok-soft"}, {"warn-fg", "warn-soft"}, {"bad-fg", "bad-soft"},
		{"bad-fg", "surface"}, {"bad-fg", "bg"}, {"ok-fg", "surface"}, {"info-fg", "surface"}, {"warn-fg", "surface"},
		{"accent-soft-fg", "surface-2"},
	}
	nonTextPairs = [][2]string{
		{"control", "surface"}, {"control", "bg"},
		{"ring", "surface"}, {"ring", "bg"},
		{"accent-mark", "surface"}, {"accent-mark", "surface-3"}, {"accent-mark", "bg"},
		{"ok-fg", "surface-3"}, {"warn-fg", "surface-3"}, {"bad-fg", "surface-3"},
	}
)

func TestTokensMeetContrastAA(t *testing.T) {
	light, darkMedia, darkAttr := tokens(t)
	for theme, m := range map[string]map[string]string{"light": light, "dark (media)": darkMedia, "dark (attribute)": darkAttr} {
		for _, p := range textPairs {
			fg, bg := m[p[0]], m[p[1]]
			require.NotEmpty(t, fg, "%s: token %s", theme, p[0])
			require.NotEmpty(t, bg, "%s: token %s", theme, p[1])
			assert.GreaterOrEqual(t, contrast(t, fg, bg), 4.5, "%s: %s %s on %s %s", theme, p[0], fg, p[1], bg)
		}
		for _, p := range nonTextPairs {
			fg, bg := m[p[0]], m[p[1]]
			require.NotEmpty(t, fg, "%s: token %s", theme, p[0])
			require.NotEmpty(t, bg, "%s: token %s", theme, p[1])
			assert.GreaterOrEqual(t, contrast(t, fg, bg), 3.0, "%s: %s %s on %s %s", theme, p[0], fg, p[1], bg)
		}
	}
}

func TestBothDarkBlocksDefineTheSameTokens(t *testing.T) {
	light, darkMedia, darkAttr := tokens(t)

	assert.Equal(t, darkMedia, darkAttr, "the media-query and attribute dark themes must stay identical")
	assert.Len(t, darkMedia, len(light), "dark defines every token that light does")
	for name := range light {
		assert.Contains(t, darkMedia, name)
	}
}
