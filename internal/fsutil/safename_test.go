package fsutil_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/fsutil"
)

func TestSafeName_OrdinaryNamesPassThroughUnchanged(t *testing.T) {
	for _, in := range []string{
		"movie.mkv",
		"My Show S01E01 [1080p].mkv",
		"archive.part01.rar",
		".hidden",
		"日本語のファイル.txt",
		"no-extension",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := fsutil.SafeName(in)
			require.NoError(t, err)
			assert.Equal(t, in, got)
		})
	}
}

func TestSafeName_RejectsNamesThatAreEmptyOrDots(t *testing.T) {
	for _, in := range []string{"", ".", "..", "...", " ", " . ", "\x00", "\u200b"} {
		t.Run(in, func(t *testing.T) {
			_, err := fsutil.SafeName(in)
			assert.ErrorIs(t, err, fsutil.ErrInvalidName)
		})
	}
}

func TestSafeName_StripsControlAndInvisibleCharacters(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"NUL":              {"a\x00b.txt", "ab.txt"},
		"C0 controls":      {"a\x01\x1f\nb\tc\r.txt", "abc.txt"},
		"DEL":              {"a\x7fb", "ab"},
		"C1 controls":      {"a\u0085\u009fb", "ab"},
		"bidi override":    {"movie\u202etxt.exe", "movietxt.exe"},
		"bidi isolates":    {"a\u2066b\u2069c", "abc"},
		"zero width space": {"a\u200bb\u200dc\ufeffd", "abcd"},
		"line separator":   {"a\u2028b\u2029c", "abc"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := fsutil.SafeName(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSafeName_ReplacesSeparatorsAndSMBUnsafeCharacters(t *testing.T) {
	tests := map[string]struct{ in, want string }{
		"slash":         {"a/b", "a_b"},
		"backslash":     {`a\b`, "a_b"},
		"traversal":     {"../../etc/passwd", ".._.._etc_passwd"},
		"windows path":  {`C:\Windows\x.dll`, "C__Windows_x.dll"},
		"absolute":      {"/etc/passwd", "_etc_passwd"},
		"lone slash":    {"/", "_"},
		"smb specials":  {`a:b*c?d"e<f>g|h`, "a_b_c_d_e_f_g_h"},
		"mixed trailer": {"name. . ", "name"},
		"leading space": {"  name", "name"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := fsutil.SafeName(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSafeName_ReplacesInvalidUTF8WithUnderscore(t *testing.T) {
	got, err := fsutil.SafeName("a\xff\xfeb\xc3.txt")
	require.NoError(t, err)
	assert.Equal(t, "a__b_.txt", got)
	assert.True(t, utf8.ValidString(got))
}

func TestSafeName_PrefixesWindowsReservedNames(t *testing.T) {
	tests := map[string]string{
		"CON":           "_CON",
		"con":           "_con",
		"Prn.txt":       "_Prn.txt",
		"AUX":           "_AUX",
		"nul.tar.gz":    "_nul.tar.gz",
		"COM1":          "_COM1",
		"com9.log":      "_com9.log",
		"LPT1":          "_LPT1",
		"lpt9.txt":      "_lpt9.txt",
		"CON .txt":      "_CON .txt",
		"CONSOLE":       "CONSOLE",
		"COM0":          "COM0",
		"COM10":         "COM10",
		"xCON.txt":      "xCON.txt",
		"a.CON":         "a.CON",
		"COM1:":         "COM1_",
		"LPT\u00b9.txt": "_LPT\u00b9.txt",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			got, err := fsutil.SafeName(in)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestSafeName_CapsLengthAt255BytesPreservingExtension(t *testing.T) {
	long := strings.Repeat("a", 400)

	t.Run("ascii with extension", func(t *testing.T) {
		got, err := fsutil.SafeName(long + ".mkv")
		require.NoError(t, err)
		assert.Len(t, got, 255)
		assert.True(t, strings.HasSuffix(got, "aaa.mkv"))
	})
	t.Run("no extension", func(t *testing.T) {
		got, err := fsutil.SafeName(long)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("a", 255), got)
	})
	t.Run("multibyte truncated on rune boundary", func(t *testing.T) {
		got, err := fsutil.SafeName(strings.Repeat("日", 200) + ".mkv")
		require.NoError(t, err)
		assert.LessOrEqual(t, len(got), 255)
		assert.True(t, utf8.ValidString(got))
		assert.True(t, strings.HasSuffix(got, "日.mkv"))
		assert.Equal(t, 83, strings.Count(got, "日")) // 83*3 + 4 = 253; a 84th rune would need 256
	})
	t.Run("exactly 255 bytes is untouched", func(t *testing.T) {
		in := strings.Repeat("b", 251) + ".mkv"
		got, err := fsutil.SafeName(in)
		require.NoError(t, err)
		assert.Equal(t, in, got)
	})
	t.Run("overlong extension is truncated with the name", func(t *testing.T) {
		got, err := fsutil.SafeName("a." + long)
		require.NoError(t, err)
		assert.Len(t, got, 255)
	})
	t.Run("reserved prefix counts toward the cap", func(t *testing.T) {
		got, err := fsutil.SafeName("CON." + strings.Repeat("x", 300))
		require.NoError(t, err)
		assert.LessOrEqual(t, len(got), 255)
		assert.True(t, strings.HasPrefix(got, "_CON."))
	})
	t.Run("truncation does not leave trailing dot or space", func(t *testing.T) {
		got, err := fsutil.SafeName(strings.Repeat("a", 254) + " .b" + long)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(got), 255)
		assert.False(t, strings.HasSuffix(got, " ") || strings.HasSuffix(got, "."))
	})
}
