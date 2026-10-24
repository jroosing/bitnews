package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jroosing/bitnews/internal/core"
)

var now = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func TestFetchedDownloadCanStartPostProcessing(t *testing.T) {
	d := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StateFetched}

	if err := d.ChangeState(core.StateChange{To: core.StatePostProcessing, At: now}); err != nil {
		t.Fatalf("ChangeState: %v", err)
	}
	if d.State != core.StatePostProcessing {
		t.Errorf("State = %s, want %s", d.State, core.StatePostProcessing)
	}
	if !d.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want %v", d.UpdatedAt, now)
	}
}

func TestReadyDownloadCannotBeFetchedAgain(t *testing.T) {
	before := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StateReady}
	d := before

	err := d.ChangeState(core.StateChange{To: core.StateFetching, At: now})
	if !errors.Is(err, core.ErrIllegalTransition) {
		t.Fatalf("got %v, want ErrIllegalTransition", err)
	}
	if d != before {
		t.Errorf("Download changed by a rejected move: %+v", d)
	}
}

func TestFetchingDownloadRecordsWhenItIsFetched(t *testing.T) {
	d := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StateFetching}

	if err := d.ChangeState(core.StateChange{To: core.StateFetched, At: now}); err != nil {
		t.Fatalf("ChangeState: %v", err)
	}
	if !d.FetchedAt.Equal(now) {
		t.Errorf("FetchedAt = %v, want %v", d.FetchedAt, now)
	}
}

func TestPostProcessedDownloadBecomesReadyWithItsOutputPath(t *testing.T) {
	d := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StatePostProcessing}
	out := "/data/usenet/complete/tv/show.s01e01"

	if err := d.ChangeState(core.StateChange{To: core.StateReady, At: now, OutputPath: out}); err != nil {
		t.Fatalf("ChangeState: %v", err)
	}
	if d.OutputPath != out {
		t.Errorf("OutputPath = %q, want %q", d.OutputPath, out)
	}
	if !d.ReadyAt.Equal(now) {
		t.Errorf("ReadyAt = %v, want %v", d.ReadyAt, now)
	}
}

func TestDownloadCannotBecomeReadyWithoutOutputPath(t *testing.T) {
	before := core.Download{ID: core.NewID(), SourceKind: core.SourceTorrent, State: core.StateFetched}
	d := before

	err := d.ChangeState(core.StateChange{To: core.StateReady, At: now})
	if !errors.Is(err, core.ErrInvalidStateChange) {
		t.Fatalf("got %v, want ErrInvalidStateChange", err)
	}
	if d != before {
		t.Errorf("Download changed by a rejected move: %+v", d)
	}
}

func TestFailedDownloadRecordsWhyItFailed(t *testing.T) {
	d := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StatePostProcessing}

	err := d.ChangeState(core.StateChange{
		To:            core.StateFailed,
		At:            now,
		FailureReason: core.FailureRepairInsufficient,
		FailureDetail: "12 damaged slices, 4 recovery slices",
	})
	if err != nil {
		t.Fatalf("ChangeState: %v", err)
	}
	if d.FailureReason != core.FailureRepairInsufficient {
		t.Errorf("FailureReason = %q, want %q", d.FailureReason, core.FailureRepairInsufficient)
	}
	if d.FailureDetail != "12 damaged slices, 4 recovery slices" {
		t.Errorf("FailureDetail = %q", d.FailureDetail)
	}
}

func TestDownloadCannotFailWithoutAKnownReason(t *testing.T) {
	for _, reason := range []core.FailureReason{"", "cosmic_rays"} {
		before := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StateFetching}
		d := before

		err := d.ChangeState(core.StateChange{To: core.StateFailed, At: now, FailureReason: reason})
		if !errors.Is(err, core.ErrInvalidStateChange) {
			t.Errorf("reason %q: got %v, want ErrInvalidStateChange", reason, err)
		}
		if d != before {
			t.Errorf("reason %q: Download changed by a rejected move: %+v", reason, d)
		}
	}
}

func TestDownloadThatDoesNotFailCannotCarryAFailureReason(t *testing.T) {
	d := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StateQueued}

	err := d.ChangeState(core.StateChange{To: core.StateFetching, At: now, FailureReason: core.FailureInternal})
	if !errors.Is(err, core.ErrInvalidStateChange) {
		t.Fatalf("got %v, want ErrInvalidStateChange", err)
	}
}

func TestDownloadCanFailForEveryDocumentedReason(t *testing.T) {
	reasons := map[core.FailureReason]string{
		core.FailureMissingArticles:    "missing_articles",
		core.FailureRepairInsufficient: "repair_insufficient",
		core.FailureArchiveUnsupported: "archive_unsupported",
		core.FailureArchivePassword:    "archive_password",
		core.FailureArchiveCorrupt:     "archive_corrupt",
		core.FailureDiskFull:           "disk_full",
		core.FailurePathViolation:      "path_violation",
		core.FailureSourceInvalid:      "source_invalid",
		core.FailureInternal:           "internal",
	}
	for reason, wire := range reasons {
		if string(reason) != wire {
			t.Errorf("reason %q, want wire value %q", reason, wire)
		}
		d := core.Download{ID: core.NewID(), SourceKind: core.SourceNZB, State: core.StatePostProcessing}
		if err := d.ChangeState(core.StateChange{To: core.StateFailed, At: now, FailureReason: reason}); err != nil {
			t.Errorf("reason %q: %v", reason, err)
		}
	}
}
