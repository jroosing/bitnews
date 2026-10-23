package core

import (
	"cmp"
	"slices"
)

// priorityRank orders Download Priorities, highest first. An unknown
// Priority ranks below low.
func priorityRank(p DownloadPriority) int {
	switch p {
	case DownloadPriorityForce:
		return 0
	case DownloadPriorityHigh:
		return 1
	case DownloadPriorityNormal:
		return 2
	case DownloadPriorityLow:
		return 3
	}
	return 4
}

// Select decides which Downloads of one Queue should be Fetching. The caller
// passes the Downloads of a single Queue, so that the protocol's limit applies
// to them alone.
//
// Only Queued and Fetching Downloads are candidates, and a Download is active
// when it is Fetching. A paused Download is never chosen. Force Downloads are
// always chosen and do not count against limit. The others are ranked by
// Download Priority (high first), then QueuePosition, then AddedAt, then ID,
// and the first limit of them are chosen. A negative limit is treated as 0.
//
// activate lists the chosen Downloads that are still Queued, and deactivate
// the Fetching Downloads that were not chosen, each in rank order. Applying
// both and calling Select again returns two empty lists.
func Select(downloads []Download, limit int) (activate, deactivate []ID) {
	limit = max(limit, 0)
	candidates := slices.DeleteFunc(slices.Clone(downloads), func(d Download) bool {
		return d.State != StateQueued && d.State != StateFetching
	})
	slices.SortFunc(candidates, func(a, b Download) int {
		return cmp.Or(
			cmp.Compare(priorityRank(a.Priority), priorityRank(b.Priority)),
			CompareQueueOrder(a, b),
		)
	})
	for _, d := range candidates {
		chosen := !d.Paused
		if chosen && d.Priority != DownloadPriorityForce {
			chosen = limit > 0
			limit--
		}
		switch {
		case chosen && d.State == StateQueued:
			activate = append(activate, d.ID)
		case !chosen && d.State == StateFetching:
			deactivate = append(deactivate, d.ID)
		}
	}
	return activate, deactivate
}
