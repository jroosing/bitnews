package sabnzbd

import "gitlab.com/kyura4k/gitnews/internal/view"

// Classify exposes the translation of a View to the table test: whether the
// Download is listed in the queue, and its SAB status.
func Classify(v view.View) (inQueueList bool, sabStatus string) {
	return listingOf(v.Download) == inQueue, status(v)
}

// FailureText exposes failureText.
var FailureText = failureText

// PriorityFromSAB and PriorityToSAB expose the priority mapping.
var (
	PriorityFromSAB = priorityFromSAB
	PriorityToSAB   = priorityToSAB
)

// FormatTimeLeft, HumanBytes and ParseNzoID expose the formatting helpers.
var (
	FormatTimeLeft = formatTimeLeft
	HumanBytes     = humanBytes
	ParseNzoID     = parseNzoID
)
