package sabnzbd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/view"
)

// The status strings media managers parse. Sonarr and Radarr treat
// "Completed" as importable, "Failed" as blocklistable and every other
// history status as still in progress.
const (
	statusQueued      = "Queued"
	statusPaused      = "Paused"
	statusDownloading = "Downloading"
	statusVerifying   = "Verifying"
	statusRepairing   = "Repairing"
	statusExtracting  = "Extracting"
	statusMoving      = "Moving"
	statusCompleted   = "Completed"
	statusFailed      = "Failed"
)

// listing says where a Download shows up: the queue while its State is in a
// Queue (core.State.InQueue), the history after.
type listing int

const (
	inQueue listing = iota
	inHistory
)

func listingOf(d core.Download) listing {
	if d.State.InQueue() {
		return inQueue
	}
	return inHistory
}

// status translates a View's phase to its SAB status.
//
//	waiting                    Queued
//	metadata                   Queued (Paused if paused)
//	paused                     Paused
//	downloading                Downloading
//	verifying                  Verifying (Fetched, or the stage is verifying or unknown)
//	repairing                  Repairing
//	unpacking                  Extracting
//	finishing                  Moving
//	ready                      Completed
//	failed                     Failed
//
// Completed is only ever returned for the ready phase, which only a Ready
// Download has: Fetched data that is not yet repaired and unpacked must never
// be imported. A phase this table does not know is Queued, never Completed.
func status(v view.View) string {
	switch v.Phase {
	case view.PhaseMetadata:
		if v.Download.Paused {
			return statusPaused
		}
		return statusQueued
	case view.PhasePaused:
		return statusPaused
	case view.PhaseDownloading:
		return statusDownloading
	case view.PhaseVerifying:
		return statusVerifying
	case view.PhaseRepairing:
		return statusRepairing
	case view.PhaseUnpacking:
		return statusExtracting
	case view.PhaseFinishing:
		return statusMoving
	case view.PhaseReady:
		return statusCompleted
	case view.PhaseFailed:
		return statusFailed
	}
	return statusQueued
}

// nzoPrefix starts every nzo_id.
const nzoPrefix = "SABnzbd_nzo_"

func nzoID(id core.ID) string { return nzoPrefix + string(id) }

// parseNzoID extracts the Download ID from an nzo_id. IDs that do not have
// the prefix or the shape of an ID are not ours.
func parseNzoID(s string) (core.ID, bool) {
	id, ok := strings.CutPrefix(strings.TrimSpace(s), nzoPrefix)
	if !ok || len(id) == 0 || len(id) > 64 {
		return "", false
	}
	for i := range len(id) {
		if c := id[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	return core.ID(id), true
}

// Priorities, as the add and queue-priority calls send them.
const (
	sabPriorityDefault = -100
	sabPriorityPaused  = -2
	sabPriorityLow     = -1
	sabPriorityNormal  = 0
	sabPriorityHigh    = 1
	sabPriorityForce   = 2
)

// priorityFromSAB maps SAB's numeric priority to a Download Priority. -100
// ("default") and anything unknown is normal. paused is true for SAB's
// "paused" priority (-2), which adds the Download in the paused state.
func priorityFromSAB(n int) (p core.DownloadPriority, paused bool) {
	switch n {
	case sabPriorityForce:
		return core.DownloadPriorityForce, false
	case sabPriorityHigh:
		return core.DownloadPriorityHigh, false
	case sabPriorityLow:
		return core.DownloadPriorityLow, false
	case sabPriorityPaused:
		return core.DownloadPriorityNormal, true
	}
	return core.DownloadPriorityNormal, false
}

// priorityToSAB maps a Download Priority to SAB's numeric priority.
func priorityToSAB(p core.DownloadPriority) int {
	switch p {
	case core.DownloadPriorityForce:
		return sabPriorityForce
	case core.DownloadPriorityHigh:
		return sabPriorityHigh
	case core.DownloadPriorityLow:
		return sabPriorityLow
	}
	return sabPriorityNormal
}

// parsePriority reads the priority parameter: SAB's number, or a name.
// Empty and unparsable values are the default (normal).
func parsePriority(s string) (core.DownloadPriority, bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return priorityFromSAB(n)
	}
	switch strings.ToLower(s) {
	case "force":
		return core.DownloadPriorityForce, false
	case "high":
		return core.DownloadPriorityHigh, false
	case "low":
		return core.DownloadPriorityLow, false
	case "paused":
		return core.DownloadPriorityNormal, true
	}
	return core.DownloadPriorityNormal, false
}

// failureText is the human-readable fail_message of a Failed Download. The
// wording follows SABnzbd's, which media managers show to the user (and a few
// of them look for), followed by the View's cleaned detail.
func failureText(f view.Failure) string {
	var base string
	switch f.Reason {
	case core.FailureMissingArticles:
		base = "Aborted, cannot be completed - articles are missing and no repair data can recover them"
	case core.FailureRepairInsufficient:
		base = "Repair failed, not enough repair blocks"
	case core.FailureArchiveUnsupported:
		base = "Unpacking failed, unsupported archive format"
	case core.FailureArchivePassword:
		base = "Unpacking failed, archive requires a password"
	case core.FailureArchiveCorrupt:
		base = "Unpacking failed, archive is damaged"
	case core.FailureDiskFull:
		base = "Aborted, not enough free disk space"
	case core.FailurePathViolation:
		base = "Aborted, the download contains an unsafe file path"
	case core.FailureSourceInvalid:
		base = "Aborted, the NZB cannot be used"
	default:
		base = "Download failed"
	}
	if f.Detail == "" {
		return base
	}
	return base + ": " + f.Detail
}

// stageText is the action_line of a history item in Post-processing.
func stageText(status string) string {
	switch status {
	case statusVerifying, statusRepairing, statusExtracting, statusMoving:
		return status
	}
	return ""
}

// Formatting helpers: SAB shows sizes as "%.2f" megabytes in the machine
// fields and as humanised strings in the display fields.

const mib = 1 << 20

func formatMB(n int64) string { return fmt.Sprintf("%.2f", float64(max(n, 0))/mib) }

// humanBytes renders n like SAB's size strings: "0 B", "512.0 KB", "1.2 GB".
func humanBytes(n int64) string {
	f := float64(max(n, 0))
	if f < 1024 {
		return fmt.Sprintf("%.0f B", f)
	}
	for _, unit := range []string{"KB", "MB", "GB", "TB"} {
		f /= 1024
		if f < 1024 || unit == "TB" {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return ""
}

// humanSpeed renders a transfer rate like SAB's speed field: "1.3 M".
func humanSpeed(bytesPerSec int64) string {
	f := float64(max(bytesPerSec, 0))
	if f < 1024 {
		return fmt.Sprintf("%.0f  ", f)
	}
	for _, unit := range []string{"K", "M", "G"} {
		f /= 1024
		if f < 1024 || unit == "G" {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return ""
}

// formatTimeLeft renders seconds as SAB does: "H:MM:SS", or "D:HH:MM:SS"
// from one day on. Both parse as a .NET TimeSpan.
func formatTimeLeft(sec int64) string {
	sec = max(sec, 0)
	d, h, m, s := sec/86400, sec/3600%24, sec/60%60, sec%60
	if d > 0 {
		return fmt.Sprintf("%d:%02d:%02d:%02d", d, h, m, s)
	}
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}

// etaText is a View's time left, "0:00:00" when it is not known.
func etaText(v view.View) string {
	if !v.ETAKnown {
		return formatTimeLeft(0)
	}
	return formatTimeLeft(int64(v.ETA / time.Second))
}

// percentage is a View's progress as a whole percentage, rounded down, as a
// string. It is worked out from the View's done and total bytes in integers,
// so a third is "33" and nothing short of all of it is "100".
func percentage(v view.View) string {
	if v.Total <= 0 || v.Done <= 0 {
		return "0"
	}
	return strconv.FormatInt(min(v.Done*100/v.Total, 100), 10)
}
