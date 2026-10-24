package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jroosing/bitnews/internal/app"
	"github.com/jroosing/bitnews/internal/auth"
	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/view"
)

// uploadLimits are the paths whose request bodies may be larger than a
// normal form post, and by how much (see secure).
var uploadLimits = map[string]int64{"/downloads/add": addBodyLimit}

// registerDownloadRoutes adds the routes for the Download detail page, the
// removal and share limit actions, and the "Add download" dialog.
func (s *Server) registerDownloadRoutes() {
	s.handleAuthed("GET /downloads/add", s.getAdd)
	s.handleAuthed("POST /downloads/add", s.postAdd)
	s.handleAuthed("GET /downloads/{id}", s.getDetail)
	s.handleAuthed("GET /downloads/{id}/live", s.getDetailLive)
	s.handleAuthed("POST /downloads/{id}/delete", s.postDelete)
	s.handleAuthed("POST /downloads/{id}/share-limit", s.postShareLimit)
	// Not a page of its own: lets a post-login redirect lead to a detail page.
	s.pages = append(s.pages, Page{Path: "/downloads"})
}

// downloadIDRE is the shape of a core.ID: 128 random bits in lower-case
// base32 without padding. IDs from URLs are checked against it before any
// service sees them.
var downloadIDRE = regexp.MustCompile(`^[a-z2-7]{26}$`)

func validDownloadID(id string) bool { return downloadIDRE.MatchString(id) }

// detailMaxFiles caps the file list of a torrent on the page.
const detailMaxFiles = 500

// detailFile is one row of the Files tab.
type detailFile struct {
	Path string
	Size string
}

// detailReport is the Article report of a Usenet fetch.
type detailReport struct {
	MissingArticles int
	MissingBytes    string
	DamagedArticles int
	DamagedBytes    string
	MissingFiles    int
	Clean           bool
}

// detailTorrent is the torrent facts of the page.
type detailTorrent struct {
	InfoHash     string
	Registered   bool
	Private      bool
	LimitPaused  bool
	Peers, Seeds int
	DownRate     string
	UpRate       string
	Uploaded     string
	Ratio        string
	SeedingTime  string
	FileCount    int
	ShowFiles    []detailFile
	MoreFiles    int
	LimitSummary string
	Form         shareForm
}

// shareForm is the share limit form with its values and problems.
type shareForm struct {
	Custom  bool
	Ratio   string
	Minutes string
	Error   string
	// RatioError and MinutesError are field-level problems.
	RatioError, MinutesError string
}

// detailPage is what detail.html and its live fragment render.
type detailPage struct {
	ID         string
	Name       string
	Kind       string
	KindLabel  string
	KindIcon   string
	State      string
	StateText  string
	Badge      string
	BadgeLabel string
	Terminal   bool
	Ready      bool
	Failed     bool
	Paused     bool
	Stage      string
	Category   string
	CategoryID string
	Priority   string
	Position   int

	Size       string
	Done       string
	Percent    int
	Added      whenView
	Fetched    whenView
	ReadyAt    whenView
	Updated    whenView
	OutputPath string
	Speed      string
	// ETA is the time left while the Download is downloading.
	ETA string

	Failure       failureInfo
	FailureCode   string
	FailureDetail string

	Report  *detailReport
	Torrent *detailTorrent
	// TorrentsUnavailable says the page is for a torrent but the daemon
	// reported no torrent facts.
	TorrentsUnavailable bool

	LiveHref string
	// Removed is set by the live fragment when the Download is gone.
	Removed bool
	// CSRF is the token for the plain forms inside the fragment.
	CSRF string
}

// getDetail renders the whole detail page.
func (s *Server) getDetail(w http.ResponseWriter, r *http.Request) {
	d, ok := s.detailDownload(w, r)
	if !ok {
		return
	}
	page, err := s.buildDetail(r.Context(), d, nil)
	if err != nil {
		s.Logger.Error("download detail failed", "id", d.ID, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load this download. Details are in the server log.")
		return
	}
	s.renderDetail(w, r, http.StatusOK, page)
}

func (s *Server) renderDetail(w http.ResponseWriter, r *http.Request, status int, page detailPage) {
	v := s.newView(r, histClean(page.Name, 80))
	page.CSRF = v.CSRF
	v.Page = page
	s.render(w, r, status, "detail.html", "layout_app", v)
}

// getDetailLive is the polled part of the page: the status, progress and
// facts. It carries the polling trigger only while the Download is not in a
// terminal state, so polling stops by itself.
func (s *Server) getDetailLive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validDownloadID(id) {
		s.renderError(w, r, http.StatusNotFound, "Download not found", "There is no download with this address.")
		return
	}
	if !s.detailServices(w, r) {
		return
	}
	d, err := s.Downloads.Get(r.Context(), core.ID(id))
	if errors.Is(err, core.ErrNotFound) {
		// Removed while the page was open: an ordinary answer (not an error
		// toast) that ends the polling.
		v := s.newView(r, "")
		v.Page = detailPage{ID: id, Removed: true}
		s.renderFragment(w, r, http.StatusOK, "detail.html", "detail_live", v)
		return
	}
	if err != nil {
		s.Logger.Error("download detail failed", "id", id, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load this download. Details are in the server log.")
		return
	}
	page, err := s.buildDetail(r.Context(), d, nil)
	if err != nil {
		s.Logger.Error("download detail failed", "id", id, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load this download. Details are in the server log.")
		return
	}
	v := s.newView(r, "")
	page.CSRF = v.CSRF
	v.Page = page
	s.renderFragment(w, r, http.StatusOK, "detail.html", "detail_live", v)
}

// detailServices answers 503 when the Download service is missing.
func (s *Server) detailServices(w http.ResponseWriter, r *http.Request) bool {
	if s.Downloads == nil {
		s.renderError(w, r, http.StatusServiceUnavailable, "Not available", "This page needs the download service, which is not running.")
		return false
	}
	return true
}

// detailDownload validates the ID of the URL and loads the Download,
// answering 404 itself.
func (s *Server) detailDownload(w http.ResponseWriter, r *http.Request) (core.Download, bool) {
	id := r.PathValue("id")
	if !validDownloadID(id) {
		s.renderError(w, r, http.StatusNotFound, "Download not found", "There is no download with this address.")
		return core.Download{}, false
	}
	if !s.detailServices(w, r) {
		return core.Download{}, false
	}
	d, err := s.Downloads.Get(r.Context(), core.ID(id))
	switch {
	case errors.Is(err, core.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Download not found", "It may have been removed. You can find the others in the history.")
		return core.Download{}, false
	case err != nil:
		s.Logger.Error("download detail failed", "id", id, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load this download. Details are in the server log.")
		return core.Download{}, false
	}
	return d, true
}

// buildDetail assembles the page data of d from its View. form, when set,
// replaces the share limit form's values (to show the problems of a rejected
// submission).
func (s *Server) buildDetail(ctx context.Context, d core.Download, form *shareForm) (detailPage, error) {
	v := s.View.Of(ctx, d)
	badge, badgeLabel := badgeOf(v)
	kind, icon := kindLabel(d.SourceKind)
	now := time.Now()
	p := detailPage{
		ID: string(d.ID), Name: histClean(d.Name, 255), Kind: string(d.SourceKind), KindLabel: kind, KindIcon: icon,
		State: string(d.State), StateText: stateText(d), Badge: badge, BadgeLabel: badgeLabel,
		Terminal: terminal(d.State), Ready: d.State == core.StateReady, Failed: d.State == core.StateFailed,
		Paused: d.Paused, Priority: string(d.Priority), Position: d.QueuePosition,
		Size: histBytes(v.Total), Done: histBytes(v.Done), Percent: progressPercent(v),
		Added: histWhen(now, d.AddedAt), Fetched: histWhen(now, d.FetchedAt), ReadyAt: histWhen(now, d.ReadyAt),
		Updated: histWhen(now, d.UpdatedAt), OutputPath: histClean(d.OutputPath, 1024),
		LiveHref: "/downloads/" + string(d.ID) + "/live",
	}
	if p.Name == "" {
		p.Name = "(unnamed)"
	}
	if v.ETAKnown {
		p.ETA = formatETA(v.ETA)
	}
	if d.CategoryID != "" {
		p.CategoryID = string(d.CategoryID)
		if s.Categories != nil {
			if cats, err := s.Categories.List(ctx); err == nil {
				for _, c := range cats {
					if c.ID == d.CategoryID {
						p.Category = histClean(c.Name, 100)
					}
				}
			}
		}
	}
	if v.Failure != nil {
		p.Failure = failureOf(v.Failure.Reason)
		p.FailureCode = histClean(string(v.Failure.Reason), 40)
		p.FailureDetail = failureDetail(v)
	}
	if d.State == core.StatePostProcessing && v.Stage != "" {
		p.Stage = v.Phase.Label()
	}

	switch d.SourceKind {
	case core.SourceNZB:
		if s.Detail != nil && (!d.FetchedAt.IsZero() || p.Failed || d.State == core.StatePostProcessing) {
			if rep, ok := s.Detail.FetchReport(ctx, d.ID); ok {
				p.Report = &detailReport{
					MissingArticles: rep.MissingArticles, MissingBytes: histBytes(rep.MissingBytes),
					DamagedArticles: rep.DamagedArticles, DamagedBytes: histBytes(rep.DamagedBytes),
					MissingFiles: rep.MissingFiles,
					Clean:        rep.MissingArticles == 0 && rep.DamagedArticles == 0 && rep.MissingFiles == 0,
				}
			}
		}
	case core.SourceTorrent:
		if s.Torrents == nil || v.Torrent == nil {
			p.TorrentsUnavailable = true
			break
		}
		files, err := s.Torrents.Files(ctx, d.ID)
		if err != nil {
			return detailPage{}, fmt.Errorf("torrent files: %w", err)
		}
		p.Torrent = s.torrentView(v, files, form)
		if v.Torrent.Registered && !p.Terminal {
			p.Speed = histRate(v.DownRate)
		}
	}
	return p, nil
}

func stateText(d core.Download) string {
	switch d.State {
	case core.StateAdded:
		return "Added"
	case core.StateFetchingMetadata:
		return "Fetching the torrent metadata"
	case core.StateQueued:
		return "Waiting in the queue"
	case core.StateFetching:
		return "Downloading"
	case core.StateFetched:
		return "Downloaded, waiting for processing"
	case core.StatePostProcessing:
		return "Processing"
	case core.StateReady:
		return "Ready"
	case core.StateFailed:
		return "Failed"
	}
	return string(d.State)
}

// torrentView is the torrent part of the page: the facts of the View's
// torrent half, the files and the Share Limit form.
func (s *Server) torrentView(v view.View, files []TorrentFile, form *shareForm) *detailTorrent {
	f := v.Torrent
	t := &detailTorrent{
		InfoHash: histClean(f.Hash, 64), Registered: f.Registered, Private: f.Private, LimitPaused: f.LimitPaused,
		Peers: f.Peers, Seeds: f.Seeds, DownRate: histRate(v.DownRate), UpRate: histRate(v.UpRate),
		Uploaded: histBytes(f.Uploaded), Ratio: strconv.FormatFloat(f.Ratio, 'f', 2, 64), SeedingTime: histDuration(f.SeedingTime),
		FileCount: len(files),
	}
	for _, file := range files[:min(len(files), detailMaxFiles)] {
		t.ShowFiles = append(t.ShowFiles, detailFile{Path: histClean(file.Path, 500), Size: histBytes(file.Size)})
	}
	t.MoreFiles = max(len(files)-detailMaxFiles, 0)

	limit := f.ShareLimit
	t.LimitSummary = shareLimitSummary(limit, s.Settings.Current().Seeding.ShareLimit())
	if form != nil {
		t.Form = *form
	} else {
		t.Form = shareForm{Custom: !limit.Global}
		if !limit.Global && limit.Ratio >= 0 {
			t.Form.Ratio = strconv.FormatFloat(limit.Ratio, 'f', -1, 64)
		}
		if !limit.Global && limit.Seeding >= 0 {
			t.Form.Minutes = strconv.Itoa(int(limit.Seeding / time.Minute))
		}
	}
	return t
}

// shareLimitSummary says in words which Share Limit a torrent has: its own
// (the View's effective values), or the global default.
func shareLimitSummary(own view.ShareLimit, global core.ShareLimit) string {
	prefix := "Global default: "
	ratio, seeding := global.Ratio, global.SeedingTime
	if !own.Global {
		prefix = "Own limit: "
		ratio, seeding = own.Ratio, own.Seeding
	}
	var parts []string
	if ratio >= 0 {
		parts = append(parts, "ratio "+strconv.FormatFloat(ratio, 'f', -1, 64))
	}
	if seeding >= 0 {
		parts = append(parts, "seed for "+histDuration(seeding))
	}
	if len(parts) == 0 {
		return prefix + "no limit, seed until you stop it"
	}
	return prefix + strings.Join(parts, " or ")
}

// ---- actions -----------------------------------------------------------

// detailTrigger sets HX-Trigger with a toast and optional events, as ASCII
// (browsers read header bytes as Latin-1).
func detailTrigger(w http.ResponseWriter, level, message string, events map[string]any) {
	m := map[string]any{}
	maps.Copy(m, events)
	if message != "" {
		m["toast"] = map[string]string{"level": level, "message": message}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x7f:
			sb.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	w.Header().Set("HX-Trigger", sb.String())
}

// postDelete removes a Download from bitnews, keeping its files unless
// delete_data is "1".
func (s *Server) postDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validDownloadID(id) {
		s.renderError(w, r, http.StatusNotFound, "Download not found", "There is no download with this address.")
		return
	}
	if !s.detailServices(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	deleteData := r.PostForm.Get("delete_data") == "1"
	from := r.PostForm.Get("from")
	d, err := s.Downloads.Get(r.Context(), core.ID(id))
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		s.Logger.Error("remove download failed", "id", id, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not remove this download. Details are in the server log.")
		return
	}
	if errors.Is(err, core.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Download not found", "It was already removed.")
		return
	}
	if err := s.Downloads.Delete(r.Context(), core.ID(id), deleteData); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Download not found", "It was already removed.")
			return
		}
		if ae, ok := errors.AsType[*app.Error](err); ok && errors.Is(err, core.ErrConflict) {
			s.renderError(w, r, http.StatusConflict, "Not removed yet", ae.Message)
			return
		}
		s.Logger.Error("remove download failed", "id", id, "delete_data", deleteData, "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Could not remove it",
			"bitnews kept the download because removing it failed. Details are in the server log.")
		return
	}
	msg := "Removed from the history"
	switch {
	case deleteData:
		msg = "Removed, and its files were deleted"
	case !terminal(d.State):
		msg = "Removed from bitnews"
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/history", http.StatusSeeOther)
		return
	}
	detailTrigger(w, "success", msg, map[string]any{"download-removed": map[string]string{"id": id}})
	if from == "history" {
		// The row is the target of the request: an empty body removes it.
		setHTMLHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("HX-Redirect", "/history")
	w.WriteHeader(http.StatusNoContent)
}

const maxShareMinutes = 10 * 365 * 24 * 60

// parseShareForm reads the share limit form. A problem is returned in form.
func parseShareForm(r *http.Request) (ShareLimitChange, shareForm) {
	form := shareForm{
		Custom:  r.PostForm.Get("mode") == "custom",
		Ratio:   strings.TrimSpace(r.PostForm.Get("ratio")),
		Minutes: strings.TrimSpace(r.PostForm.Get("minutes")),
	}
	form.Ratio, form.Minutes = histClean(form.Ratio, 32), histClean(form.Minutes, 32)
	if !form.Custom {
		return ShareLimitChange{UseDefault: true}, form
	}
	change := ShareLimitChange{Ratio: -1, Minutes: -1}
	if form.Ratio != "" {
		v, err := strconv.ParseFloat(form.Ratio, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100000 {
			form.RatioError = "Enter a ratio of 0 or more, such as 2 or 1.5, or leave it empty for no ratio limit."
		}
		change.Ratio = v
	}
	if form.Minutes != "" {
		v, err := strconv.Atoi(form.Minutes)
		if err != nil || v < 0 || v > maxShareMinutes {
			form.MinutesError = "Enter a whole number of minutes, such as 1440 for a day, or leave it empty for no time limit."
		}
		change.Minutes = v
	}
	return change, form
}

// postShareLimit sets or clears the Share Limit of a torrent.
func (s *Server) postShareLimit(w http.ResponseWriter, r *http.Request) {
	d, ok := s.detailDownload(w, r)
	if !ok {
		return
	}
	if d.SourceKind != core.SourceTorrent || s.Torrents == nil {
		s.renderError(w, r, http.StatusConflict, "Not a torrent", "Only torrents have a share limit.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		return
	}
	change, form := parseShareForm(r)
	if form.RatioError != "" || form.MinutesError != "" {
		form.Error = "The share limit was not saved. Fix the problems below."
		s.shareFormAgain(w, r, d, &form)
		return
	}
	if err := s.Torrents.SetShareLimit(r.Context(), d.ID, change); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			s.renderError(w, r, http.StatusNotFound, "Download not found", "It may have been removed.")
			return
		}
		s.Logger.Error("set share limit failed", "id", d.ID, "err", err)
		form.Error = "bitnews could not save the share limit. Details are in the server log."
		s.shareFormAgain(w, r, d, &form)
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/downloads/"+string(d.ID), http.StatusSeeOther)
		return
	}
	detailTrigger(w, "success", "Share limit saved", nil)
	page, err := s.buildDetail(r.Context(), d, nil)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The share limit was saved, but the page could not be refreshed.")
		return
	}
	s.renderShareCard(w, r, http.StatusOK, page)
}

// shareFormAgain shows the form with its problems (422).
func (s *Server) shareFormAgain(w http.ResponseWriter, r *http.Request, d core.Download, form *shareForm) {
	page, err := s.buildDetail(r.Context(), d, form)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "bitnews could not load this download. Details are in the server log.")
		return
	}
	if !isHTMX(r) {
		s.renderDetail(w, r, http.StatusUnprocessableEntity, page)
		return
	}
	s.renderShareCard(w, r, http.StatusUnprocessableEntity, page)
}

func (s *Server) renderShareCard(w http.ResponseWriter, r *http.Request, status int, page detailPage) {
	v := s.newView(r, "")
	if p, ok := auth.FromContext(r.Context()); ok {
		page.CSRF = p.CSRFToken()
	}
	v.Page = page
	s.renderFragment(w, r, status, "detail.html", "share_card", v)
}
