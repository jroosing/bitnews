package runner

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"sync"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// memStore is the ProgressStore used when Config.Progress is nil.
type memStore struct {
	mu    sync.Mutex
	blobs map[core.ID][]byte
}

func newMemStore() *memStore { return &memStore{blobs: map[core.ID][]byte{}} }

func (m *memStore) Load(_ context.Context, id core.ID) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[id]
	if !ok {
		return nil, fmt.Errorf("runner: no progress for %s: %w", id, core.ErrNotFound)
	}
	return b, nil
}

func (m *memStore) Save(_ context.Context, id core.ID, blob []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[id] = append([]byte(nil), blob...)
	return nil
}

func (m *memStore) Delete(_ context.Context, id core.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, id)
	return nil
}

// Resume blob wire format (version 1), integers are unsigned varints unless
// noted:
//
//	"GNRP" version(1 byte)
//	len(writer snapshot), writer snapshot
//	nsegments, nsegments x { file, segment, status(1 byte) }
//	crc32 (IEEE, 4 bytes little endian) over everything before it
const (
	blobMagic   = "GNRP"
	blobVersion = 1
)

var errBadBlob = errors.New("runner: invalid resume blob")

// segRecord is one resolved Article in the blob.
type segRecord struct {
	Key    segKey
	Status segStatus
}

// resumeBlob is what ProgressStore keeps for a Download: the filewriter
// snapshot plus the Articles already resolved. Byte ranges cannot say which
// Articles are done (NZB sizes are encoded sizes), so the Articles are
// recorded themselves.
type resumeBlob struct {
	Writer   []byte
	Segments []segRecord
}

func encodeBlob(b resumeBlob) []byte {
	out := append([]byte(blobMagic), blobVersion)
	out = binary.AppendUvarint(out, uint64(len(b.Writer)))
	out = append(out, b.Writer...)
	out = binary.AppendUvarint(out, uint64(len(b.Segments)))
	for _, s := range b.Segments {
		out = binary.AppendUvarint(out, uint64(s.Key.File))
		out = binary.AppendUvarint(out, uint64(s.Key.Seg))
		out = append(out, byte(s.Status))
	}
	return binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
}

func decodeBlob(data []byte) (resumeBlob, error) {
	hdr := len(blobMagic) + 1
	if len(data) < hdr+4 || string(data[:len(blobMagic)]) != blobMagic {
		return resumeBlob{}, fmt.Errorf("%w: bad header", errBadBlob)
	}
	if data[len(blobMagic)] != blobVersion {
		return resumeBlob{}, fmt.Errorf("%w: version %d", errBadBlob, data[len(blobMagic)])
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return resumeBlob{}, fmt.Errorf("%w: checksum mismatch", errBadBlob)
	}
	rest := body[hdr:]
	var bad error
	uvarint := func() uint64 {
		v, n := binary.Uvarint(rest)
		if n <= 0 {
			bad = fmt.Errorf("%w: bad varint", errBadBlob)
			return 0
		}
		rest = rest[n:]
		return v
	}
	var b resumeBlob
	wl := uvarint()
	if bad == nil && wl > uint64(len(rest)) {
		bad = fmt.Errorf("%w: writer snapshot exceeds data", errBadBlob)
	}
	if bad != nil {
		return resumeBlob{}, bad
	}
	b.Writer = append([]byte(nil), rest[:wl]...)
	rest = rest[wl:]
	n := uvarint()
	if bad == nil && n > uint64(len(rest)/3) {
		bad = fmt.Errorf("%w: segment count exceeds data", errBadBlob)
	}
	for i := uint64(0); bad == nil && i < n; i++ {
		file, seg := uvarint(), uvarint()
		if bad != nil {
			break
		}
		if len(rest) < 1 || file > math.MaxInt32 || seg > math.MaxInt32 || rest[0] < byte(segDone) || rest[0] > byte(segMissing) {
			bad = fmt.Errorf("%w: bad segment", errBadBlob)
			break
		}
		b.Segments = append(b.Segments, segRecord{Key: segKey{int(file), int(seg)}, Status: segStatus(rest[0])})
		rest = rest[1:]
	}
	if bad == nil && len(rest) != 0 {
		bad = fmt.Errorf("%w: trailing bytes", errBadBlob)
	}
	if bad != nil {
		return resumeBlob{}, bad
	}
	return b, nil
}
