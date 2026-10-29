package web

import (
	"fmt"
	"strconv"
	"time"

	"github.com/jroosing/bitnews/internal/view"
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
func formatRate(bytesPerSecond int64) string { return rateOr(bytesPerSecond, "-") }

// rateOr renders a speed, zero when there is none ("0 B/s", "No limit").
func rateOr(bytesPerSecond int64, zero string) string {
	if bytesPerSecond <= 0 {
		return zero
	}
	return formatBytes(bytesPerSecond) + "/s"
}

// formatETA renders a View's ETA, rounded up to the second, "-" when there
// is none.
func formatETA(eta time.Duration) string {
	const day = 24 * 3600
	switch {
	case eta <= 0:
		return "-"
	case eta > 99*day*time.Second:
		return ">99d"
	}
	secs := int64((eta + time.Second - 1) / time.Second) // round up
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

// progressPercent is a View's progress as a whole percentage, as the queue
// and the detail page show it: 100 once the Download has all its data, and
// never 100 before.
func progressPercent(v view.View) int {
	if v.Progress >= 1 {
		return 100
	}
	return percent(v.Done, v.Total)
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
