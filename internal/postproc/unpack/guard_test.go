package unpack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func admitAll(cfg *config, entries ...*entry) error {
	g := newGuard(cfg)
	for _, e := range entries {
		if err := g.admit(e); err != nil {
			return err
		}
	}
	return nil
}

func file(name string) *entry  { return &entry{name: name, kind: kindFile, size: 1} }
func dirOf(name string) *entry { return &entry{name: name, kind: kindDir} }

func TestHostileEntryNamesAreRefused(t *testing.T) {
	names := map[string]string{
		"parent traversal":          "../x",
		"deep traversal":            "a/../../b",
		"traversal inside the path": "a/../b",
		"bare dotdot":               "..",
		"trailing dotdot":           "a/..",
		"backslash traversal":       `..\x`,
		"absolute":                  "/etc/passwd",
		"windows drive":             `C:\x`,
		"drive relative":            "C:x",
		"lowercase drive":           "c:/x",
		"unc path":                  `\\server\share\x`,
		"nul byte":                  "a\x00b",
		"nul after traversal":       "a/..\x00/b",
		"empty":                     "",
		"only dots and spaces":      " . . ",
		"only slashes":              "///",
		"component of dots":         "a/.../b",
		"path far too long":         strings.Repeat("d/", 3000) + "f",
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			require.ErrorIs(t, admitAll(newConfig(nil), file("fine.txt"), file(name)), ErrPathViolation)
		})
	}
}

func TestNamesAreSanitizedPerComponent(t *testing.T) {
	cases := map[string]string{
		"dir/con.txt":                     "dir/_con.txt",
		"dir/a:b?.txt":                    "dir/a_b_.txt",
		`win\sub\file.txt`:                "win/sub/file.txt",
		"./lead/./dot.txt":                "lead/dot.txt",
		"trailing. ":                      "trailing",
		"dir//double.txt":                 "dir/double.txt",
		"a\u202eb\x01c":                   "abc",
		strings.Repeat("x", 300) + ".mkv": strings.Repeat("x", 251) + ".mkv",
	}
	for in, want := range cases {
		got, err := cleanName(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
}

func TestCaseInsensitiveDuplicateNamesAreRefused(t *testing.T) {
	cases := map[string][]*entry{
		"same name twice":              {file("a.txt"), file("a.txt")},
		"differing only by case":       {file("A.txt"), file("a.TXT")},
		"collide after sanitizing":     {file("a:b"), file("a_b")},
		"collide through a component":  {file("D/x"), file("d/X")},
		"file then directory":          {file("a"), dirOf("A")},
		"directory then file":          {dirOf("a"), file("A")},
		"file as a parent directory":   {file("a"), file("a/b")},
		"directory file via sanitizer": {file("a"), file("a/./b")},
	}
	for label, entries := range cases {
		t.Run(label, func(t *testing.T) {
			require.ErrorIs(t, admitAll(newConfig(nil), entries...), ErrPathViolation)
		})
	}
}

func TestRepeatedDirectoryEntriesAreHarmless(t *testing.T) {
	require.NoError(t, admitAll(newConfig(nil), dirOf("d"), dirOf("d/"), dirOf("D"), file("d/f"), dirOf("."), dirOf("./")))
}

func TestAFileNamedAfterTheRootIsRefused(t *testing.T) {
	require.ErrorIs(t, admitAll(newConfig(nil), file("./")), ErrPathViolation)
}

func TestOnlyFilesAndDirectoriesAreAdmitted(t *testing.T) {
	for _, k := range []entryKind{kindSymlink, kindHardlink, kindDevice, kindFIFO, kindOther} {
		t.Run(k.String(), func(t *testing.T) {
			err := admitAll(newConfig(nil), file("ok"), &entry{name: "special", kind: k})

			require.ErrorIs(t, err, ErrPathViolation)
		})
	}
}

func TestASpecialEntryWithAHostileNameIsAPathViolationToo(t *testing.T) {
	require.ErrorIs(t, admitAll(newConfig(nil), &entry{name: "../../etc/cron.d/x", kind: kindSymlink}), ErrPathViolation)
}

func TestEntryCountCap(t *testing.T) {
	cfg := newConfig([]Option{WithMaxEntries(2)})

	require.NoError(t, admitAll(cfg, file("a"), file("b")))
	require.ErrorIs(t, admitAll(cfg, file("a"), file("b"), file("c")), ErrTooManyEntries)
	require.ErrorIs(t, admitAll(cfg, file("a"), dirOf("d"), dirOf("e")), ErrLimit, "directories count as entries")
}

func TestDeclaredSizesAreSummedAndSaturate(t *testing.T) {
	g := newGuard(newConfig(nil))
	require.NoError(t, g.admit(&entry{name: "a", kind: kindFile, size: 10}))
	require.NoError(t, g.admit(&entry{name: "b", kind: kindFile, size: 5}))
	require.EqualValues(t, 15, g.declared)
	require.NoError(t, g.admit(&entry{name: "c", kind: kindFile, size: 1 << 62}))
	require.NoError(t, g.admit(&entry{name: "d", kind: kindFile, size: 1 << 62}))
	require.EqualValues(t, int64(1<<62), g.declared)
}

func TestErrorMessagesDoNotCarryControlCharacters(t *testing.T) {
	_, err := cleanName("../evil\x1b[31m\nname")

	require.Error(t, err)
	require.NotContains(t, err.Error(), "\x1b")
	require.NotContains(t, err.Error(), "\n")
}
