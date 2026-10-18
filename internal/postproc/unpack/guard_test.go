package unpack

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostileEntryNamesFailTheWholeUnpackAndLeaveNothingBehind(t *testing.T) {
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
			s := newSeam(t)

			_, err := s.run(t, opener(
				fileEntry("fine.txt", "ok"),
				fileEntry(name, "evil"),
			))

			require.ErrorIs(t, err, ErrPathViolation)
			require.Empty(t, children(t, s.parent), "no staging dir, no dst, nothing outside")
		})
	}
}

func TestOneBadNameStopsTheUnpackBeforeAnyByteIsWritten(t *testing.T) {
	s := newSeam(t)
	opens := 0
	counted := fileEntry("first.txt", "data")
	inner := counted.open
	counted.open = func() (io.Reader, error) { opens++; return inner() }

	_, err := s.run(t, opener(counted, fileEntry("../x", "evil")))

	require.ErrorIs(t, err, ErrPathViolation)
	require.Zero(t, opens, "every entry is checked before the first one is read")
}

func TestNamesAreSanitizedPerComponent(t *testing.T) {
	long := strings.Repeat("x", 300) + ".mkv"
	s := newSeam(t)

	res, err := s.run(t, opener(
		fileEntry("dir/con.txt", "1"),
		fileEntry("dir/a:b?.txt", "2"),
		fileEntry(`win\sub\file.txt`, "3"),
		fileEntry("./lead/./dot.txt", "4"),
		fileEntry("trailing. ", "5"),
		fileEntry(long, "6"),
		fileEntry("dir//double.txt", "7"),
	))

	require.NoError(t, err)
	var cut string
	for _, f := range res.Files {
		if strings.HasSuffix(f, ".mkv") {
			cut = f
		}
	}
	require.NotEmpty(t, cut)
	require.LessOrEqual(t, len(cut), 255)
	require.Equal(t, []string{
		"dir/_con.txt", "dir/a_b_.txt", "dir/double.txt", "lead/dot.txt", "trailing", "win/sub/file.txt", cut,
	}, res.Files)
}

func TestCaseInsensitiveDuplicateNamesAreRejected(t *testing.T) {
	cases := map[string][]*entry{
		"same name twice":              {fileEntry("a.txt", "1"), fileEntry("a.txt", "2")},
		"differing only by case":       {fileEntry("A.txt", "1"), fileEntry("a.TXT", "2")},
		"collide after sanitizing":     {fileEntry("a:b", "1"), fileEntry("a_b", "2")},
		"collide through a component":  {fileEntry("D/x", "1"), fileEntry("d/X", "2")},
		"file then directory":          {fileEntry("a", "1"), dirEntry("A")},
		"directory then file":          {dirEntry("a"), fileEntry("A", "1")},
		"file as a parent directory":   {fileEntry("a", "1"), fileEntry("a/b", "2")},
		"directory file via sanitizer": {fileEntry("a", "1"), fileEntry("a/./b", "2")},
	}
	for label, entries := range cases {
		t.Run(label, func(t *testing.T) {
			s := newSeam(t)

			_, err := s.run(t, opener(entries...))

			require.ErrorIs(t, err, ErrPathViolation)
			require.Empty(t, children(t, s.parent))
		})
	}
}

func TestRepeatedDirectoryEntriesAreHarmless(t *testing.T) {
	s := newSeam(t)

	_, err := s.run(t, opener(dirEntry("d"), dirEntry("d/"), dirEntry("D"), fileEntry("d/f", "x"), dirEntry("."), dirEntry("./")))

	require.NoError(t, err)
	require.Equal(t, []string{"d", "d/f"}, listing(t, s.dst))
}

func TestARootNamedFileIsRejected(t *testing.T) {
	s := newSeam(t)

	_, err := s.run(t, opener(fileEntry("./", "x")))

	require.ErrorIs(t, err, ErrPathViolation)
}

func TestSpecialEntriesAreSkippedAndCounted(t *testing.T) {
	s := newSeam(t)

	res, err := s.run(t, opener(
		otherEntry("link", kindSymlink),
		otherEntry("hard", kindHardlink),
		otherEntry("dev", kindDevice),
		otherEntry("pipe", kindFIFO),
		otherEntry("odd", kindOther),
		fileEntry("real.txt", "data"),
	))

	require.NoError(t, err)
	require.Equal(t, []string{"real.txt"}, listing(t, s.dst))
	require.Equal(t, 5, res.Skipped)
	require.Equal(t, []string{"real.txt"}, res.Files)
}

func TestSpecialEntriesWithHostileNamesStillFailTheUnpack(t *testing.T) {
	s := newSeam(t)

	_, err := s.run(t, opener(otherEntry("../../etc/cron.d/x", kindSymlink)))

	require.ErrorIs(t, err, ErrPathViolation)
}

func TestSkippedEntryContentIsReadSoSolidArchivesStayInStep(t *testing.T) {
	s := newSeam(t)
	read := 0
	link := &entry{name: "link", kind: kindSymlink, size: 3, open: func() (io.Reader, error) {
		return readerFunc(func(p []byte) (int, error) {
			if read > 0 {
				return 0, io.EOF
			}
			read++
			return copy(p, "abc"), nil
		}), nil
	}}

	res, err := s.run(t, opener(link, fileEntry("f", "x")))

	require.NoError(t, err)
	require.Equal(t, 1, read)
	require.Equal(t, 1, res.Skipped)
	require.Equal(t, []string{"f"}, listing(t, s.dst))
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestExtractedFilesHaveNoSpecialModeBits(t *testing.T) {
	s := newSeam(t)

	_, err := s.run(t, opener(dirEntry("d"), fileEntry("d/f", "x")))

	require.NoError(t, err)
	for _, p := range []string{"d", "d/f"} {
		fi, err := os.Lstat(filepath.Join(s.dst, p))
		require.NoError(t, err)
		require.Zero(t, fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky), p)
	}
	fi, err := os.Lstat(filepath.Join(s.dst, "d/f"))
	require.NoError(t, err)
	require.Zero(t, fi.Mode().Perm()&0o111, "files are never executable")
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm()&0o600, "owner can read and write")
}
