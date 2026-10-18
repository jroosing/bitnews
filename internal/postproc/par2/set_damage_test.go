package par2_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/postproc/par2"
)

func joined(s *par2.Set) string { return strings.Join(s.Warnings, "\n") }

func TestLoadDir_TruncatedVolumeKeepsEarlierSlicesAndWarns(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	writeFile(t, dir, "a.par2", set.Index())
	vol := set.Volume(0, 1, 2, 3)
	writeFile(t, dir, "a.vol0+4.par2", vol[:len(vol)-30])

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, got.Recovery, 3)
	require.Contains(t, joined(got), "a.vol0+4.par2")
	require.Contains(t, joined(got), "damaged")
}

func TestLoadDir_FlippedByteInVolumeLosesOnlyThatSlice(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	writeFile(t, dir, "a.par2", set.Index())
	main := len(set.mainPacket())
	vol := set.Volume(0, 1, 2)
	vol[main+len(set.recvPacket(0))+80] ^= 0xFF // inside the data of exponent 1
	writeFile(t, dir, "a.vol0+3.par2", vol)

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, got.Recovery, 2)
	require.Contains(t, got.Recovery, uint32(0))
	require.Contains(t, got.Recovery, uint32(2))
	require.NotEmpty(t, got.Warnings)
}

func TestLoadDir_DropsDuplicatePacketsSilently(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40, 8)...)
	writeFile(t, dir, "a.par2", set.Index())
	writeFile(t, dir, "b.par2", set.Index())
	first := writeFile(t, dir, "c.vol0+2.par2", set.Volume(0, 1))
	writeFile(t, dir, "d.vol0+2.par2", set.Volume(0, 1, 1, 2))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, got.FileDescs, 2)
	require.Len(t, got.IFSCs, 2)
	require.Len(t, got.Recovery, 3)
	require.Equal(t, first, got.Recovery[0].Path, "first file in name order wins")
	require.Empty(t, got.Warnings)
}

func TestLoadDir_FiltersPacketsOfForeignSets(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40, 8)...)
	foreign := newSet(16, testFiles(70)...)
	writeFile(t, dir, "a.par2", foreign.Index()) // sorts first, fewer packets
	writeFile(t, dir, "b.par2", set.Index())
	writeFile(t, dir, "b.vol0+2.par2", concat(set.Volume(0, 1), foreign.Volume(7)))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, par2.ID(set.SetID), got.ID)
	require.Len(t, got.FileDescs, 2)
	require.Len(t, got.Recovery, 2)
	require.NotContains(t, got.Recovery, uint32(7))
	require.Contains(t, joined(got), "other recovery set")
}

func TestLoadDir_EqualCandidateSetsPickSmallestIDWhateverTheFileOrder(t *testing.T) {
	x := newSet(16, testFiles(40)...)
	y := newSet(16, testFiles(41)...)
	want := x.SetID
	if bytes.Compare(y.SetID[:], x.SetID[:]) < 0 {
		want = y.SetID
	}
	for _, order := range [][2]*testSet{{x, y}, {y, x}} {
		dir := t.TempDir()
		writeFile(t, dir, "a.par2", order[0].Index())
		writeFile(t, dir, "b.par2", order[1].Index())

		got, err := par2.LoadDir(dir)

		require.NoError(t, err)
		require.Equal(t, par2.ID(want), got.ID)
	}
}

func TestLoadDir_IgnoresMainPacketWhoseSetIDIsNotTheMD5OfItsBody(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	forged := rawPacket([16]byte{9, 9, 9}, typeMain, set.MainBody)
	writeFile(t, dir, "a.par2", forged)

	_, err := par2.LoadDir(dir)

	require.ErrorIs(t, err, par2.ErrNoMainPacket)
}

func TestLoadDir_RejectsSliceSizesThatAreZeroOrNotAMultipleOfFour(t *testing.T) {
	for _, ss := range []uint64{0, 1, 2, 6, 10, 4*1024 + 2} {
		dir := t.TempDir()
		body := mainBody(ss, [][16]byte{{1}}, nil)
		writeFile(t, dir, "a.par2", rawPacket(md5sum(body), typeMain, body))

		_, err := par2.LoadDir(dir)

		require.ErrorIs(t, err, par2.ErrInvalidSliceSize, "slice size %d", ss)
	}
}

func TestLoadDir_ValidSetBeatsSetWithInvalidSliceSize(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	bad := mainBody(6, [][16]byte{{1}}, nil)
	writeFile(t, dir, "a.par2", rawPacket(md5sum(bad), typeMain, bad))
	writeFile(t, dir, "b.par2", set.Index())

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, par2.ID(set.SetID), got.ID)
}

func TestLoadDir_DropsIFSCWhosePairCountDoesNotMatchTheFileLength(t *testing.T) {
	for name, mutate := range map[string]func([]byte) []byte{
		"extra pair":   func(b []byte) []byte { return append(b, make([]byte, 20)...) },
		"missing pair": func(b []byte) []byte { return b[:len(b)-20] },
		"no pairs":     func(b []byte) []byte { return b[:16] },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			set := newSet(16, testFiles(40, 8)...) // first file: 3 slices
			body := mutate(ifscBody(set.IDs[0], set.Files[0], 16))
			writeFile(t, dir, "a.par2", concat(
				set.mainPacket(), set.fileDescPacket(0), set.fileDescPacket(1),
				rawPacket(set.SetID, typeIFSC, body), set.ifscPacket(1)))

			got, err := par2.LoadDir(dir)

			require.NoError(t, err)
			require.NotContains(t, got.IFSCs, par2.ID(set.IDs[0]))
			require.Contains(t, got.IFSCs, par2.ID(set.IDs[1]))
			require.Contains(t, got.FileDescs, par2.ID(set.IDs[0]))
			require.Contains(t, joined(got), "IFSC")
		})
	}
}

func TestLoadDir_AcceptsALaterIFSCWhenTheFirstOneIsWrong(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	bad := rawPacket(set.SetID, typeIFSC, append(ifscBody(set.IDs[0], set.Files[0], 16), make([]byte, 20)...))
	writeFile(t, dir, "a.par2", concat(set.mainPacket(), set.fileDescPacket(0), bad))
	writeFile(t, dir, "b.par2", concat(set.ifscPacket(0)))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Contains(t, got.IFSCs, par2.ID(set.IDs[0]))
}

func TestLoadDir_ZeroLengthFileNeedsNoSlices(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, fileSpec{Name: "empty.txt"})
	writeFile(t, dir, "a.par2", set.Index())

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Contains(t, got.IFSCs, par2.ID(set.IDs[0]))
	require.Empty(t, got.IFSCs[par2.ID(set.IDs[0])].Slices)
	require.Empty(t, got.Warnings)
}

func TestLoadDir_DropsFileDescWhoseFileIDIsNotTheDocumentedHash(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40, 8)...)
	badBody := fileDescBody([16]byte{0xAA}, set.Files[0]) // ID not derived from the content
	// A FileDesc whose ID is right for the Main packet but whose name was altered.
	renamed := fileDescBody(set.IDs[1], fileSpec{Name: "other.bin", Data: set.Files[1].Data})
	writeFile(t, dir, "a.par2", concat(
		set.mainPacket(),
		rawPacket(set.SetID, typeFileDesc, badBody),
		rawPacket(set.SetID, typeFileDesc, renamed),
		set.ifscPacket(0), set.ifscPacket(1)))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Empty(t, got.FileDescs)
	require.Equal(t, ids(set.IDs), got.FileIDs, "Main stays the source of truth for the file list")
	require.Empty(t, got.IFSCs, "IFSC without a trusted FileDesc is useless")
	require.Contains(t, joined(got), "file ID")
}

func TestLoadDir_FileIDHashUsesNameWithoutPadding(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, fileSpec{Name: "a", Data: []byte("hello world hello world")})
	writeFile(t, dir, "a.par2", set.Index())

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, "a", got.FileDescs[par2.ID(set.IDs[0])].Name)
}

func TestLoadDir_IgnoresFileDescNotListedInMain(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	extra := fileSpec{Name: "extra.bin", Data: []byte("0123456789abcdef0123")}
	writeFile(t, dir, "a.par2", concat(set.Index(),
		rawPacket(set.SetID, typeFileDesc, fileDescBody(extra.ID(), extra))))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, got.FileDescs, 1)
}

func TestLoadDir_KeepsNonRecoveryFileDescs(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	extra := fileSpec{Name: "extra.nfo", Data: []byte("0123456789abcdef0123")}
	body := mainBody(16, [][16]byte{set.IDs[0]}, [][16]byte{extra.ID()})
	setID := md5sum(body)
	writeFile(t, dir, "a.par2", concat(
		rawPacket(setID, typeMain, body),
		rawPacket(setID, typeFileDesc, fileDescBody(set.IDs[0], set.Files[0])),
		rawPacket(setID, typeFileDesc, fileDescBody(extra.ID(), extra))))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, []par2.ID{par2.ID(extra.ID())}, got.NonRecoveryIDs)
	require.Len(t, got.FileDescs, 2)
}

func TestLoadDir_DropsRecoverySlicesOfTheWrongSize(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	short := rawPacket(set.SetID, typeRecvSlic, recvSlicBody(9, make([]byte, 12)))
	long := rawPacket(set.SetID, typeRecvSlic, recvSlicBody(10, make([]byte, 20)))
	writeFile(t, dir, "a.par2", concat(set.Index(), set.recvPacket(1), short, long))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, got.Recovery, 1)
	require.Contains(t, got.Recovery, uint32(1))
	require.Contains(t, joined(got), "recovery slice")
}

func TestLoadDir_ConflictingFileDescsKeepTheFirstAndWarn(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(20000)...)
	other := set.Files[0]
	other.Data = bytes.Clone(other.Data)
	other.Data[19999] ^= 1 // same first 16 KiB, length and name, so same file ID; different full MD5
	conflicting := rawPacket(set.SetID, typeFileDesc, fileDescBody(set.IDs[0], other))
	writeFile(t, dir, "a.par2", concat(set.Index(), conflicting))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, md5sum(set.Files[0].Data), got.FileDescs[par2.ID(set.IDs[0])].MD5)
	require.Contains(t, joined(got), "conflicting")
}

func TestLoadDir_WarnsAboutListedFilesWithoutDescriptions(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40, 8)...)
	writeFile(t, dir, "a.par2", concat(set.mainPacket(), set.fileDescPacket(0), set.ifscPacket(0)))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Len(t, got.FileDescs, 1)
	require.Contains(t, joined(got), "no description")
}

func TestLoadDir_DuplicateFileIDsInMainAreListedOnce(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	body := mainBody(16, [][16]byte{set.IDs[0], set.IDs[0]}, nil)
	setID := md5sum(body)
	writeFile(t, dir, "a.par2", concat(
		rawPacket(setID, typeMain, body),
		rawPacket(setID, typeFileDesc, fileDescBody(set.IDs[0], set.Files[0])),
		rawPacket(setID, typeIFSC, ifscBody(set.IDs[0], set.Files[0], 16))))

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.Equal(t, []par2.ID{par2.ID(set.IDs[0])}, got.FileIDs)
}

func TestLoadDir_WarningsAreSanitizedAndBounded(t *testing.T) {
	dir := t.TempDir()
	set := newSet(16, testFiles(40)...)
	evil := "evil\n\x1b[31m\x00name" + strings.Repeat("A", 60000)
	var stream []byte
	stream = append(stream, set.mainPacket()...)
	for i := range 1000 {
		f := fileSpec{Name: evil + string(rune('a'+i%26)), Data: []byte{byte(i), byte(i >> 8), 1, 2}}
		body := fileDescBody([16]byte{0xEE, byte(i)}, f) // wrong ID so every one produces a warning
		stream = append(stream, rawPacket(set.SetID, typeFileDesc, body)...)
	}
	writeFile(t, dir, "ev\nil\x1b[0m.par2", stream)

	got, err := par2.LoadDir(dir)

	require.NoError(t, err)
	require.NotEmpty(t, got.Warnings)
	require.LessOrEqual(t, len(got.Warnings), 101)
	for _, w := range got.Warnings {
		require.LessOrEqual(t, len(w), 512)
		for _, r := range w {
			require.GreaterOrEqual(t, r, rune(0x20), "control character in %q", w)
			require.NotEqual(t, rune(0x7f), r)
		}
	}
}

func TestLoadDir_HugeMainFileCountDoesNotBlowUp(t *testing.T) {
	dir := t.TempDir()
	body := binary.LittleEndian.AppendUint64(nil, 16)
	body = binary.LittleEndian.AppendUint32(body, 0xFFFFFFFF)
	body = append(body, make([]byte, 32)...)
	writeFile(t, dir, "a.par2", rawPacket(md5sum(body), typeMain, body))

	_, err := par2.LoadDir(dir)

	require.ErrorIs(t, err, par2.ErrNoMainPacket)
}
