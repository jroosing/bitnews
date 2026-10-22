package sabnzbd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/redact"
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

// listing says where a Download shows up.
type listing int

const (
	inQueue listing = iota
	inHistory
)

// classify maps a Download to the list it appears in and its SAB status.
// stage is the Post-processing stage it is in, if known (haveStage).
//
//	Added, FetchingMetadata, Queued         queue     Queued (Paused if paused)
//	Fetching                                queue     Downloading (Paused if paused)
//	Fetched                                 history   Verifying
//	PostProcessing verifying                history   Verifying
//	PostProcessing repairing                history   Repairing
//	PostProcessing unpacking                history   Extracting
//	PostProcessing finishing                history   Moving
//	PostProcessing, stage unknown           history   Verifying (the first step)
//	Ready                                   history   Completed
//	Failed                                  history   Failed
//
// Completed is only ever returned for Ready: Fetched data that is not yet
// repaired and unpacked must never be imported. A State this table does not
// know is shown as a history item that is Queued, never as Completed.
func classify(d core.Download, stage core.Stage, haveStage bool) (listing, string) {
	switch d.State {
	case core.StateAdded, core.StateFetchingMetadata, core.StateQueued:
		if d.Paused {
			return inQueue, statusPaused
		}
		return inQueue, statusQueued
	case core.StateFetching:
		if d.Paused {
			return inQueue, statusPaused
		}
		return inQueue, statusDownloading
	case core.StateFetched:
		return inHistory, statusVerifying
	case core.StatePostProcessing:
		if !haveStage {
			return inHistory, statusVerifying
		}
		switch stage {
		case core.StageRepairing:
			return inHistory, statusRepairing
		case core.StageUnpacking:
			return inHistory, statusExtracting
		case core.StageFinishing:
			return inHistory, statusMoving
		}
		return inHistory, statusVerifying
	case core.StateReady:
		return inHistory, statusCompleted
	case core.StateFailed:
		return inHistory, statusFailed
	}
	return inHistory, statusQueued
}

// queueStates and historyStates are the States of the two lists, matching
// classify.
var (
	queueStates   = []core.State{core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching}
	historyStates = []core.State{core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed}
)

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

// priorityName is how queue slots spell the priority.
func priorityName(p core.DownloadPriority) string {
	switch p {
	case core.DownloadPriorityForce:
		return "Force"
	case core.DownloadPriorityHigh:
		return "High"
	case core.DownloadPriorityLow:
		return "Low"
	}
	return "Normal"
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
// of them look for), followed by bitnews's own detail. The detail is already
// redacted by the engines; URLs in it are masked again here, and control
// characters and excess length are dropped.
func failureText(reason core.FailureReason, detail string) string {
	var base string
	switch reason {
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
	detail = cleanDetail(detail)
	if detail == "" {
		return base
	}
	return base + ": " + detail
}

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"']+`)

const maxDetailBytes = 300

func cleanDetail(s string) string {
	s = urlInText.ReplaceAllStringFunc(s, redact.URL)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxDetailBytes {
		s = s[:maxDetailBytes]
		for len(s) > 0 && !isRuneStart(s[len(s)-1]) {
			s = s[:len(s)-1]
		}
		s = strings.ToValidUTF8(s, "") + "..."
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

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

// percentage is the integer percentage as a string.
func percentage(done, total int64) string {
	if total <= 0 || done <= 0 {
		return "0"
	}
	return strconv.FormatInt(min(done*100/total, 100), 10)
}
