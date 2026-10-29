package web_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

func TestTheFolderPickerListsTheFoldersOfAPath(t *testing.T) {
	_, b := settingsSignedIn(t)
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "media", "tv"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "usenet"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), nil, 0o644))

	rec := b.get("/settings/folders?path=" + url.QueryEscape(root))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	doc := parseHTML(t, rec.Body.String())
	list := byID(doc, "folder-list")
	require.NotNil(t, list)
	assert.Equal(t, root, attr(list, "data-path"))
	var dirs []string
	for _, n := range findAll(list, func(n *html.Node) bool { return attr(n, "data-folder") != "" }) {
		dirs = append(dirs, attr(n, "data-folder"))
	}
	assert.Equal(t, []string{filepath.Join(root, "media"), filepath.Join(root, "usenet")}, dirs)
	assert.NotContains(t, rec.Body.String(), "notes.txt", "files are not listed")
	use := byID(doc, "folder-use")
	require.NotNil(t, use)
	assert.Equal(t, root, attr(use, "data-path"))
}

func TestTheFolderPickerOpensAtTheNearestExistingFolder(t *testing.T) {
	_, b := settingsSignedIn(t)
	root := t.TempDir()

	rec := b.get("/settings/folders?path=" + url.QueryEscape(filepath.Join(root, "not", "there")))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, root, attr(byID(parseHTML(t, rec.Body.String()), "folder-list"), "data-path"))
}

func TestTheFolderPickerNeedsASession(t *testing.T) {
	e := newEnv(t)
	b := e.browser()

	rec := b.get("/settings/folders?path=/")

	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "folder-list")
}

func TestThePathFieldsHaveABrowseButton(t *testing.T) {
	_, b := settingsSignedIn(t)

	doc := parseHTML(t, b.get("/settings/paths").Body.String())

	for _, id := range []string{"usenet-incomplete", "usenet-complete", "torrent-incomplete", "torrent-complete", "category-usenet-dir", "category-torrent-dir"} {
		btn := find(doc, func(n *html.Node) bool { return attr(n, "data-folder-input") == id })
		assert.NotNil(t, btn, id)
	}
	assert.NotNil(t, byID(doc, "folder-dialog"))
}
