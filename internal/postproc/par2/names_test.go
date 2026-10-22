package par2_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func specs(names ...string) []fileSpec {
	var out []fileSpec
	for _, n := range names {
		out = append(out, fileSpec{Name: n, Data: []byte("data of " + n)})
	}
	return out
}

func names(t *testing.T, data []byte) []string {
	t.Helper()
	got, err := par2.ReadNames(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	return got
}

func TestReadNamesFindsEveryFileDescription(t *testing.T) {
	set := newSet(16, specs("a.bin", "sub/b.bin", "c d.bin")...)

	assert.ElementsMatch(t, []string{"a.bin", "sub/b.bin", "c d.bin"}, names(t, set.index()))
}

func TestReadNamesIgnoresPaddingNULsAndKeepsOrderAndDuplicates(t *testing.T) {
	set := newSet(16, specs("x.bin", "y.bin")...)
	data := concat(set.index(), set.volume([]byte("0123456789abcdef")), set.index())

	assert.Equal(t, []string{"x.bin", "y.bin"}, names(t, data))
}

func TestReadNamesSkipsGarbageAndDamagedPackets(t *testing.T) {
	set := newSet(16, specs("good.bin", "hurt.bin")...)
	idx := set.index()
	hurt := bytes.Index(idx, []byte("hurt.bin"))
	idx[hurt] ^= 0x20 // breaks that FileDesc MD5

	got := names(t, concat([]byte("garbage"), idx, []byte("PAR2\x00PKT\xff\xff")))

	assert.Equal(t, []string{"good.bin"}, got)
}

func TestReadNamesFindsAPacketHiddenInsideAnotherPacket(t *testing.T) {
	// par2 may find a FileDesc where a damaged packet was expected, so the
	// reader must look inside recovery data too.
	hidden := newSet(16, specs("../../evil")...)
	outer := newSet(16, specs("ok.bin")...)
	data := outer.volume(hidden.fileDescPacket(0))

	assert.Equal(t, []string{"../../evil"}, names(t, data))
}

func TestReadNamesFindsMagicsAcrossChunkBoundaries(t *testing.T) {
	set := newSet(16, specs("edge.bin")...)
	pkt := set.fileDescPacket(0)
	for pad := 256<<10 - 12; pad < 256<<10+12; pad++ {
		data := concat(bytes.Repeat([]byte{'x'}, pad), pkt)
		assert.Equal(t, []string{"edge.bin"}, names(t, data), "pad %d", pad)
	}
}

func TestReadNamesRefusesAnOversizedFileDescriptionPacket(t *testing.T) {
	pkt := newSet(16, specs("a")...).fileDescPacket(0)
	binary.LittleEndian.PutUint64(pkt[8:16], 8<<20)
	data := concat(pkt, make([]byte, 8<<20))

	_, err := par2.ReadNames(bytes.NewReader(data), int64(len(data)))

	assert.ErrorIs(t, err, par2.ErrUnsafeSet)
}

func TestReadNamesBoundsTheWorkOnRepeatedBrokenPackets(t *testing.T) {
	pkt := newSet(16, specs("a")...).fileDescPacket(0)
	pkt[20] ^= 1 // MD5 mismatch
	data := bytes.Repeat(pkt, 5000)

	_, err := par2.ReadNames(bytes.NewReader(data), int64(len(data)))

	assert.ErrorIs(t, err, par2.ErrUnsafeSet)
}

func TestReadNamesFileReadsFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.par2")
	require.NoError(t, os.WriteFile(path, newSet(16, specs("d.bin")...).index(), 0o600))

	got, err := par2.ReadNamesFile(path)

	require.NoError(t, err)
	assert.Equal(t, []string{"d.bin"}, got)
}

func TestCheckNameAcceptsPlainRelativePaths(t *testing.T) {
	for _, n := range []string{"a.bin", "Movie (2020) [x264].mkv", "sub/dir/a.bin", "ß 日本.bin", "..a", "a..", ".hidden", "a b.bin", "xy:z.bin", "Star Wars: IV.mkv"} {
		assert.NoError(t, par2.CheckName(n), n)
	}
}

func TestCheckNameRefusesHostileNames(t *testing.T) {
	long := strings.Repeat("a", 300)
	tests := map[string]string{
		"empty":             "",
		"dot dot":           "..",
		"dot":               ".",
		"parent":            "../evil",
		"parent deep":       "a/../../evil",
		"parent mid":        "a/../b",
		"parent end":        "a/..",
		"absolute":          "/etc/passwd",
		"double slash":      "a//b",
		"trailing slash":    "dir/",
		"dot component":     "a/./b",
		"drive letter":      "C:evil",
		"drive path":        `c:/evil`,
		"backslash":         `..\evil`,
		"backslash name":    `a\b`,
		"unc":               `\\host\share\x`,
		"nul":               "a\x00../b",
		"newline":           "a\nRepair is possible.",
		"carriage return":   "a\rb",
		"escape":            "a\x1b[2Jb",
		"del":               "a\x7fb",
		"par2 target":       "set.vol0+4.par2",
		"par2 target upper": "SET.PAR2",
		"long component":    long,
		"long name":         strings.Repeat("a/", 600),
	}
	for name, n := range tests {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, par2.CheckName(n), par2.ErrUnsafeSet)
		})
	}
}

func TestCheckNamesRefusesASymlinkedParentAndASymlinkedTarget(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link")))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "x"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "x"), filepath.Join(dir, "t.bin")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ok.bin"), []byte("x"), 0o600))

	assert.NoError(t, par2.CheckNames(dir, []string{"ok.bin", "missing.bin", "newdir/new.bin"}))
	assert.ErrorIs(t, par2.CheckNames(dir, []string{"link/x"}), par2.ErrUnsafeSet)
	assert.ErrorIs(t, par2.CheckNames(dir, []string{"t.bin"}), par2.ErrUnsafeSet)
}

func FuzzReadNames(f *testing.F) {
	set := newSet(16, specs("a.bin", "../b", "c/d")...)
	f.Add([]byte(nil))
	f.Add([]byte("PAR2\x00PKT"))
	f.Add(set.index())
	f.Add(concat(set.index(), set.volume([]byte("0123456789abcdef"))))
	f.Add(set.index()[:len(set.index())-5])
	f.Add(concat([]byte("junk"), set.fileDescPacket(1), []byte("PAR2\x00PKT\xff\xff\xff\xff\xff\xff\xff\xff")))
	huge := set.fileDescPacket(0)
	binary.LittleEndian.PutUint64(huge[8:16], 1<<40)
	f.Add(huge)

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := par2.ReadNames(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			require.Nil(t, got)
			return
		}
		require.LessOrEqual(t, len(got), par2.MaxNames)
		seen := map[string]bool{}
		for _, n := range got {
			require.False(t, seen[n], "names are distinct")
			seen[n] = true
			// Every name was read from a packet whose MD5 verifies, so its
			// bytes occur in the input.
			require.Contains(t, string(data), n)
			// The check never panics and always answers.
			_ = par2.CheckName(n)
		}
	})
}
