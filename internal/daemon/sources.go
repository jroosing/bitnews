package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/store"
	"gitlab.com/kyura4k/gitnews/internal/usenet/runner"
)

// serverSource adapts core.ServerRepo to runner.ServerSource.
type serverSource struct {
	repo core.ServerRepo
	log  *slog.Logger

	mu     sync.Mutex
	warned map[core.ID]bool
}

func newServerSource(repo core.ServerRepo, log *slog.Logger) *serverSource {
	return &serverSource{repo: repo, log: log, warned: map[core.ID]bool{}}
}

// Servers lists the Servers as specs. A Server whose password cannot be
// decrypted is left out, because dialling it without a password would only
// fail the login or, worse, lock the account: it is logged once, until the
// password is entered again.
func (s *serverSource) Servers(ctx context.Context) ([]runner.ServerSpec, error) {
	servers, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	specs := make([]runner.ServerSpec, 0, len(servers))
	s.mu.Lock()
	defer s.mu.Unlock()
	still := map[core.ID]bool{}
	for _, srv := range servers {
		if srv.PasswordUnavailable {
			still[srv.ID] = true
			if !s.warned[srv.ID] {
				s.log.Warn("usenet server skipped: its stored password cannot be decrypted, enter it again",
					"server", srv.Name)
			}
			continue
		}
		specs = append(specs, runner.ServerSpec{
			Name: srv.Name, Host: srv.Host, Port: srv.Port, User: srv.Username, Pass: srv.Password.Reveal(),
			Priority: srv.Priority, MaxConnections: srv.MaxConnections, RetentionDays: srv.RetentionDays,
			Enabled: srv.Enabled,
		})
	}
	s.warned = still
	return specs, nil
}

// nzbSource adapts core.DownloadSourceRepo to runner.NZBSource.
type nzbSource struct {
	repo core.DownloadSourceRepo
	log  *slog.Logger
}

// Source returns the NZB path and the archive password. A password that
// cannot be decrypted is returned empty with a warning: the fetch does not
// need it, and Unpack then fails with archive_password if the release has
// one.
func (n nzbSource) Source(ctx context.Context, id core.ID) (string, core.Secret, error) {
	path, pw, err := n.repo.Get(ctx, id)
	if errors.Is(err, core.ErrSecretUnavailable) {
		n.log.Warn("download password cannot be decrypted", "id", id)
		return path, "", nil
	}
	return path, pw, err
}

// reports adapts store.UsenetReportRepo to runner.ReportStore.
type reports struct{ repo *store.UsenetReportRepo }

func (r reports) SaveReport(ctx context.Context, id core.ID, d runner.FetchDetail) error {
	return r.repo.SaveReport(ctx, id, store.UsenetReport{
		MissingArticles: d.MissingArticles, MissingBytes: d.MissingBytes,
		DamagedArticles: d.DamagedArticles, DamagedBytes: d.DamagedBytes, MissingFiles: d.MissingFiles,
	})
}

func (r reports) LoadReport(ctx context.Context, id core.ID) (runner.FetchDetail, error) {
	rep, err := r.repo.LoadReport(ctx, id)
	if err != nil {
		return runner.FetchDetail{}, err
	}
	return runner.FetchDetail{
		MissingArticles: rep.MissingArticles, MissingBytes: rep.MissingBytes,
		DamagedArticles: rep.DamagedArticles, DamagedBytes: rep.DamagedBytes, MissingFiles: rep.MissingFiles,
	}, nil
}
