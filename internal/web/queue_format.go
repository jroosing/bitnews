package web

import (
	"fmt"
	"strconv"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// The formatters below turn numbers into the short strings the queue shows.
// They use binary units (1 KB = 1024 B) like the rest of bitnews.

var byteUnits = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// formatBytes renders a size, such as "5.7 GB".
func formatBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(byteUnits)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + " B"
	}
	return fmt.Sprintf("%.1f %s", v, byteUnits[i])
}

// formatRate renders a speed, "-" when there is none.
func formatRate(bytesPerSecond int64) string {
	if bytesPerSecond <= 0 {
		return "-"
	}
	return formatBytes(bytesPerSecond) + "/s"
}

// formatETA renders the time left for remaining bytes at rate bytes/s, "-"
// when it cannot be estimated.
func formatETA(remaining, rate int64) string {
	if remaining <= 0 || rate <= 0 {
		return "-"
	}
	secs := (remaining + rate - 1) / rate // round up
	const day = 24 * 3600
	switch {
	case secs > 99*day:
		return ">99d"
	case secs >= day:
		return fmt.Sprintf("%dd %dh", secs/day, secs%day/3600)
	case secs >= 3600:
		return fmt.Sprintf("%dh %02dm", secs/3600, secs%3600/60)
	case secs >= 60:
		return fmt.Sprintf("%dm %02ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%ds", secs)
}

// percent is done as a whole percentage of total, never 100 before done is
// total, and 0 when the total is unknown.
func percent(done, total int64) int {
	switch {
	case total <= 0 || done <= 0:
		return 0
	case done >= total:
		return 100
	}
	return min(int(done*100/total), 99)
}

// stageLabel is the text of a Post-processing stage.
func stageLabel(s core.Stage) string {
	switch s {
	case core.StageVerifying:
		return "Verifying"
	case core.StageRepairing:
		return "Repairing"
	case core.StageUnpacking:
		return "Extracting"
	case core.StageFinishing:
		return "Moving"
	}
	return "Processing"
}

// truncateRunes shortens s to at most n characters, ending in an ellipsis.
// It is used where a Download name goes into a sentence (the confirmation
// dialog); the table truncates with CSS instead.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
