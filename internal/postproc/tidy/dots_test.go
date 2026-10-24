package tidy_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/postproc/tidy"
)

func touch(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestDotUnderscoresRenamesFilesAndFoldersAtTheTopLevel(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Show_S01E01_1080p.mkv"))
	touch(t, filepath.Join(dir, "Sample_Folder", "inner_file_name.txt"))
	touch(t, filepath.Join(dir, "no-underscore.nfo"))

	renamed, err := tidy.DotUnderscores(dir)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Sample.Folder", "Show.S01E01.1080p.mkv", "no-underscore.nfo"}, names(t, dir))
	assert.ElementsMatch(t, []tidy.Rename{
		{From: "Show_S01E01_1080p.mkv", To: "Show.S01E01.1080p.mkv"},
		{From: "Sample_Folder", To: "Sample.Folder"},
	}, renamed)
	assert.ElementsMatch(t, []string{"inner_file_name.txt"}, names(t, filepath.Join(dir, "Sample.Folder")), "it does not recurse")
}

func TestDotUnderscoresNeverOverwritesAnExistingName(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "a_b.txt"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.b.txt"), []byte("keep"), 0o644))

	renamed, err := tidy.DotUnderscores(dir)

	require.NoError(t, err)
	assert.Empty(t, renamed)
	assert.ElementsMatch(t, []string{"a_b.txt", "a.b.txt"}, names(t, dir))
	got, _ := os.ReadFile(filepath.Join(dir, "a.b.txt"))
	assert.Equal(t, "keep", string(got))
}

func TestDotUnderscoresOfAMissingDirectoryIsAnError(t *testing.T) {
	_, err := tidy.DotUnderscores(filepath.Join(t.TempDir(), "nope"))

	assert.Error(t, err)
}

func TestDotUnderscoresLeavesASymlinkTargetAlone(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "real_file.txt"))
	require.NoError(t, os.Symlink("real_file.txt", filepath.Join(dir, "link_name")))

	_, err := tidy.DotUnderscores(dir)

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"real.file.txt", "link.name"}, names(t, dir), "the entries are renamed, never what a link points at")
}
