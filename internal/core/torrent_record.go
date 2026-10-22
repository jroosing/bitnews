package core

import (
	"context"
	"time"
)

// TorrentRecord is what bitnews persists about one torrent Download beyond
// its Download and TorrentSource rows: the Seeding counters that must survive
// a restart, the per-Download Share Limit and where the torrent's data lives.
// A Download without a record has all-zero counters, the global default Share
// Limit and has not been moved.
type TorrentRecord struct {
	DownloadID ID
	// Uploaded is the number of bytes uploaded in earlier runs of the
	// daemon. The engine counts from zero after a restart, so the total is
	// Uploaded plus the engine's own count.
	Uploaded int64
	// SeedingTime is the seeding time accrued in earlier runs, with the same
	// meaning as Uploaded. It is stored in whole seconds.
	SeedingTime time.Duration
	// RatioLimit is the Download's own ratio limit. Nil means the global
	// default, a negative value means unlimited.
	RatioLimit *float64
	// SeedingTimeLimit is the Download's own seeding time limit. Nil means
	// the global default, a negative value means unlimited. It is stored in
	// whole seconds.
	SeedingTimeLimit *time.Duration
	// Moved says the torrent's data was moved to its complete location and
	// Location is where it lives now. A Location with Moved false is the
	// target of a move that was started and has not been confirmed yet.
	Moved bool
	// Location is the torrent's storage directory once it was moved; empty
	// before. The stored .torrent file's directory is the original location.
	Location  string
	UpdatedAt time.Time
}

// TorrentRecordRepo persists TorrentRecords. Deleting a Download deletes its
// record.
type TorrentRecordRepo interface {
	// Get returns the record, or an error wrapping ErrNotFound when the
	// Download has none.
	Get(ctx context.Context, id ID) (TorrentRecord, error)
	// Upsert stores r, replacing the previous record of the Download. The
	// Download must exist. UpdatedAt is set by the repository.
	Upsert(ctx context.Context, r TorrentRecord) error
	// Delete forgets the record; an unknown ID is not an error.
	Delete(ctx context.Context, id ID) error
}
