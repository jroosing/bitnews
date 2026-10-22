package web

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/core"
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
		remaining, rate int64
		want            string
	}{
		{0, 100, "-"},             // nothing left
		{100, 0, "-"},             // no rate, no estimate
		{100, 100, "1s"},          // exactly a second
		{100, 1000, "1s"},         // rounds up
		{90 * 100, 100, "1m 30s"}, // minutes
		{3 * 3600 * 10, 10, "3h 00m"},
		{(26*3600 + 5*60) * 10, 10, "1d 2h"},
		{1 << 62, 1, ">99d"},
	} {
		assert.Equal(t, tc.want, formatETA(tc.remaining, tc.rate), "%d at %d", tc.remaining, tc.rate)
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

func TestStageLabel(t *testing.T) {
	assert.Equal(t, "Verifying", stageLabel(core.StageVerifying))
	assert.Equal(t, "Repairing", stageLabel(core.StageRepairing))
	assert.Equal(t, "Extracting", stageLabel(core.StageUnpacking))
	assert.Equal(t, "Moving", stageLabel(core.StageFinishing))
	assert.Equal(t, "Processing", stageLabel(""))
	assert.Equal(t, "Processing", stageLabel("something-new"))
}

func TestTruncateRunes(t *testing.T) {
	assert.Equal(t, "abc", truncateRunes("abc", 5))
	assert.Equal(t, "abcd…", truncateRunes("abcdefgh", 5))
	assert.Equal(t, "ééééé", truncateRunes("ééééé", 5), "counts characters, not bytes")
}

var _ = time.Second
