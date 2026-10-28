package qbit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/adapter/adaptertest"
	"github.com/jroosing/bitnews/internal/core"
)

// contractDir holds the replay fixtures. THEY ARE HAND-WRITTEN FROM THE
// QBITTORRENT WEBUI API DOCUMENTATION AND FROM WHAT SONARR AND RADARR ARE
// KNOWN TO PARSE; THEY ARE NOT RECORDED TRAFFIC. The recording issue (M5-09)
// was dropped: differences found in real use are fixed and, where useful,
// added as fixtures. See testdata/README.md.
const contractDir = "testdata/contract"

// seedOp is one entry of a fixture's "seed" list.
type seedOp struct {
	Op string `json:"op"`

	// op "category"
	Name string `json:"name"`

	// op "download": a torrent Download and its torrent.
	Ref      string `json:"ref"`
	State    string `json:"state"`
	Category string `json:"category"`
	Priority string `json:"priority"`
	Paused   bool   `json:"paused"`
	Total    int64  `json:"total"`
	Done     int64  `json:"done"`
	Output   string `json:"output_path"`
	// Engine side. There is no ratio: the view derives it from uploaded
	// and total.
	DownRate    int64   `json:"down_rate"`
	UpRate      int64   `json:"up_rate"`
	Uploaded    int64   `json:"uploaded"`
	SeedingSec  int64   `json:"seeding_s"`
	Seeds       int     `json:"seeds"`
	Peers       int     `json:"peers"`
	SavePath    string  `json:"save_path"`
	ContentPath string  `json:"content_path"`
	Private     bool    `json:"private"`
	ViewPaused  bool    `json:"engine_paused"`
	LimitGlobal *bool   `json:"limit_global"`
	LimitRatio  float64 `json:"limit_ratio"`
	LimitSec    int64   `json:"limit_s"`
	AddedMin    int     `json:"added_min"`
	CompletedMn int     `json:"completed_min"`
	// op "files": the files of Ref; their progress is the torrent's.
	Files []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

// seedFixture creates the state the fixture expects and returns the
// variables it may use: user, pass, torrent_dir, and per seeded download
// hash.<ref> and id.<ref>.
func seedFixture(t *testing.T, s *stack, fx *adaptertest.Fixture) map[string]string {
	t.Helper()
	vars := map[string]string{"user": s.user, "pass": s.pass, "torrent_dir": s.torrents, "incomplete_dir": s.incomplete}
	cats := map[string]core.Category{}
	hashes := map[string]string{}
	for _, raw := range fx.Seed {
		var op seedOp
		raw = []byte(strings.NewReplacer("{{torrent_dir}}", s.torrents, "{{incomplete_dir}}", s.incomplete).Replace(string(raw)))
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		require.NoError(t, dec.Decode(&op), string(raw))
		switch op.Op {
		case "category":
			c, err := s.categories.Create(context.Background(), op.Name, "")
			require.NoError(t, err)
			cats[op.Name] = c
		case "download":
			d, v := s.seed(op.Name, func(d *core.Download, v *torrent) {
				d.State = core.State(op.State)
				if op.Priority != "" {
					d.Priority = core.DownloadPriority(op.Priority)
				}
				d.Paused, d.TotalBytes, d.DoneBytes = op.Paused, op.Total, op.Done
				if op.Category != "" {
					d.CategoryID = cats[op.Category].ID
				}
				if op.AddedMin != 0 {
					d.AddedAt = epoch.Add(time.Duration(op.AddedMin) * time.Minute)
					d.UpdatedAt = d.AddedAt
				}
				switch d.State {
				case core.StateFetched, core.StatePostProcessing, core.StateReady, core.StateFailed:
					d.FetchedAt = d.AddedAt.Add(time.Minute)
					if op.CompletedMn != 0 {
						d.FetchedAt = epoch.Add(time.Duration(op.CompletedMn) * time.Minute)
					}
				}
				if d.State == core.StateReady {
					d.OutputPath, d.ReadyAt = op.Output, d.FetchedAt.Add(time.Minute)
				}
				if d.State == core.StateFailed {
					d.FailureReason = core.FailureArchiveCorrupt
				}
				v.Size, v.DoneBytes = op.Total, op.Done
				v.DownRate, v.UpRate, v.Uploaded = op.DownRate, op.UpRate, op.Uploaded
				v.SeedingTime = time.Duration(op.SeedingSec) * time.Second
				v.Seeds, v.Peers, v.Private, v.Paused = op.Seeds, op.Peers, op.Private, op.ViewPaused
				v.SavePath, v.ContentPath = op.SavePath, op.ContentPath
				if op.LimitGlobal != nil && !*op.LimitGlobal {
					v.OwnRatio, v.OwnSeeding = new(op.LimitRatio), new(time.Duration(op.LimitSec)*time.Second)
				}
			})
			hashes[op.Ref] = v.InfoHash
			vars["hash."+op.Ref] = v.InfoHash
			vars["id."+op.Ref] = string(d.ID)
		case "files":
			var fs []core.TorrentFile
			for _, f := range op.Files {
				fs = append(fs, core.TorrentFile{Path: f.Name, Size: f.Size})
			}
			s.engine.setFiles(hashes[op.Ref], fs...)
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
			t.Parallel()
			s := newStack(t, withSeeding(core.SeedingSettings{RatioLimit: 2, SeedingTimeMinutes: 1440, Action: core.SeedingActionPause}))
			vars := seedFixture(t, s, fx)
			mux := http.NewServeMux()
			mux.Handle("/api/v2/", s.handler)

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

func TestABrokenStateStringFailsTheContractWithAReadableDiff(t *testing.T) {
	t.Parallel()
	fx, err := adaptertest.LoadFile(filepath.Join(contractDir, "torrents_info_import_holdoff.json"))
	require.NoError(t, err)
	s := newStack(t)
	vars := seedFixture(t, s, fx)
	raw, err := os.ReadFile(fx.Path)
	require.NoError(t, err)
	require.Positive(t, strings.Count(string(raw), `"state": "uploading"`))
	broken := strings.Replace(string(raw), `"state": "uploading"`, `"state": "seeding"`, 1)
	path := filepath.Join(t.TempDir(), "broken.json")
	require.NoError(t, os.WriteFile(path, []byte(broken), 0o600))
	fxBroken, err := adaptertest.LoadFile(path)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/api/v2/", s.handler)

	diffs, err := adaptertest.Check(mux, fxBroken, vars)

	require.NoError(t, err)
	require.NotEmpty(t, diffs)
	assert.Contains(t, strings.Join(diffs, "\n"), `.state: want "seeding", got "uploading"`)
}
