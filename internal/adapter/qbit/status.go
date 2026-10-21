package qbit

import (
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/core"
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

// torrentState maps a Download and its torrent to qBittorrent's state and
// progress.
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
// only. The Post-processing stage makes no difference: every stage is
// checkingUP. stopped selects the names of web API 2.11 (stoppedDL,
// stoppedUP) over the older ones (pausedDL, pausedUP).
func torrentState(d core.Download, v TorrentView, stopped bool) (state string, progress float64) {
	pausedDL, pausedUP := statePausedDL, statePausedUP
	if stopped {
		pausedDL, pausedUP = stateStoppedDL, stateStoppedUP
	}
	paused := d.Paused || v.Paused
	force := d.Priority == core.DownloadPriorityForce
	progress = fraction(v, d)

	switch d.State {
	case core.StateAdded, core.StateQueued:
		switch {
		case paused:
			return pausedDL, progress
		case force:
			return stateForcedDL, progress
		}
		return stateQueuedDL, progress
	case core.StateFetchingMetadata:
		if paused {
			return pausedDL, 0
		}
		return stateMetaDL, 0
	case core.StateFetching:
		switch {
		case paused:
			return pausedDL, progress
		case force:
			return stateForcedDL, progress
		case v.DownRate > 0:
			return stateDownloading, progress
		}
		return stateStalledDL, progress
	case core.StateFetched, core.StatePostProcessing:
		return stateCheckingUP, holdOffProgress
	case core.StateReady:
		switch {
		case paused:
			return pausedUP, 1
		case force:
			return stateForcedUP, 1
		case v.UpRate > 0:
			return stateUploading, 1
		}
		return stateStalledUP, 1
	case core.StateFailed:
		return stateError, progress
	}
	return stateUnknown, progress
}

// fraction is the real progress of a Download that is not Ready: below 1,
// whatever the byte counts say, because only Ready is complete.
func fraction(v TorrentView, d core.Download) float64 {
	size, done := v.Size, v.DoneBytes
	if size <= 0 {
		size, done = d.TotalBytes, d.DoneBytes
	}
	if size <= 0 || done <= 0 {
		return 0
	}
	return min(float64(done)/float64(size), holdOffProgress)
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
