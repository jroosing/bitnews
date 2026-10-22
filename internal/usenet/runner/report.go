package runner

import (
	"context"
	"fmt"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// FetchDetail is what a finished fetch knows about the state of a
// Download's Articles. It extends core.FetchReport with sizes.
type FetchDetail struct {
	// MissingArticles is how many Articles no Server had.
	MissingArticles int
	// MissingBytes is the encoded size, from the NZB, of the Missing
	// Articles. The decoded size is smaller by a few percent.
	MissingBytes int64
	// DamagedArticles is how many Articles were written from a copy that
	// failed its CRC.
	DamagedArticles int
	// DamagedBytes is the decoded size of the damaged ranges.
	DamagedBytes int64
	// MissingFiles counts files none of whose Articles arrived, so the
	// writer never created them.
	MissingFiles int
}

// Report returns the core.FetchReport form of the detail.
func (d FetchDetail) Report() core.FetchReport {
	return core.FetchReport{MissingArticles: d.MissingArticles, DamagedArticles: d.DamagedArticles}
}

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

// FetchReport implements core.FetchReporter. It reads the in-memory report
// first, then Config.Reports, and returns an error wrapping core.ErrNotFound
// when the Download has none.
func (r *Runner) FetchReport(ctx context.Context, d core.Download) (core.FetchReport, error) {
	if detail, ok := r.Detail(d.ID); ok {
		return detail.Report(), nil
	}
	if r.cfg.Reports != nil {
		detail, err := r.cfg.Reports.LoadReport(ctx, d.ID)
		if err != nil {
			return core.FetchReport{}, fmt.Errorf("runner: fetch report of %s: %w", d.ID, err)
		}
		return detail.Report(), nil
	}
	return core.FetchReport{}, fmt.Errorf("runner: no fetch report for %s: %w", d.ID, core.ErrNotFound)
}

var _ core.FetchReporter = (*Runner)(nil)
