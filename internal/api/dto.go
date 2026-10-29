package api

import (
	"time"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/core"
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

// categoryJSON is how a Category looks in responses. A folder override is
// null when unset.
type categoryJSON struct {
	ID         core.ID `json:"id"`
	Name       string  `json:"name"`
	UsenetDir  *string `json:"usenet_dir"`
	TorrentDir *string `json:"torrent_dir"`
}

func toCategoryJSON(c core.Category) categoryJSON {
	orNull := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	return categoryJSON{ID: c.ID, Name: c.Name, UsenetDir: orNull(c.UsenetDir), TorrentDir: orNull(c.TorrentDir)}
}

// serverJSON is how a Server looks in responses. The password is never part
// of it: has_password says whether one is stored, and password_unavailable
// that it is stored but cannot be decrypted, so the user must enter it
// again. There is deliberately no TLS field: TLS is always on and verified.
type serverJSON struct {
	ID                  core.ID   `json:"id"`
	Name                string    `json:"name"`
	Host                string    `json:"host"`
	Port                int       `json:"port"`
	Username            string    `json:"username"`
	HasPassword         bool      `json:"has_password"`
	PasswordUnavailable bool      `json:"password_unavailable"`
	MaxConnections      int       `json:"max_connections"`
	RetentionDays       int       `json:"retention_days"`
	Priority            int       `json:"priority"`
	Enabled             bool      `json:"enabled"`
	CreatedAt           time.Time `json:"created_at"`
}

func toServerJSON(s core.Server) serverJSON {
	return serverJSON{
		ID: s.ID, Name: s.Name, Host: s.Host, Port: s.Port, Username: s.Username,
		HasPassword:         s.Password != "" || s.PasswordUnavailable,
		PasswordUnavailable: s.PasswordUnavailable,
		MaxConnections:      s.MaxConnections, RetentionDays: s.RetentionDays, Priority: s.Priority,
		Enabled: s.Enabled, CreatedAt: s.CreatedAt,
	}
}

// probeJSON is the result of a connection test. It is sent with status 200
// even when the test failed: the request worked, the Server did not.
type probeJSON struct {
	OK        bool   `json:"ok"`
	Class     string `json:"class"`
	Detail    string `json:"detail"`
	Message   string `json:"message"`
	ConnectMS int64  `json:"connect_ms"`
	LatencyMS int64  `json:"latency_ms"`
}

func toProbeJSON(r app.ProbeResult) probeJSON {
	return probeJSON{
		OK: r.OK, Class: string(r.Class), Detail: r.Detail, Message: r.Message,
		ConnectMS: r.ConnectTime.Milliseconds(), LatencyMS: r.Latency.Milliseconds(),
	}
}
