package core_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestInfoHashRoundTripsThroughLowercaseHex(t *testing.T) {
	const hex = "0123456789abcdef0123456789abcdef01234567"

	h, err := core.ParseInfoHash(hex)

	require.NoError(t, err)
	assert.Equal(t, hex, h.String())
	assert.False(t, h.IsZero())
}

func TestParseInfoHashAcceptsUppercase(t *testing.T) {
	h, err := core.ParseInfoHash("0123456789ABCDEF0123456789ABCDEF01234567")

	require.NoError(t, err)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", h.String())
}

func TestParseInfoHashTruncatesAV2Hash(t *testing.T) {
	v2 := "0123456789abcdef0123456789abcdef01234567" + "89abcdef89abcdef89abcdef"

	h, err := core.ParseInfoHash(v2)

	require.NoError(t, err)
	assert.Equal(t, v2[:40], h.String())
}

func TestParseInfoHashRejectsMalformedInput(t *testing.T) {
	for name, in := range map[string]string{
		"empty":      "",
		"short":      "abcd",
		"non hex":    "zz23456789abcdef0123456789abcdef01234567",
		"odd length": "0123456789abcdef0123456789abcdef012345678",
		"nul":        "0123456789abcdef0123456789abcdef0123456\x00",
		"padded":     " 0123456789abcdef0123456789abcdef01234567",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := core.ParseInfoHash(in)
			assert.ErrorIs(t, err, core.ErrInvalidInfoHash)
		})
	}
}

func TestZeroInfoHash(t *testing.T) {
	var h core.InfoHash
	assert.True(t, h.IsZero())
}

func TestShareLimitReached(t *testing.T) {
	tests := map[string]struct {
		limit   core.ShareLimit
		ratio   float64
		seeding time.Duration
		want    bool
	}{
		"unlimited never reached":     {core.UnlimitedShareLimit(), 1e9, 1e6 * time.Hour, false},
		"ratio reached":               {core.ShareLimit{Ratio: 2, SeedingTime: -1}, 2, 0, true},
		"ratio not reached":           {core.ShareLimit{Ratio: 2, SeedingTime: -1}, 1.99, time.Hour, false},
		"time reached":                {core.ShareLimit{Ratio: -1, SeedingTime: time.Hour}, 0, time.Hour, true},
		"time not reached":            {core.ShareLimit{Ratio: -1, SeedingTime: time.Hour}, 9, time.Minute, false},
		"either limit is enough":      {core.ShareLimit{Ratio: 2, SeedingTime: time.Hour}, 0.1, 2 * time.Hour, true},
		"zero ratio limit is at once": {core.ShareLimit{Ratio: 0, SeedingTime: -1}, 0, 0, true},
		"zero time limit is at once":  {core.ShareLimit{Ratio: -1, SeedingTime: 0}, 0, 0, true},
		"negative ratio is unlimited": {core.ShareLimit{Ratio: -0.5, SeedingTime: -1}, 5, 0, false},
		"negative time is unlimited":  {core.ShareLimit{Ratio: -1, SeedingTime: -time.Second}, 0, time.Hour, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.limit.Reached(tc.ratio, tc.seeding))
		})
	}
}

func TestTorrentSpecValidate(t *testing.T) {
	ok := core.TorrentSpec{Magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", StorageDir: "/data"}
	require.NoError(t, ok.Validate())
	require.NoError(t, core.TorrentSpec{Metainfo: []byte("d4:infod"), StorageDir: "/data"}.Validate())

	for name, spec := range map[string]core.TorrentSpec{
		"neither":          {StorageDir: "/data"},
		"both":             {Metainfo: []byte("x"), Magnet: ok.Magnet, StorageDir: "/data"},
		"not a magnet":     {Magnet: "http://example.com/x.torrent", StorageDir: "/data"},
		"no storage dir":   {Magnet: ok.Magnet},
		"metainfo too big": {Metainfo: make([]byte, core.MaxMetainfoSize+1), StorageDir: "/data"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, spec.Validate(), core.ErrInvalidTorrentSpec)
		})
	}
}
