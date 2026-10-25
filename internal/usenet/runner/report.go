package runner

import (
	"context"
	"fmt"

	"github.com/jroosing/bitnews/internal/core"
)

// FetchDetail is what a finished fetch knows about the state of a
// Download's Articles. It extends core.FetchReport with sizes.
type FetchDetail = core.FetchDetail

// ReportStore persists FetchDetails so Post-processing still finds them
// after a restart. Implementations are wired later; nil in Config keeps
// them in memory only.
type ReportStore interface {
	SaveReport(ctx context.Context, id core.ID, d FetchDetail) error
	// LoadReport returns an error wrapping core.ErrNotFound when there is
	// no report.
	LoadReport(ctx context.Context, id core.ID) (FetchDetail, error)
}

// Report returns the FetchReport recorded when the Download was Fetched.
// ok is false when the runner has none in memory (a Download Fetched
// before a restart); FetchReport also consults Config.Reports.
func (r *Runner) Report(id core.ID) (core.FetchReport, bool) {
	d, ok := r.Detail(id)
	return d.Report(), ok
}

// Detail is like Report but with the sizes.
func (r *Runner) Detail(id core.ID) (FetchDetail, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.reports[id]
	return d, ok
}

// FetchReport implements core.FetchReporter: the report of FetchDetailOf.
func (r *Runner) FetchReport(ctx context.Context, d core.Download) (core.FetchReport, error) {
	detail, err := r.FetchDetailOf(ctx, d.ID)
	return detail.Report(), err
}

// FetchDetailOf reads the in-memory detail first, then Config.Reports, and
// returns an error wrapping core.ErrNotFound when the Download has none.
func (r *Runner) FetchDetailOf(ctx context.Context, id core.ID) (FetchDetail, error) {
	if detail, ok := r.Detail(id); ok {
		return detail, nil
	}
	if r.cfg.Reports != nil {
		detail, err := r.cfg.Reports.LoadReport(ctx, id)
		if err != nil {
			return FetchDetail{}, fmt.Errorf("runner: fetch report of %s: %w", id, err)
		}
		return detail, nil
	}
	return FetchDetail{}, fmt.Errorf("runner: no fetch report for %s: %w", id, core.ErrNotFound)
}

var _ core.FetchReporter = (*Runner)(nil)
