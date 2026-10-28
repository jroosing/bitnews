package torrent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// This file is the torrent file storage: plain positional reads and writes
// on ordinary files. anacrolix's own file storage is not used because its
// default I/O in v1.61.0 maps files into memory (mmap), which
//
//   - fails with "invalid argument" on any piece that spans a zero-length
//     file, so torrents with an empty file never download;
//   - turns a full disk or a flaky network share into SIGBUS, a crash,
//     instead of an error; and
//   - cannot be switched off from Go code (only by an environment variable
//     read in an init function).
//
// Files are opened for each operation and closed again, which is what
// anacrolix's classic I/O does too, so a torrent holds no file descriptors
// and a directory can be moved while the torrent is closed.

const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// torrentFiles is the opened storage of one torrent.
type torrentFiles struct {
	logger  *slog.Logger
	pc      storage.PieceCompletion
	hash    metainfo.Hash
	paths   []string // absolute path of each file, in torrent order
	starts  []int64  // offset of each file in the torrent
	lengths []int64
	total   int64
}

func openTorrentFiles(dir string, info *metainfo.Info, hash metainfo.Hash, pc storage.PieceCompletion, logger *slog.Logger) (*torrentFiles, error) {
	rels, err := torrentPaths(dir, info)
	if err != nil {
		return nil, err
	}
	files := info.UpvertedFiles()
	t := &torrentFiles{logger: logger, pc: pc, hash: hash,
		paths: make([]string, len(files)), starts: make([]int64, len(files)), lengths: make([]int64, len(files))}
	for i := range files {
		t.paths[i] = filepath.Join(dir, filepath.FromSlash(rels[i]))
		t.starts[i] = t.total
		t.lengths[i] = files[i].Length
		t.total += files[i].Length
		if files[i].Length == 0 {
			// No piece ever touches an empty file, so create it now.
			if err := createEmpty(t.paths[i]); err != nil {
				return nil, fmt.Errorf("torrent: create empty file: %w", err)
			}
		}
	}
	return t, nil
}

func createEmpty(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return err
	}
	return f.Close()
}

// TorrentImpl returns the anacrolix view of the storage.
func (t *torrentFiles) TorrentImpl() storage.TorrentImpl {
	return storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl { return &filePiece{t: t, p: p} },
		Close: func() error { return nil },
	}
}

// segment is the part of an extent that lies in one file.
type segment struct {
	file   int
	offset int64 // within the file
	length int64
}

// segments splits the torrent extent [off, off+n) over the files it covers.
// Zero-length files are skipped.
func (t *torrentFiles) segments(off, n int64) []segment {
	var out []segment
	i := sort.Search(len(t.starts), func(i int) bool { return t.starts[i]+t.lengths[i] > off })
	for ; i < len(t.starts) && n > 0; i++ {
		if t.lengths[i] == 0 {
			continue
		}
		fileOff := off - t.starts[i]
		l := min(n, t.lengths[i]-fileOff)
		out = append(out, segment{i, fileOff, l})
		off += l
		n -= l
	}
	return out
}

// filePiece is one piece of the torrent.
type filePiece struct {
	t *torrentFiles
	p metainfo.Piece
}

func (fp *filePiece) key() metainfo.PieceKey {
	return metainfo.PieceKey{InfoHash: fp.t.hash, Index: fp.p.Index()}
}

// ReadAt reads piece data. A missing or short file is io.EOF or
// io.ErrUnexpectedEOF: the data is not there.
func (fp *filePiece) ReadAt(b []byte, off int64) (n int, err error) {
	for _, s := range fp.t.segments(fp.p.Offset()+off, int64(len(b))) {
		f, err := os.Open(fp.t.paths[s.file])
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return n, io.EOF
			}
			return n, err
		}
		n1, err := f.ReadAt(b[n:n+int(s.length)], s.offset)
		_ = f.Close()
		n += n1
		if int64(n1) != s.length {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
	}
	if n < len(b) {
		return n, io.EOF // past the end of the torrent
	}
	return n, nil
}

// WriteAt writes piece data, creating files and directories as needed.
func (fp *filePiece) WriteAt(b []byte, off int64) (n int, err error) {
	for _, s := range fp.t.segments(fp.p.Offset()+off, int64(len(b))) {
		path := fp.t.paths[s.file]
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, filePerm)
		if errors.Is(err, fs.ErrNotExist) {
			if err = os.MkdirAll(filepath.Dir(path), dirPerm); err == nil {
				f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE, filePerm)
			}
		}
		if err != nil {
			return n, err
		}
		n1, err := f.WriteAt(b[n:n+int(s.length)], s.offset)
		n += n1
		if err != nil {
			_ = f.Close()
			return n, err
		}
		if err := f.Close(); err != nil {
			return n, err
		}
	}
	if n < len(b) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

// MarkComplete records the piece as verified. The piece's files are synced
// first, so a crash cannot leave a verified mark for data that never reached
// the disk.
func (fp *filePiece) MarkComplete() error {
	if err := fp.sync(); err != nil {
		fp.t.logger.Warn("torrent: sync of completed piece failed", "piece", fp.p.Index(), "err", err)
	}
	return fp.t.pc.Set(fp.key(), g.Some(true))
}

func (fp *filePiece) sync() error {
	var errs []error
	for _, s := range fp.t.segments(fp.p.Offset(), fp.p.Length()) {
		f, err := os.OpenFile(fp.t.paths[s.file], os.O_WRONLY, 0)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, f.Sync(), f.Close())
	}
	return errors.Join(errs...)
}

// MarkNotComplete records the piece as not verified.
func (fp *filePiece) MarkNotComplete() error { return fp.t.pc.Set(fp.key(), g.Some(false)) }

// Completion reports a verified piece as complete only while its files still
// have at least the size the piece needs; otherwise the piece is unknown and
// gets hashed again.
func (fp *filePiece) Completion() storage.Completion {
	c, err := fp.t.pc.Get(fp.key())
	c.Err = errors.Join(c.Err, err)
	if c.Err != nil || !c.Ok || !c.Complete {
		return c
	}
	for _, s := range fp.t.segments(fp.p.Offset(), fp.p.Length()) {
		info, err := os.Stat(fp.t.paths[s.file])
		if err != nil || info.Size() < s.offset+s.length {
			fp.t.logger.Warn("torrent: file missing or too small for a piece marked complete",
				"piece", fp.p.Index(), "file", filepath.Base(fp.t.paths[s.file]))
			_ = fp.t.pc.Set(fp.key(), g.Some(false))
			return storage.Completion{Ok: true}
		}
	}
	return c
}
