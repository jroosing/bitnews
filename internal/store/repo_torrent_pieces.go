package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// MaxTorrentPieces is the most pieces a torrent may have in the store: 4 Mi
// pieces, a 512 KiB bitmap. The migration enforces the same cap.
const MaxTorrentPieces = 4 << 20

// ErrInvalidPieces is returned for a bitmap that does not match its piece
// count, or a piece count outside [0, MaxTorrentPieces].
var ErrInvalidPieces = errors.New("invalid torrent pieces")

// TorrentPieces is the completion bitmap of one torrent: bit i, counted from
// the least significant bit of Bitmap[i/8], is set when piece i is verified.
type TorrentPieces struct {
	PieceCount int
	Bitmap     []byte // exactly (PieceCount+7)/8 bytes
}

func (p TorrentPieces) validate() error {
	if p.PieceCount < 0 || p.PieceCount > MaxTorrentPieces {
		return fmt.Errorf("%w: piece count %d outside 0 to %d", ErrInvalidPieces, p.PieceCount, MaxTorrentPieces)
	}
	if want := (p.PieceCount + 7) / 8; len(p.Bitmap) != want {
		return fmt.Errorf("%w: bitmap is %d bytes, %d pieces need %d", ErrInvalidPieces, len(p.Bitmap), p.PieceCount, want)
	}
	return nil
}

// TorrentPieceRepo stores a completion bitmap per torrent, keyed by info
// hash. The torrent adapter batches its writes; this type is the row-level
// store only.
type TorrentPieceRepo struct {
	db *DB
}

// NewTorrentPieceRepo returns a TorrentPieceRepo that stores bitmaps in db.
func NewTorrentPieceRepo(db *DB) *TorrentPieceRepo { return &TorrentPieceRepo{db: db} }

// Save replaces the bitmap of h.
func (r *TorrentPieceRepo) Save(ctx context.Context, h core.InfoHash, p TorrentPieces) error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Bitmap == nil {
		p.Bitmap = []byte{} // a nil slice would bind as NULL
	}
	_, err := r.db.Writer().ExecContext(ctx,
		`INSERT INTO torrent_pieces (info_hash, piece_count, bitmap, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (info_hash) DO UPDATE SET
		   piece_count = excluded.piece_count, bitmap = excluded.bitmap, updated_at = excluded.updated_at`,
		h.String(), p.PieceCount, p.Bitmap, toMillis(time.Now()))
	return mapErr("save torrent pieces", err)
}

// Load returns the bitmap of h, or core.ErrNotFound. A stored row that fails
// validation is reported as ErrInvalidPieces.
func (r *TorrentPieceRepo) Load(ctx context.Context, h core.InfoHash) (TorrentPieces, error) {
	var p TorrentPieces
	err := r.db.Reader().QueryRowContext(ctx,
		`SELECT piece_count, bitmap FROM torrent_pieces WHERE info_hash = ?`, h.String()).
		Scan(&p.PieceCount, &p.Bitmap)
	if err != nil {
		return TorrentPieces{}, mapErr("load torrent pieces", err)
	}
	if err := p.validate(); err != nil {
		return TorrentPieces{}, err
	}
	return p, nil
}

// Get reports whether piece index of h is verified. A torrent with no row,
// or an index past its piece count, is core.ErrNotFound.
func (r *TorrentPieceRepo) Get(ctx context.Context, h core.InfoHash, index int) (bool, error) {
	p, err := r.Load(ctx, h)
	if err != nil {
		return false, err
	}
	if index < 0 || index >= p.PieceCount {
		return false, fmt.Errorf("store: get torrent piece %d of %d: %w", index, p.PieceCount, core.ErrNotFound)
	}
	return p.Bitmap[index/8]&(1<<(index%8)) != 0, nil
}

// Set marks piece index of h verified or not. The first Set of a torrent
// creates its row with pieceCount pieces; later calls must use the same count.
func (r *TorrentPieceRepo) Set(ctx context.Context, h core.InfoHash, pieceCount, index int, complete bool) error {
	if index < 0 || index >= pieceCount {
		return fmt.Errorf("%w: piece %d of %d", ErrInvalidPieces, index, pieceCount)
	}
	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		p := TorrentPieces{PieceCount: pieceCount, Bitmap: make([]byte, (pieceCount+7)/8)}
		var storedCount int
		var stored []byte
		err := tx.QueryRowContext(ctx,
			`SELECT piece_count, bitmap FROM torrent_pieces WHERE info_hash = ?`, h.String()).
			Scan(&storedCount, &stored)
		switch {
		case err == nil:
			if storedCount != pieceCount {
				return fmt.Errorf("%w: torrent has %d pieces, not %d", ErrInvalidPieces, storedCount, pieceCount)
			}
			p.Bitmap = stored
		case !errors.Is(err, sql.ErrNoRows):
			return mapErr("set torrent piece", err)
		}
		if err := p.validate(); err != nil {
			return err
		}
		if complete {
			p.Bitmap[index/8] |= 1 << (index % 8)
		} else {
			p.Bitmap[index/8] &^= 1 << (index % 8)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO torrent_pieces (info_hash, piece_count, bitmap, updated_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (info_hash) DO UPDATE SET bitmap = excluded.bitmap, updated_at = excluded.updated_at`,
			h.String(), pieceCount, p.Bitmap, toMillis(time.Now()))
		return mapErr("set torrent piece", err)
	})
}

// Delete removes the row of h. A torrent with no row is not an error.
func (r *TorrentPieceRepo) Delete(ctx context.Context, h core.InfoHash) error {
	_, err := r.db.Writer().ExecContext(ctx, `DELETE FROM torrent_pieces WHERE info_hash = ?`, h.String())
	return mapErr("delete torrent pieces", err)
}

// ListInfoHashes returns the info hashes that have a row, in hex order.
func (r *TorrentPieceRepo) ListInfoHashes(ctx context.Context) ([]core.InfoHash, error) {
	rows, err := r.db.Reader().QueryContext(ctx, `SELECT info_hash FROM torrent_pieces ORDER BY info_hash`)
	if err != nil {
		return nil, mapErr("list torrent pieces", err)
	}
	defer rows.Close()
	var out []core.InfoHash
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, mapErr("list torrent pieces", err)
		}
		h, err := core.ParseInfoHash(s)
		if err != nil {
			return nil, fmt.Errorf("store: list torrent pieces: stored hash %q: %w", s, err)
		}
		out = append(out, h)
	}
	return out, mapErr("list torrent pieces", rows.Err())
}
