package coretest

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"strconv"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// TorrentEntry is one file of a built torrent. A nil Path makes the torrent a
// single-file one named after the torrent.
type TorrentEntry struct {
	Path []string
	Size int64
}

type torrentBuilder struct {
	private  bool
	announce []string
}

// TorrentOption changes the torrent BuildTorrent makes.
type TorrentOption func(*torrentBuilder)

// WithPrivate sets the BEP 27 private flag.
func WithPrivate() TorrentOption { return func(b *torrentBuilder) { b.private = true } }

// WithAnnounce adds tracker URLs: the first as "announce", all of them as one
// "announce-list" tier.
func WithAnnounce(urls ...string) TorrentOption {
	return func(b *torrentBuilder) { b.announce = append(b.announce, urls...) }
}

const builtPieceLength = 16 << 10

// BuildTorrent returns the bytes of a valid v1 .torrent file and its info
// hash. File names are written as given, so tests can make hostile ones. The
// content is taken to be zero bytes, which is what the piece hashes cover.
// It writes the bencoding by hand so that no BitTorrent library is needed.
func BuildTorrent(name string, files []TorrentEntry, opts ...TorrentOption) ([]byte, core.InfoHash) {
	var b torrentBuilder
	for _, o := range opts {
		o(&b)
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	var pieces []byte
	for off := int64(0); off < total; off += builtPieceLength {
		sum := sha1.Sum(make([]byte, min(builtPieceLength, total-off)))
		pieces = append(pieces, sum[:]...)
	}

	var info bytes.Buffer
	info.WriteString("d")
	if len(files) != 1 || files[0].Path != nil {
		info.WriteString("5:filesl")
		for _, f := range files {
			info.WriteString("d6:lengthi" + strconv.FormatInt(f.Size, 10) + "e4:pathl")
			for _, p := range f.Path {
				info.WriteString(bstr(p))
			}
			info.WriteString("ee")
		}
		info.WriteString("e")
	} else {
		info.WriteString("6:lengthi" + strconv.FormatInt(files[0].Size, 10) + "e")
	}
	info.WriteString("4:name" + bstr(name))
	info.WriteString("12:piece lengthi" + strconv.Itoa(builtPieceLength) + "e")
	info.WriteString("6:pieces" + bstr(string(pieces)))
	if b.private {
		info.WriteString("7:privatei1e")
	}
	info.WriteString("e")

	var out bytes.Buffer
	out.WriteString("d")
	if len(b.announce) > 0 {
		out.WriteString("8:announce" + bstr(b.announce[0]))
		out.WriteString("13:announce-listll")
		for _, u := range b.announce {
			out.WriteString(bstr(u))
		}
		out.WriteString("ee")
	}
	out.WriteString("4:info")
	out.Write(info.Bytes())
	out.WriteString("e")
	return out.Bytes(), sha1.Sum(info.Bytes())
}

func bstr(s string) string { return fmt.Sprintf("%d:%s", len(s), s) }
