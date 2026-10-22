package sabnzbd

import "gitlab.com/kyura4k/gitnews/internal/core"

// Classify exposes classify to the table test: it returns whether the
// Download is listed in the queue, and its SAB status.
func Classify(d core.Download, stage core.Stage, haveStage bool) (inQueueList bool, status string) {
	l, s := classify(d, stage, haveStage)
	return l == inQueue, s
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
