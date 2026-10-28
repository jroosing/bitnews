package web_test

import (
	"testing"

	"github.com/a-h/templ"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/web"
)

func TestHxAttrsEmitsOnlyTheFieldsSet(t *testing.T) {
	got, err := web.Hx{
		Post:        "/queue/abc123/remove",
		Target:      "#queue-lists",
		Swap:        "outerHTML",
		DisabledElt: "find button[type='submit']",
	}.Attrs()
	require.NoError(t, err)
	assert.Equal(t, templ.Attributes{
		"hx-post":         "/queue/abc123/remove",
		"hx-target":       "#queue-lists",
		"hx-swap":         "outerHTML",
		"hx-disabled-elt": "find button[type='submit']",
	}, got)
}

func TestHxAttrsInclude(t *testing.T) {
	got, err := web.Hx{Get: "/history/seeding", Trigger: "every 5s", Swap: "none", Include: "#history-rows [name=id]"}.Attrs()
	require.NoError(t, err)
	assert.Equal(t, "#history-rows [name=id]", got["hx-include"])
}

func TestHxAttrsZeroValueIsEmpty(t *testing.T) {
	got, err := web.Hx{}.Attrs()
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestHxAttrsVals(t *testing.T) {
	got, err := web.Hx{Post: "/x", Vals: map[string]string{"delete_data": "true", "from": "history"}}.Attrs()
	require.NoError(t, err)
	assert.JSONEq(t, `{"delete_data":"true","from":"history"}`, got["hx-vals"].(string))
}

func TestHxAttrsRefusesNonRelativeURLs(t *testing.T) {
	for name, h := range map[string]web.Hx{
		"absolute":        {Get: "https://evil.example/x"},
		"scheme-relative": {Post: "//evil.example/x"},
		"javascript":      {Get: "javascript:alert(1)"},
		"backslash":       {Delete: `/x\evil`},
		"control":         {Put: "/x\n"},
		"empty-sse":       {SSEConnect: "data:text/html,x"},
		"push-url":        {PushURL: "https://evil.example/"},
		"replace-url":     {ReplaceURL: "//evil.example/"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.Attrs()
			assert.Error(t, err)
		})
	}
}

func TestHxAttrsPushURLTakesTrueAndFalse(t *testing.T) {
	for _, v := range []string{"true", "false", "/history?state=done"} {
		got, err := web.Hx{Get: "/history", PushURL: v}.Attrs()
		require.NoError(t, err)
		assert.Equal(t, v, got["hx-push-url"])
	}
}

func TestHxAttrsValsTakeOnlyIDShapedValues(t *testing.T) {
	for _, bad := range []string{"", `"><script>`, "a b", "a/b", "../x", "x'y", "{}"} {
		_, err := web.Hx{Post: "/x", Vals: map[string]string{"id": bad}}.Attrs()
		assert.Error(t, err, "value %q", bad)
	}
	_, err := web.Hx{Post: "/x", Vals: map[string]string{"bad name": "ok"}}.Attrs()
	assert.Error(t, err)
}

func TestHxMustAttrsPanicsWithAnHxError(t *testing.T) {
	assert.PanicsWithError(t, `web: hx get needs a relative URL, got "https://evil.example/"`, func() {
		web.Hx{Get: "https://evil.example/"}.MustAttrs()
	})
}
