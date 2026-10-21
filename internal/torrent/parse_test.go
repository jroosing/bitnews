package torrent_test

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/torrent"
)

// metainfoOf builds a .torrent from files (path, size), with correct piece
// hashes over zero bytes. mod may change the info dictionary and the
// metainfo before they are encoded.
func metainfoOf(t *testing.T, name string, files []metainfo.FileInfo, mod func(*metainfo.Info, *metainfo.MetaInfo)) []byte {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: pieceLength}
	var total int64
	for _, f := range files {
		total += f.Length
	}
	if len(files) == 1 && files[0].Path == nil {
		info.Length = files[0].Length
	} else {
		info.Files = files
	}
	for off := int64(0); off < total; off += pieceLength {
		sum := sha1.Sum(make([]byte, min(pieceLength, total-off)))
		info.Pieces = append(info.Pieces, sum[:]...)
	}
	mi := metainfo.MetaInfo{}
	if mod != nil {
		mod(&info, &mi)
	}
	var err error
	mi.InfoBytes, err = bencode.Marshal(info)
	require.NoError(t, err)
	out, err := bencode.Marshal(mi)
	require.NoError(t, err)
	return out
}

func TestParseMetainfoDescribesAMultiFileTorrent(t *testing.T) {
	data := metainfoOf(t, "Some.Show.S01", []metainfo.FileInfo{
		{Length: 20000, Path: []string{"ep1.mkv"}},
		{Length: 5, Path: []string{"extras", "readme.txt"}},
	}, func(info *metainfo.Info, mi *metainfo.MetaInfo) {
		priv := true
		info.Private = &priv
		mi.Announce = "http://tracker.example/announce?passkey=abc"
		mi.AnnounceList = metainfo.AnnounceList{{"http://a.example/announce"}, {"udp://b.example:80", "udp://c.example:80"}}
	})
	mi, err := metainfo.Load(strings.NewReader(string(data)))
	require.NoError(t, err)

	got, err := torrent.ParseMetainfo(data)

	require.NoError(t, err)
	assert.Equal(t, "Some.Show.S01", got.Name)
	assert.Equal(t, core.InfoHash(mi.HashInfoBytes()), got.InfoHash)
	assert.True(t, got.InfoHashV2.IsZero())
	assert.EqualValues(t, 20005, got.Size)
	assert.True(t, got.Private)
	assert.Equal(t, []core.TorrentFile{
		{Path: "Some.Show.S01/ep1.mkv", Size: 20000},
		{Path: "Some.Show.S01/extras/readme.txt", Size: 5},
	}, got.Files)
	assert.Equal(t, [][]string{{"http://a.example/announce"}, {"udp://b.example:80", "udp://c.example:80"}}, got.Trackers)
}

func TestParseMetainfoDescribesASingleFileTorrent(t *testing.T) {
	data := metainfoOf(t, "movie.mkv", []metainfo.FileInfo{{Length: 100}}, nil)

	got, err := torrent.ParseMetainfo(data)

	require.NoError(t, err)
	assert.Equal(t, "movie.mkv", got.Name)
	assert.EqualValues(t, 100, got.Size)
	assert.False(t, got.Private)
	assert.Equal(t, []core.TorrentFile{{Path: "movie.mkv", Size: 100}}, got.Files)
	assert.Empty(t, got.Trackers)
}

func TestParseMetainfoRefusesHostileInput(t *testing.T) {
	file := func(length int64, path ...string) metainfo.FileInfo {
		return metainfo.FileInfo{Length: length, Path: path}
	}
	many := make([]metainfo.FileInfo, core.MaxTorrentFiles+1)
	for i := range many {
		many[i] = file(0, "d", strings.Repeat("x", 3), string(rune('a'+i%26)), strings.Repeat("y", i%50), string(rune('A'+i/26%26)), "f"+itoa(i))
	}
	cases := map[string][]byte{
		"not bencode":         []byte("this is not a torrent"),
		"empty":               nil,
		"no info":             []byte("d8:announce3:fooe"),
		"info is not a dict":  []byte("d4:infoi5ee"),
		"trailing garbage":    append(metainfoOf(t, "a", []metainfo.FileInfo{file(1, "f")}, nil), "junk"...),
		"larger than the cap": append(metainfoOf(t, "a", []metainfo.FileInfo{file(1, "f")}, nil), make([]byte, core.MaxMetainfoSize)...),
		"dot dot component":   metainfoOf(t, "root", []metainfo.FileInfo{file(1, "..", "evil")}, nil),
		"dot dot name":        metainfoOf(t, "..", []metainfo.FileInfo{file(1, "f")}, nil),
		"empty component":     metainfoOf(t, "root", []metainfo.FileInfo{file(1, "a", "", "b")}, nil),
		"two files, one path": metainfoOf(t, "root", []metainfo.FileInfo{file(1, "a", "b"), file(1, "a", "b")}, nil),
		"case-colliding":      metainfoOf(t, "root", []metainfo.FileInfo{file(1, "Readme"), file(1, "README")}, nil),
		"file is a directory": metainfoOf(t, "root", []metainfo.FileInfo{file(1, "a"), file(1, "a", "b")}, nil),
		"negative length":     metainfoOf(t, "root", []metainfo.FileInfo{file(-5, "a")}, nil),
		"length overflow": metainfoOf(t, "root", []metainfo.FileInfo{file(1<<62, "a"), file(1<<62, "b")}, func(info *metainfo.Info, _ *metainfo.MetaInfo) {
			info.Pieces = nil
		}),
		"too many files": metainfoOf(t, "root", many, nil),
		"piece length zero": metainfoOf(t, "root", []metainfo.FileInfo{file(1, "a")}, func(info *metainfo.Info, _ *metainfo.MetaInfo) {
			info.PieceLength = 0
		}),
		"piece length huge": metainfoOf(t, "root", []metainfo.FileInfo{file(1, "a")}, func(info *metainfo.Info, _ *metainfo.MetaInfo) {
			info.PieceLength = 1 << 40
		}),
		"pieces not a multiple of 20": metainfoOf(t, "root", []metainfo.FileInfo{file(1, "a")}, func(info *metainfo.Info, _ *metainfo.MetaInfo) {
			info.Pieces = info.Pieces[:19]
		}),
		"fewer pieces than the size needs": metainfoOf(t, "root", []metainfo.FileInfo{file(5*pieceLength, "a")}, func(info *metainfo.Info, _ *metainfo.MetaInfo) {
			info.Pieces = info.Pieces[:20]
		}),
		"more pieces than the size needs": metainfoOf(t, "root", []metainfo.FileInfo{file(1, "a")}, func(info *metainfo.Info, _ *metainfo.MetaInfo) {
			info.Pieces = append(info.Pieces, make([]byte, 40)...)
		}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := torrent.ParseMetainfo(data)
			assert.ErrorIs(t, err, torrent.ErrInvalidMetainfo)
		})
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestParseMetainfoRefusesAV2OnlyTorrent(t *testing.T) {
	data := []byte("d4:infod9:file treed1:ad0:d6:lengthi1e11:pieces root32:" + strings.Repeat("r", 32) + "eee12:meta versioni2e4:name1:n12:piece lengthi16384eee")

	_, err := torrent.ParseMetainfo(data)

	assert.ErrorIs(t, err, torrent.ErrInvalidMetainfo)
}

func TestParseMetainfoAgreesWithTheEnginesPathCheck(t *testing.T) {
	files := []metainfo.FileInfo{{Length: 1, Path: []string{"C:\\x", "CON"}}, {Length: 2, Path: []string{"a\x00b"}}}
	data := metainfoOf(t, "r/o\nt", files, nil)

	enginePaths, engineErr := torrent.TorrentPaths("/data/x", data)
	got, err := torrent.ParseMetainfo(data)

	require.NoError(t, engineErr)
	require.NoError(t, err)
	var paths []string
	for _, f := range got.Files {
		paths = append(paths, f.Path)
	}
	assert.Equal(t, enginePaths, paths)
}

const hexHash = "0123456789abcdef0123456789abcdef01234567"

// b32Hash is hexHash in base32.
var b32Hash = func() string {
	raw, _ := hex.DecodeString(hexHash)
	return base32.StdEncoding.EncodeToString(raw)
}()

func TestParseMagnetReadsHashNameAndTrackers(t *testing.T) {
	uri := "magnet:?xt=urn:btih:" + hexHash + "&dn=Some+Show&tr=http%3A%2F%2Fa.example%2Fannounce%3Fpasskey%3Dxyz&tr=udp%3A%2F%2Fb.example%3A80" +
		"&xs=http%3A%2F%2Fevil.example%2Fx.torrent&as=http%3A%2F%2Fevil.example%2Fy"

	got, err := torrent.ParseMagnet(uri)

	require.NoError(t, err)
	want, _ := core.ParseInfoHash(hexHash)
	assert.Equal(t, want, got.InfoHash)
	assert.True(t, got.InfoHashV2.IsZero())
	assert.Equal(t, "Some Show", got.DisplayName)
	assert.Equal(t, []string{"http://a.example/announce?passkey=xyz", "udp://b.example:80"}, got.Trackers)
}

func TestParseMagnetAcceptsABase32Hash(t *testing.T) {
	got, err := torrent.ParseMagnet("magnet:?xt=urn:btih:" + b32Hash)

	require.NoError(t, err)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", got.InfoHash.String())
}

func TestParseMagnetAcceptsAHybridLinkAndKeepsTheV2Hash(t *testing.T) {
	v2 := strings.Repeat("ab", 32)

	got, err := torrent.ParseMagnet("magnet:?xt=urn:btih:" + hexHash + "&xt=urn:btmh:1220" + v2)

	require.NoError(t, err)
	assert.Equal(t, hexHash, got.InfoHash.String())
	assert.Equal(t, v2[:40], got.InfoHashV2.String())
}

func TestParseMagnetRefusesWhatTheEngineWould(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"not a magnet":         "http://example.com/a.torrent",
		"no xt":                "magnet:?dn=x",
		"v2 only":              "magnet:?xt=urn:btmh:1220" + strings.Repeat("ab", 32),
		"bad multihash":        "magnet:?xt=urn:btih:" + hexHash + "&xt=urn:btmh:zz",
		"short hash":           "magnet:?xt=urn:btih:abcd",
		"long hash":            "magnet:?xt=urn:btih:" + hexHash + "00",
		"not hex":              "magnet:?xt=urn:btih:" + strings.Repeat("g", 40),
		"lower case base32":    "magnet:?xt=urn:btih:" + strings.ToLower(b32Hash),
		"base32 with newlines": "magnet:?xt=urn:btih:" + b32Hash[:30] + "%0A%0A",
		"two btih":             "magnet:?xt=urn:btih:" + hexHash + "&xt=urn:btih:" + strings.Repeat("1", 40),
		"too long":             "magnet:?xt=urn:btih:" + hexHash + "&dn=" + strings.Repeat("a", torrent.MaxMagnetLength),
	}
	for name, uri := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := torrent.ParseMagnet(uri)
			assert.ErrorIs(t, err, torrent.ErrInvalidMagnet)
		})
	}
}

func TestParseMagnetCapsTrackersAndTheDisplayName(t *testing.T) {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:" + hexHash + "&dn=" + strings.Repeat("n", 1500))
	for i := range torrent.MaxTrackers + 20 {
		b.WriteString("&tr=http://t" + strconv.Itoa(i) + ".example/a")
	}
	b.WriteString("&tr=http://long.example/" + strings.Repeat("p", torrent.MaxTrackerLength+100))

	got, err := torrent.ParseMagnet(b.String())

	require.NoError(t, err)
	assert.Len(t, got.Trackers, torrent.MaxTrackers)
	assert.LessOrEqual(t, len(got.DisplayName), 1024)
}

func TestParseMetainfoRefusesNamesBitnewsKeepsForItself(t *testing.T) {
	// Unpack writes _unpack inside the torrent's content directory and
	// replaces what is there, so a torrent must not own that name: its
	// own files would be deleted while they are being seeded.
	tests := []struct {
		name  string
		root  string
		files []metainfo.FileInfo
	}{
		{"unpack dir inside a multi-file torrent", "Release", []metainfo.FileInfo{{Length: 10, Path: []string{"_unpack", "movie.mkv"}}, {Length: 10, Path: []string{"a.nfo"}}}},
		{"unpack dir in another case", "Release", []metainfo.FileInfo{{Length: 10, Path: []string{"_UNPACK", "movie.mkv"}}, {Length: 10, Path: []string{"a.nfo"}}}},
		{"unpack staging dir", "Release", []metainfo.FileInfo{{Length: 10, Path: []string{"_unpack.tmp", "x"}}, {Length: 10, Path: []string{"a.nfo"}}}},
		{"single file named _unpack", "_unpack", []metainfo.FileInfo{{Length: 10}}},
		{"root directory named like the marker", ".bitnews-done", []metainfo.FileInfo{{Length: 10, Path: []string{"a"}}, {Length: 10, Path: []string{"b"}}}},
		{"single file named like the marker", ".bitnews-done", []metainfo.FileInfo{{Length: 10}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := torrent.ParseMetainfo(metainfoOf(t, tt.root, tt.files, nil))

			require.Error(t, err)
			assert.ErrorIs(t, err, torrent.ErrInvalidMetainfo)
		})
	}
}

func TestParseMetainfoAcceptsNamesThatOnlyLookLikeReservedOnes(t *testing.T) {
	data := metainfoOf(t, "Release", []metainfo.FileInfo{{Length: 10, Path: []string{"sub", "_unpack"}}, {Length: 10, Path: []string{"_unpacked", "x"}}}, nil)

	_, err := torrent.ParseMetainfo(data)

	assert.NoError(t, err, "only a top-level _unpack collides with what bitnews writes")
}
