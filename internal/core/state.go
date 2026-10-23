package core

import (
	"errors"
	"fmt"
	"slices"
)

// State is a Download's position in its lifecycle. Pausing is not a State:
// it is the separate Download.Paused flag. Seeding is not a State either: it
// is a torrent attribute that runs in parallel from Fetched onwards. Removal
// deletes the Download instead of moving it to a State.
type State string

// The lifecycle states of a Download.
const (
	StateAdded            State = "added"
	StateFetchingMetadata State = "fetching_metadata" // magnet links only
	StateQueued           State = "queued"
	StateFetching         State = "fetching"
	StateFetched          State = "fetched"
	StatePostProcessing   State = "post_processing"
	StateReady            State = "ready"
	StateFailed           State = "failed"
)

// InQueue reports whether a Download in this State is still in a Queue: it
// has been added but not Fetched yet.
func (s State) InQueue() bool {
	switch s {
	case StateAdded, StateFetchingMetadata, StateQueued, StateFetching:
		return true
	}
	return false
}

// ErrIllegalTransition is returned by Transition for any move the lifecycle
// does not allow. Getting it means there is a bug in the caller.
var ErrIllegalTransition = errors.New("illegal state transition")

// transitions is the single table of legal lifecycle moves. It mirrors the
// "Download lifecycle" diagram in docs/architecture.md together with the
// moves the M3 and M5 designs add:
//
//	Added            -> Queued            NZB or .torrent enters its Queue
//	Added            -> FetchingMetadata  magnet link (M5)
//	FetchingMetadata -> Queued            metadata received (M5)
//	FetchingMetadata -> Failed            metadata timeout, source_invalid (M5)
//	Queued           -> Fetching          Queue activates the Download
//	Queued           -> Failed            fails before activation
//	Fetching         -> Queued            Queue deactivates it, or restart (M2)
//	Fetching         -> Fetched           all data received
//	Fetching         -> Failed
//	Fetched          -> PostProcessing
//	Fetched          -> Ready             torrent without archives (M5)
//	Fetched          -> Failed            torrent move to complete fails (M5)
//	PostProcessing   -> Fetching          deferred recovery volumes (M3-07)
//	PostProcessing   -> Ready
//	PostProcessing   -> Failed
//
// Ready and Failed are terminal. No State may move to itself: a restart
// during Post-processing simply stays in PostProcessing.
var transitions = map[State][]State{
	StateAdded:            {StateQueued, StateFetchingMetadata},
	StateFetchingMetadata: {StateQueued, StateFailed},
	StateQueued:           {StateFetching, StateFailed},
	StateFetching:         {StateQueued, StateFetched, StateFailed},
	StateFetched:          {StatePostProcessing, StateReady, StateFailed},
	StatePostProcessing:   {StateFetching, StateReady, StateFailed},
}

// Transition returns nil if a Download may move from one State to another,
// and an error wrapping ErrIllegalTransition otherwise.
func Transition(from, to State) error {
	if slices.Contains(transitions[from], to) {
		return nil
	}
	return fmt.Errorf("core: %s -> %s: %w", from, to, ErrIllegalTransition)
}
