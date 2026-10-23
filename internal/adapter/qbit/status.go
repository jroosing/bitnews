package qbit

import (
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/view"
)

// holdOffProgress is the progress of a Download that is Fetched or being
// Post-processed. Media managers import a torrent when it looks complete, and
// bitnews has not finished: the files may still be unpacked. 0.9999 keeps
// every media manager from treating it as complete; the state is checkingUP,
// which they show as queued or checking.
const holdOffProgress = 0.9999

// The qBittorrent states the adapter reports.
const (
	stateError       = "error"
	stateMetaDL      = "metaDL"
	stateQueuedDL    = "queuedDL"
	statePausedDL    = "pausedDL"
	stateStoppedDL   = "stoppedDL"
	stateDownloading = "downloading"
	stateStalledDL   = "stalledDL"
	stateForcedDL    = "forcedDL"
	stateCheckingUP  = "checkingUP"
	stateUploading   = "uploading"
	stateStalledUP   = "stalledUP"
	statePausedUP    = "pausedUP"
	stateStoppedUP   = "stoppedUP"
	stateForcedUP    = "forcedUP"
	stateUnknown     = "unknown"
)

// torrentState maps the view of a torrent Download to qBittorrent's state
// and progress.
//
//	Added, Queued                      queuedDL (pausedDL if paused, forcedDL if force)
//	FetchingMetadata                   metaDL (pausedDL if paused)
//	Fetching                           downloading, stalledDL when no data arrives,
//	                                   pausedDL if paused, forcedDL if force
//	Fetched, PostProcessing            checkingUP, progress 0.9999 (the hold-off)
//	Ready                              uploading, stalledUP when idle, pausedUP if
//	                                   the engine paused it (Share Limit reached),
//	                                   forcedUP if force; progress 1
//	Failed                             error
//	anything else                      unknown
//
// The states that say "finished downloading" (uploading, stalledUP, pausedUP,
// stoppedUP, forcedUP, queuedUP) and a progress of 1 are produced for Ready
// only: the view's progress is capped at 0.9999 below Ready. The
// Post-processing stage makes no difference: every stage is checkingUP.
// stopped selects the names of web API 2.11 (stoppedDL, stoppedUP) over the
// older ones (pausedDL, pausedUP).
func torrentState(v view.View, stopped bool) (state string, progress float64) {
	pausedDL, pausedUP := statePausedDL, statePausedUP
	if stopped {
		pausedDL, pausedUP = stateStoppedDL, stateStoppedUP
	}
	d := v.Download
	paused := d.Paused || v.Torrent != nil && v.Torrent.LimitPaused
	force := d.Priority == core.DownloadPriorityForce
	progress = min(v.Progress, holdOffProgress)
	if !known(d.State) {
		return stateUnknown, progress
	}

	switch v.Phase {
	case view.PhaseWaiting, view.PhasePaused:
		switch {
		case paused:
			return pausedDL, progress
		case force:
			return stateForcedDL, progress
		}
		return stateQueuedDL, progress
	case view.PhaseMetadata:
		if paused {
			return pausedDL, 0
		}
		return stateMetaDL, 0
	case view.PhaseDownloading:
		switch {
		case paused:
			return pausedDL, progress
		case force:
			return stateForcedDL, progress
		case v.DownRate > 0:
			return stateDownloading, progress
		}
		return stateStalledDL, progress
	case view.PhaseVerifying, view.PhaseRepairing, view.PhaseUnpacking, view.PhaseFinishing:
		return stateCheckingUP, holdOffProgress
	case view.PhaseReady:
		switch {
		case paused:
			return pausedUP, 1
		case force:
			return stateForcedUP, 1
		case v.UpRate > 0:
			return stateUploading, 1
		}
		return stateStalledUP, 1
	case view.PhaseFailed:
		return stateError, progress
	}
	return stateUnknown, progress
}

// known says whether s is a State of the lifecycle; the view takes any other
// for waiting, qBittorrent's answer is unknown.
func known(s core.State) bool {
	switch s {
	case core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching,
		core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed:
		return true
	}
	return false
}

// isCompleteState says whether a state is one a media manager takes for
// finished downloading.
func isCompleteState(s string) bool {
	switch s {
	case stateUploading, stateStalledUP, statePausedUP, stateStoppedUP, stateForcedUP, "queuedUP":
		return true
	}
	return false
}

// hashes

// hashList is the parsed hashes parameter.
type hashList struct {
	// all means "all".
	all bool
	// given means the parameter was there and not empty.
	given bool
	// hashes are the valid, distinct, lowercase hashes.
	hashes []string
}

// parseHashes reads the hashes parameter: "all", or hashes separated by "|".
// A hash that is not 40 hex characters (a 64 character v2 hash is truncated)
// is dropped. More than maxHashes entries is ok == false.
func parseHashes(s string) (hashList, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return hashList{}, true
	}
	if strings.EqualFold(s, "all") {
		return hashList{all: true, given: true}, true
	}
	l := hashList{given: true}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '|' || r == ',' })
	if len(parts) > maxHashes {
		return hashList{}, false
	}
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		h, err := core.ParseInfoHash(strings.TrimSpace(p))
		if err != nil {
			continue
		}
		if hs := h.String(); !seen[hs] {
			seen[hs] = true
			l.hashes = append(l.hashes, hs)
		}
	}
	return l, true
}
