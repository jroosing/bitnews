package core

import (
	"fmt"
	"math"
	"time"
)

// SeedingAction is what happens to a torrent when its Share Limit is
// reached.
type SeedingAction string

// The Seeding actions.
const (
	// SeedingActionPause stops Seeding and keeps the torrent and its data.
	// It is the default: media managers see the torrent as finished
	// Seeding and remove it themselves.
	SeedingActionPause SeedingAction = "pause"
	// SeedingActionRemove deletes the Download and keeps its data.
	SeedingActionRemove SeedingAction = "remove"
	// SeedingActionRemoveData deletes the Download and its data.
	SeedingActionRemoveData SeedingAction = "remove_data"
)

// maxSeedingTimeMinutes bounds the default seeding time to about ten years,
// well inside what a time.Duration can hold.
const maxSeedingTimeMinutes = 10 * 365 * 24 * 60

// SeedingSettings are the global defaults for Share Limits. A Download can
// have a Share Limit of its own, which wins. The zero value is the default:
// no limit and the pause action.
//
// By default there is no limit at all, as in qBittorrent: a ratio or time
// that bitnews picked on its own could stop Seeding before a private
// tracker's minimum is met. Media managers set the limits per torrent, and
// users who want a global one set it here.
type SeedingSettings struct {
	// RatioLimit is the default ratio (uploaded bytes divided by the
	// torrent size) after which Seeding stops. Zero or negative means no
	// limit.
	RatioLimit float64 `json:"ratio_limit"`
	// SeedingTimeMinutes is the default time after which Seeding stops.
	// Zero or negative means no limit.
	SeedingTimeMinutes int `json:"seeding_time_minutes"`
	// Action is taken when either limit is reached. Empty means pause.
	Action SeedingAction `json:"action"`
}

// DefaultSeedingSettings returns the Seeding defaults: no limit, pause.
func DefaultSeedingSettings() SeedingSettings {
	return SeedingSettings{Action: SeedingActionPause}
}

// ShareLimit returns the defaults as a ShareLimit, with -1 for a limit that
// is not set.
func (s SeedingSettings) ShareLimit() ShareLimit {
	l := UnlimitedShareLimit()
	if s.RatioLimit > 0 {
		l.Ratio = s.RatioLimit
	}
	if s.SeedingTimeMinutes > 0 {
		l.SeedingTime = time.Duration(s.SeedingTimeMinutes) * time.Minute
	}
	return l
}

// EffectiveAction returns the action, with the empty value read as pause.
func (s SeedingSettings) EffectiveAction() SeedingAction {
	if s.Action == "" {
		return SeedingActionPause
	}
	return s.Action
}

// validate lists the problems with s.
func (s SeedingSettings) validate() []error {
	var problems []error
	switch s.Action {
	case "", SeedingActionPause, SeedingActionRemove, SeedingActionRemoveData:
	default:
		problems = append(problems, fmt.Errorf("seeding action %q is not pause, remove or remove_data", s.Action))
	}
	if math.IsNaN(s.RatioLimit) || math.IsInf(s.RatioLimit, 0) {
		problems = append(problems, fmt.Errorf("seeding ratio limit %v is not a number", s.RatioLimit))
	}
	if s.SeedingTimeMinutes > maxSeedingTimeMinutes {
		problems = append(problems, fmt.Errorf("seeding time %d minutes is more than %d", s.SeedingTimeMinutes, maxSeedingTimeMinutes))
	}
	return problems
}
