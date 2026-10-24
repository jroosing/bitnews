package view

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

// Phase says what a Download is doing, finer than its State only where the
// transports care: a paused Download, and the Post-processing steps.
type Phase int

// The phases.
const (
	// PhaseWaiting is a Download that is added or Queued and not paused.
	PhaseWaiting Phase = iota
	// PhaseMetadata is a magnet link fetching its metadata.
	PhaseMetadata
	// PhasePaused is a Download the user paused, before it is Fetched.
	PhasePaused
	// PhaseDownloading is a Fetching Download.
	PhaseDownloading
	// PhaseVerifying is a Fetched Download, or one in Post-processing whose
	// stage is verifying or not known yet (the first step).
	PhaseVerifying
	PhaseRepairing
	PhaseUnpacking
	PhaseFinishing
	// PhaseReady is a Download whose output can be imported.
	PhaseReady
	PhaseFailed
)

// Live is what the database does not hold about one Download.
type Live struct {
	// Stage is the Post-processing stage; HaveStage says it is known.
	Stage     core.Stage
	HaveStage bool
	// DownRate and UpRate are this Download's transfer rates in bytes per
	// second.
	DownRate, UpRate int64
	// Torrent is the live state of a torrent, nil for a Download that is not
	// one or whose torrent nothing is known about.
	Torrent *TorrentLive
}

// TorrentLive is what is known about one torrent besides its row.
type TorrentLive struct {
	// Hash is the info hash in hex.
	Hash string
	// Registered says the engine has the torrent.
	Registered bool
	// Size and DoneBytes are the engine's numbers; zero Size means unknown
	// and the row's size is used.
	Size, DoneBytes int64
	Peers, Seeds    int
	Private         bool
	// Uploaded and SeedingTime are totals over all runs.
	Uploaded    int64
	SeedingTime time.Duration
	// OwnRatio and OwnSeedingTime are the Download's own Share Limit halves:
	// nil means the global default applies, a negative value is unlimited.
	OwnRatio       *float64
	OwnSeedingTime *time.Duration
	// LimitPaused says a Share Limit paused the torrent.
	LimitPaused bool
}

// LiveSource provides Live for a Download. The daemon implements it.
type LiveSource interface {
	Live(ctx context.Context, d core.Download) Live
}

// Config is what New needs.
type Config struct {
	// Downloads is needed by Queue only.
	Downloads core.DownloadRepo
	// Live is required.
	Live LiveSource
	// Settings returns the current Settings; nil means the defaults.
	Settings func() core.Settings
}

// Module builds Views. It is safe for concurrent use.
type Module struct {
	downloads core.DownloadRepo
	live      LiveSource
	settings  func() core.Settings
}

// New returns a Module.
func New(cfg Config) (*Module, error) {
	if cfg.Live == nil {
		return nil, errors.New("view: Config.Live is required")
	}
	if cfg.Settings == nil {
		cfg.Settings = core.DefaultSettings
	}
	return &Module{downloads: cfg.Downloads, live: cfg.Live, settings: cfg.Settings}, nil
}

// View is one Download as every transport shows it.
type View struct {
	Download core.Download
	Phase    Phase
	// InQueue says the Download is still in a Queue (not Fetched yet).
	InQueue bool
	// Done and Total are bytes; Progress is Done over Total from 0 to 1,
	// and 1 once the Download is Fetched.
	Done, Total int64
	Progress    float64
	// DownRate and UpRate are bytes per second.
	DownRate, UpRate int64
	// ETA is how long the rest takes at the current rate; ETAKnown is false
	// when the Download is not downloading or has no rate.
	ETA      time.Duration
	ETAKnown bool
	// Stage is the Post-processing stage, empty when none is known.
	Stage core.Stage
	// Torrent is set for a torrent that something is known about.
	Torrent *Torrent
	// Failure is set for a Failed Download.
	Failure *Failure
}

// Torrent is the torrent half of a View.
type Torrent struct {
	Hash         string
	Registered   bool
	Peers, Seeds int
	Private      bool
	// Uploaded and SeedingTime are totals over all runs; Ratio is Uploaded
	// over the torrent's size.
	Uploaded    int64
	SeedingTime time.Duration
	Ratio       float64
	ShareLimit  ShareLimit
	// LimitPaused says a Share Limit paused the torrent.
	LimitPaused bool
}

// ShareLimit is the Share Limit in force for a torrent. Global says the
// Download has none of its own. Otherwise Ratio and Seeding are the
// effective values, the global default filling in a half the Download does
// not set; a negative value is unlimited.
type ShareLimit struct {
	Global  bool
	Ratio   float64
	Seeding time.Duration
}

// Failure is why a Download failed, with the detail made safe to show.
type Failure struct {
	Reason core.FailureReason
	Detail string
}

// Of builds the View of d.
func (m *Module) Of(ctx context.Context, d core.Download) View {
	live := m.live.Live(ctx, d)
	v := View{
		Download: d, InQueue: d.State.InQueue(),
		Done: d.DoneBytes, Total: d.TotalBytes,
		DownRate: live.DownRate, UpRate: live.UpRate,
	}
	if live.HaveStage {
		v.Stage = live.Stage
	}
	v.Phase = phaseOf(d, live)
	if t := live.Torrent; t != nil {
		if t.Size > 0 {
			v.Total = t.Size
		}
		if t.DoneBytes > 0 || t.Size > 0 {
			v.Done = t.DoneBytes
		}
		v.Torrent = m.torrentOf(t, v.Total)
	}
	if d.State == core.StateFailed {
		v.Failure = &Failure{Reason: d.FailureReason, Detail: CleanDetail(d.FailureDetail)}
	}
	if !v.InQueue && d.State != core.StateFailed && v.Total > 0 {
		v.Done = v.Total // Fetched or later: all the data is there
	}
	switch {
	case d.State == core.StateFetched || d.State == core.StatePostProcessing || d.State == core.StateReady:
		v.Progress = 1
	case v.Total > 0:
		v.Progress = min(float64(v.Done)/float64(v.Total), 1)
	}
	if v.Phase == PhaseDownloading && v.DownRate > 0 && v.Total > v.Done {
		v.ETA = time.Duration(float64(v.Total-v.Done) / float64(v.DownRate) * float64(time.Second))
		v.ETAKnown = true
	}
	return v
}

func phaseOf(d core.Download, live Live) Phase {
	switch d.State {
	case core.StateAdded, core.StateQueued:
		if d.Paused {
			return PhasePaused
		}
		return PhaseWaiting
	case core.StateFetchingMetadata:
		return PhaseMetadata
	case core.StateFetching:
		if d.Paused {
			return PhasePaused
		}
		return PhaseDownloading
	case core.StatePostProcessing:
		if live.HaveStage {
			switch live.Stage {
			case core.StageRepairing:
				return PhaseRepairing
			case core.StageUnpacking:
				return PhaseUnpacking
			case core.StageFinishing:
				return PhaseFinishing
			}
		}
		return PhaseVerifying
	case core.StateFetched:
		return PhaseVerifying
	case core.StateReady:
		return PhaseReady
	case core.StateFailed:
		return PhaseFailed
	}
	return PhaseWaiting
}

// torrentOf builds the torrent half of a View. size is the torrent's size
// for the ratio.
func (m *Module) torrentOf(t *TorrentLive, size int64) *Torrent {
	out := &Torrent{
		Hash: t.Hash, Registered: t.Registered, Peers: t.Peers, Seeds: t.Seeds, Private: t.Private,
		Uploaded: t.Uploaded, SeedingTime: t.SeedingTime, LimitPaused: t.LimitPaused,
	}
	if size > 0 {
		out.Ratio = float64(t.Uploaded) / float64(size)
	}
	out.ShareLimit = m.shareLimit(t.OwnRatio, t.OwnSeedingTime)
	return out
}

func (m *Module) shareLimit(ratio *float64, seeding *time.Duration) ShareLimit {
	if ratio == nil && seeding == nil {
		return ShareLimit{Global: true}
	}
	l := m.settings().Seeding.ShareLimit()
	if ratio != nil {
		l.Ratio = *ratio
	}
	if seeding != nil {
		l.SeedingTime = *seeding
	}
	out := ShareLimit{Ratio: -1, Seeding: -1}
	if l.Ratio >= 0 {
		out.Ratio = l.Ratio
	}
	if l.SeedingTime >= 0 {
		out.Seeding = l.SeedingTime
	}
	return out
}

// Queue returns the Views of the in-queue Downloads of one Source kind, in
// queue order. It reads every page.
func (m *Module) Queue(ctx context.Context, kind core.SourceKind) ([]View, error) {
	if m.downloads == nil {
		return nil, errors.New("view: Queue needs Config.Downloads")
	}
	var ds []core.Download
	for _, st := range []core.State{core.StateAdded, core.StateFetchingMetadata, core.StateQueued, core.StateFetching} {
		for after := ""; ; {
			page, next, err := m.downloads.List(ctx, core.DownloadQuery{State: st, SourceKind: kind, After: after, Limit: 200})
			if err != nil {
				return nil, fmt.Errorf("view: list %s downloads: %w", st, err)
			}
			ds = append(ds, page...)
			if next == "" {
				break
			}
			after = next
		}
	}
	slices.SortStableFunc(ds, core.CompareQueueOrder)
	out := make([]View, len(ds))
	for i, d := range ds {
		out[i] = m.Of(ctx, d)
	}
	return out, nil
}
