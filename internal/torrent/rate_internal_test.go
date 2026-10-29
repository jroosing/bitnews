package torrent

import (
	"testing"
	"time"

	anacrolix "github.com/anacrolix/torrent"
	"github.com/stretchr/testify/assert"
)

// anacrolix's counters move in bursts; a second without a chunk in the
// middle of a download must not read as 0 B/s.
func TestTorrentRatesAreAveragedSoAQuietSecondIsNotAStop(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tor := new(anacrolix.Torrent)
	var r rateState
	for i, read := range []int64{0, 4000, 8000, 8000} { // nothing in the last second
		r.sample(tor, t0.Add(time.Duration(i)*time.Second), read, int64(i)*100)
	}

	assert.Equal(t, int64(8000/3), r.downRate)
	assert.Equal(t, int64(100), r.upRate)
}

// A torrent added again (after a pause or a move) counts from zero; that is
// a new start, not a negative rate.
func TestTorrentRatesStartOverForANewIncarnation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var r rateState
	old, fresh := new(anacrolix.Torrent), new(anacrolix.Torrent)
	r.sample(old, t0, 0, 0)
	r.sample(old, t0.Add(time.Second), 50_000, 50_000)

	r.sample(fresh, t0.Add(2*time.Second), 0, 0)
	assert.Zero(t, r.downRate, "one sample of the new torrent")
	r.sample(fresh, t0.Add(3*time.Second), 1000, 2000)

	assert.Equal(t, int64(1000), r.downRate)
	assert.Equal(t, int64(2000), r.upRate)
}
