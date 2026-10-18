package api

import (
	"time"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// downloadJSON is how a Download looks in responses and in SSE events.
// category_id and the three timestamps after added_at are null when unset;
// failure_reason, failure_detail and output_path are left out when empty.
type downloadJSON struct {
	ID            core.ID               `json:"id"`
	Name          string                `json:"name"`
	SourceKind    core.SourceKind       `json:"source_kind"`
	CategoryID    *core.ID              `json:"category_id"`
	State         core.State            `json:"state"`
	Priority      core.DownloadPriority `json:"priority"`
	QueuePosition int                   `json:"queue_position"`
	Paused        bool                  `json:"paused"`
	FailureReason core.FailureReason    `json:"failure_reason,omitempty"`
	FailureDetail string                `json:"failure_detail,omitempty"`
	TotalBytes    int64                 `json:"total_bytes"`
	DoneBytes     int64                 `json:"done_bytes"`
	AddedAt       time.Time             `json:"added_at"`
	FetchedAt     *time.Time            `json:"fetched_at"`
	ReadyAt       *time.Time            `json:"ready_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
	OutputPath    string                `json:"output_path,omitempty"`
}

func toDownloadJSON(d core.Download) downloadJSON {
	out := downloadJSON{
		ID: d.ID, Name: d.Name, SourceKind: d.SourceKind, State: d.State, Priority: d.Priority,
		QueuePosition: d.QueuePosition, Paused: d.Paused,
		FailureReason: d.FailureReason, FailureDetail: d.FailureDetail,
		TotalBytes: d.TotalBytes, DoneBytes: d.DoneBytes,
		AddedAt: d.AddedAt, FetchedAt: timeOrNil(d.FetchedAt), ReadyAt: timeOrNil(d.ReadyAt),
		UpdatedAt: d.UpdatedAt, OutputPath: d.OutputPath,
	}
	if d.CategoryID != "" {
		id := d.CategoryID
		out.CategoryID = &id
	}
	return out
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// categoryJSON is how a Category looks in responses. dir_override is null
// when unset.
type categoryJSON struct {
	ID          core.ID `json:"id"`
	Name        string  `json:"name"`
	DirOverride *string `json:"dir_override"`
}

func toCategoryJSON(c core.Category) categoryJSON {
	out := categoryJSON{ID: c.ID, Name: c.Name}
	if c.DirOverride != "" {
		dir := c.DirOverride
		out.DirOverride = &dir
	}
	return out
}
