package unpack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func block(fields ...string) string { return strings.Join(fields, "\n") + "\n\n" }

func TestParseListingReadsNamesSizesAndKinds(t *testing.T) {
	out := "\nEnter password:\n" +
		block("Path = docs", "Folder = +", "Size = 0", "Attributes = D drwxr-xr-x", "Encrypted = -") +
		block("Path = docs/a b.txt", "Folder = -", "Size = 12", "Packed Size = 5", "Attributes = A -rw-r--r--", "Encrypted = +", "Created = ") +
		block("Path = l", "Size = 3", "Attributes =  lrwxrwxrwx") +
		block("Path = win", "Size = 1", "Attributes = RAL") +
		block("Path = p", "Size = 0", "Attributes = A prw-r--r--") +
		block("Path = h", "Size = 0", "Hard Link = other") +
		block("Path = plain", "Size = 7", "Attributes = A")

	got, err := parseListing([]byte(out))

	require.NoError(t, err)
	require.Len(t, got, 7)
	want := []entry{
		{name: "docs", kind: kindDir},
		{name: "docs/a b.txt", kind: kindFile, size: 12, encrypted: true},
		{name: "l", kind: kindSymlink, size: 3},
		{name: "win", kind: kindSymlink, size: 1},
		{name: "p", kind: kindFIFO},
		{name: "h", kind: kindHardlink},
		{name: "plain", kind: kindFile, size: 7},
	}
	for i := range want {
		require.Equal(t, want[i], *got[i], want[i].name)
	}
}

func TestParseListingWithoutTrailingBlankLine(t *testing.T) {
	got, err := parseListing([]byte("Path = a\nSize = 1"))

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.EqualValues(t, 1, got[0].size)
}

func TestParseListingOfNothingIsEmpty(t *testing.T) {
	for _, in := range []string{"", "\n", "\nEnter password:\n\n"} {
		got, err := parseListing([]byte(in))
		require.NoError(t, err, in)
		require.Empty(t, got)
	}
}

func TestParseListingRefusesForgedOrMalformedOutput(t *testing.T) {
	cases := map[string]string{
		// A name with a line break is how an archive forges fields.
		"name forges a smaller size":  "Path = a\nSize = 0\nAttributes = D\nFolder = -\nSize = 99999999\n\n",
		"name with a line break":      "Path = a\nb\nSize = 5\n\n",
		"name forges a second entry":  "Path = a\nPath = ../../x\nSize = 5\n\n",
		"comment forges encrypted":    "Path = a\nSize = 5\nEncrypted = -\nComment = x\nEncrypted = -\n\n",
		"duplicate key":               "Path = a\nSize = 5\nSize = 6\n\n",
		"line without a key":          "Path = a\nthis is not a field\n\n",
		"text after the entries":      "Path = a\nSize = 5\n\nERRORS:\nHeaders Error\n",
		"negative size":               "Path = a\nSize = -1\n\n",
		"size not a number":           "Path = a\nSize = 12x\n\n",
		"size overflows":              "Path = a\nSize = 99999999999999999999\n\n",
		"size above the sane maximum": "Path = a\nSize = 9223372036854775807\n\n",
		"empty key":                   "Path = a\n = 5\n\n",
		"very long key":               "Path = a\n" + strings.Repeat("k", 100) + " = 5\n\n",
		"a Path line inside an entry": "Path = a\nSize = 1\nPath = b\n\n",
	}
	for label, in := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := parseListing([]byte(in))

			require.ErrorIs(t, err, ErrCorrupt)
		})
	}
}

func TestParseListingToleratesCRLFAndUnknownFields(t *testing.T) {
	got, err := parseListing([]byte("Path = a\r\nSize = 3\r\nSomething New = x\r\n\r\n"))

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "a", got[0].name)
}

func TestKindFromAttributes(t *testing.T) {
	cases := map[string]entryKind{
		"":                kindFile,
		"A":               kindFile,
		"A -rw-r--r--":    kindFile,
		" -rwxr-xr-x":     kindFile,
		"D":               kindDir,
		"D drwxr-xr-x":    kindDir,
		" lrwxrwxrwx":     kindSymlink,
		"A lrwxr-xr-x":    kindSymlink,
		"RAL":             kindSymlink,
		"A crw-rw-rw-":    kindDevice,
		"A brw-rw----":    kindDevice,
		"A prw-r--r--":    kindFIFO,
		"A srwxr-xr-x":    kindOther,
		"A -rwsr-xr-x":    kindFile,
		"A -rwxrwxrwt":    kindFile,
		"junk":            kindFile,
		"A -rw-r--r-- zz": kindFile,
	}
	for attr, want := range cases {
		require.Equal(t, want, kindFromAttributes(attr), attr)
	}
}
