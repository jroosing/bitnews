package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"}, {1, "1 B"}, {1023, "1023 B"}, {1024, "1.0 KB"}, {1536, "1.5 KB"},
		{5 << 20, "5.0 MB"}, {6_120_000_000, "5.7 GB"}, {3 << 40, "3.0 TB"}, {-5, "0 B"},
	} {
		assert.Equal(t, tc.want, formatBytes(tc.in), "%d", tc.in)
	}
}

func TestFormatRate(t *testing.T) {
	assert.Equal(t, "-", formatRate(0))
	assert.Equal(t, "-", formatRate(-3))
	assert.Equal(t, "512 B/s", formatRate(512))
	assert.Equal(t, "48.2 MB/s", formatRate(50_541_363))
}

func TestFormatETA(t *testing.T) {
	for _, tc := range []struct {
		eta  time.Duration
		want string
	}{
		{0, "-"},                       // nothing left, or no estimate
		{-time.Second, "-"},            // never negative
		{time.Second, "1s"},            // exactly a second
		{100 * time.Millisecond, "1s"}, // rounds up
		{90 * time.Second, "1m 30s"},   // minutes
		{3 * time.Hour, "3h 00m"},
		{26*time.Hour + 5*time.Minute, "1d 2h"},
		{1 << 62, ">99d"},
	} {
		assert.Equal(t, tc.want, formatETA(tc.eta), "%s", tc.eta)
	}
}

func TestPercent(t *testing.T) {
	assert.Equal(t, 0, percent(0, 0))
	assert.Equal(t, 0, percent(5, 0))
	assert.Equal(t, 50, percent(50, 100))
	assert.Equal(t, 99, percent(999, 1000), "never 100 until done")
	assert.Equal(t, 100, percent(1000, 1000))
	assert.Equal(t, 100, percent(2000, 1000), "clamped")
	assert.Equal(t, 0, percent(-4, 10))
}
