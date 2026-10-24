package sabnzbd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/adaptertest"
	"github.com/jroosing/bitnews/internal/core"
)

// contractDir holds the replay fixtures. THEY ARE HAND-WRITTEN FROM THE
// SABNZBD API DOCUMENTATION AND FROM WHAT SONARR AND RADARR ARE KNOWN TO
// PARSE; THEY ARE NOT RECORDED TRAFFIC. The recording issue (M4-01) was dropped:
// differences found in real use are fixed and, where useful, added as
// fixtures. See testdata/README.md.
const contractDir = "testdata/contract"

// seedOp is one entry of a fixture's "seed" list.
type seedOp struct {
	Op string `json:"op"`

	// op "category"
	Name        string `json:"name"`
	DirOverride string `json:"dir_override"`

	// op "download"
	Ref           string `json:"ref"`
	State         string `json:"state"`
	Category      string `json:"category"`
	Priority      string `json:"priority"`
	Paused        bool   `json:"paused"`
	Total         int64  `json:"total"`
	Done          int64  `json:"done"`
	OutputPath    string `json:"output_path"`
	FailureReason string `json:"failure_reason"`
	FailureDetail string `json:"failure_detail"`
	Stage         string `json:"stage"`
	// AddedMin is the minutes after the epoch the Download was added at
	// (default: one minute after the previous one). DownloadSec is how long
	// it took to Fetch, PostprocSec how long Post-processing took before
	// it became Ready or Failed.
	AddedMin    int `json:"added_min"`
	DownloadSec int `json:"download_s"`
	PostprocSec int `json:"postproc_s"`

	// op "speed": the global rate and pause flag; op "download": the
	// Download's own rate.
	BytesPerSec int64 `json:"bytes_per_sec"`
	GlobalPause bool  `json:"global_pause"`

	// op "indexer": serves files/<File> at every path
	File string `json:"file"`
}

// seedFixture creates the state the fixture expects and returns the
// variables it may use: apikey, complete_dir, incomplete_dir, indexer, and
// per seeded download nzo.<ref> and id.<ref>.
func seedFixture(t *testing.T, s *stack, fx *adaptertest.Fixture) map[string]string {
	t.Helper()
	vars := map[string]string{
		"apikey": s.key, "complete_dir": s.complete, "incomplete_dir": s.incomplete,
	}
	cats := map[string]core.Category{}
	for _, raw := range fx.Seed {
		var op seedOp
		dec := json.NewDecoder(bytesReader(raw))
		dec.DisallowUnknownFields()
		require.NoError(t, dec.Decode(&op), string(raw))
		switch op.Op {
		case "category":
			c, err := s.categories.Create(context.Background(), op.Name, op.DirOverride)
			require.NoError(t, err)
			cats[op.Name] = c
		case "download":
			d := s.seed(op.Name, func(d *core.Download) {
				d.State = core.State(op.State)
				if op.Priority != "" {
					d.Priority = core.DownloadPriority(op.Priority)
				}
				d.Paused, d.TotalBytes, d.DoneBytes, d.OutputPath = op.Paused, op.Total, op.Done, op.OutputPath
				d.FailureReason, d.FailureDetail = core.FailureReason(op.FailureReason), op.FailureDetail
				if op.Category != "" {
					d.CategoryID = cats[op.Category].ID
				}
				if op.AddedMin != 0 {
					d.AddedAt = epoch.Add(time.Duration(op.AddedMin) * time.Minute)
					d.UpdatedAt = d.AddedAt
				}
				switch d.State {
				case core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed:
					d.FetchedAt = d.AddedAt.Add(time.Duration(op.DownloadSec) * time.Second)
				}
				if d.State == core.StateReady {
					d.ReadyAt = d.FetchedAt.Add(time.Duration(op.PostprocSec) * time.Second)
				}
				if d.State == core.StateFailed {
					d.UpdatedAt = d.FetchedAt.Add(time.Duration(op.PostprocSec) * time.Second)
				}
			})
			if op.Stage != "" {
				s.setStage(d.ID, core.Stage(op.Stage))
			}
			if op.BytesPerSec != 0 {
				s.setRate(d.ID, op.BytesPerSec)
			}
			vars["nzo."+op.Ref] = nzo(d)
			vars["id."+op.Ref] = string(d.ID)
		case "speed":
			s.setSpeed(op.BytesPerSec, op.GlobalPause)
		case "indexer":
			content, err := os.ReadFile(filepath.Join(fx.Dir, op.File))
			require.NoError(t, err)
			ix := newIndexer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-nzb")
				_, _ = w.Write(content)
			})
			vars["indexer"] = ix.URL
		default:
			t.Fatalf("unknown seed op %q", op.Op)
		}
	}
	return vars
}

func TestContractFixturesReplay(t *testing.T) {
	fixtures, err := adaptertest.Load(contractDir)
	require.NoError(t, err)
	require.NotEmpty(t, fixtures, "no fixtures in %s", contractDir)

	for _, fx := range fixtures {
		t.Run(filepath.Base(fx.Path), func(t *testing.T) {
			s := newStack(t)
			vars := seedFixture(t, s, fx)
			mux := http.NewServeMux()
			mux.Handle("/sabnzbd/api", s.handler)

			adaptertest.Run(t, mux, fx, vars)
		})
	}
}

func TestContractFixturesSayWhereTheyComeFrom(t *testing.T) {
	fixtures, err := adaptertest.Load(contractDir)
	require.NoError(t, err)

	for _, fx := range fixtures {
		assert.Regexp(t, `NOT recorded|^recorded from `, fx.Provenance,
			"%s: say \"NOT recorded\" for a hand-written fixture, or \"recorded from <versions>\"", filepath.Base(fx.Path))
	}
}

func TestABrokenStatusStringFailsTheContractWithAReadableDiff(t *testing.T) {
	fx, err := adaptertest.LoadFile(filepath.Join(contractDir, "history_completed.json"))
	require.NoError(t, err)
	s := newStack(t)
	vars := seedFixture(t, s, fx)
	// The adapter says "Completed" for the Ready download; the broken
	// fixture claims "Finished".
	raw, err := os.ReadFile(fx.Path)
	require.NoError(t, err)
	broken := replaceOnce(t, string(raw), `"status": "Completed"`, `"status": "Finished"`)
	path := filepath.Join(t.TempDir(), "broken.json")
	require.NoError(t, os.WriteFile(path, []byte(broken), 0o600))
	fxBroken, err := adaptertest.LoadFile(path)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/sabnzbd/api", s.handler)

	diffs, err := adaptertest.Check(mux, fxBroken, vars)

	require.NoError(t, err)
	require.NotEmpty(t, diffs)
	assert.Contains(t, diffs[0], `$.history.slots[0].status: want "Finished", got "Completed"`)
}

func replaceOnce(t *testing.T, s, old, repl string) string {
	t.Helper()
	require.Equal(t, 1, countOf(s, old), "the fixture text to break")
	return stringsReplace(s, old, repl)
}
