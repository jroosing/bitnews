package core_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestCleanUsenetDownloadBecomesReady(t *testing.T) {
	h := newPPHarness(t)
	d := h.add("Show.S01E01", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateReady, got.State)
	assert.Equal(t, "/complete/Show.S01E01", got.OutputPath)
	assert.Equal(t, []string{"verify", "detect", "finish"}, h.log.all())
}

func TestEventsAreDownloadUpdatedStagesThenReady(t *testing.T) {
	h := newPPHarness(t)
	h.unpacker.sets = []core.ArchiveSet{{Name: "a.rar"}}
	h.verifier.fn = func(context.Context, string) (core.VerifyOutcome, error) {
		return core.VerifyOutcome{HasPAR2: true, NeedsRepair: true, DamagedSlices: 2, RecoveryOnDisk: 5, Repairable: true}, nil
	}
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	evs := h.events()
	require.Len(t, evs, 7, "%#v", evs)
	assert.Equal(t, core.StatePostProcessing, evs[0].(core.DownloadUpdated).Download.State)
	assert.Equal(t, core.PostProcessStage{ID: d.ID, Stage: core.StageVerifying}, evs[1])
	assert.Equal(t, core.PostProcessStage{ID: d.ID, Stage: core.StageRepairing}, evs[2])
	assert.Equal(t, core.PostProcessStage{ID: d.ID, Stage: core.StageUnpacking}, evs[3])
	assert.Equal(t, core.PostProcessStage{ID: d.ID, Stage: core.StageFinishing}, evs[4])
	assert.Equal(t, core.StateReady, evs[5].(core.DownloadUpdated).Download.State)
	assert.Equal(t, core.Ready{ID: d.ID, OutputPath: "/complete/x"}, evs[6])
	assert.Equal(t, core.KindPostProcessStage, evs[1].Kind())
}

func damaged() func(context.Context, string) (core.VerifyOutcome, error) {
	return func(context.Context, string) (core.VerifyOutcome, error) {
		return core.VerifyOutcome{HasPAR2: true, NeedsRepair: true, DamagedSlices: 3, RecoveryOnDisk: 10, Repairable: true}, nil
	}
}

func TestDamagedDownloadIsRepairedThenReady(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = damaged()
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.StateReady, h.get(d.ID).State)
	assert.Equal(t, []string{"verify", "repair", "detect", "finish"}, h.log.all())
}

func TestFailedRepairFailsTheDownloadAndPublishesFailed(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = damaged()
	h.repairer.fn = func(context.Context, string, core.VerifyOutcome) (core.RepairOutcome, error) {
		return core.RepairOutcome{}, fmt.Errorf("solve: %w", core.ErrRepairFailed)
	}
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateFailed, got.State)
	assert.Equal(t, core.FailureRepairInsufficient, got.FailureReason)
	assert.NotContains(t, h.log.all(), "finish")
	evs := h.events()
	require.GreaterOrEqual(t, len(evs), 2)
	assert.Equal(t, core.StateFailed, evs[len(evs)-2].(core.DownloadUpdated).Download.State)
	f := evs[len(evs)-1].(core.Failed)
	assert.Equal(t, core.Failed{ID: d.ID, Reason: core.FailureRepairInsufficient, Detail: got.FailureDetail}, f)
}

func shortfall() func(context.Context, string) (core.VerifyOutcome, error) {
	return func(context.Context, string) (core.VerifyOutcome, error) {
		return core.VerifyOutcome{HasPAR2: true, NeedsRepair: true, DamagedSlices: 12, RecoveryOnDisk: 5}, nil
	}
}

func TestShortOfRecoveryAsksForDeferredVolumesAndHandsOff(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = shortfall()
	h.deferred.more = true
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, []string{"verify", "deferred"}, h.log.all(), "no repair attempt, no unpack, no finish")
	assert.Equal(t, []int{7}, h.deferred.minBlocks, "the missing blocks: 12 damaged - 5 on disk")
	// The fetch side moves the Download back to Fetching; here the fake does
	// not, so it stays PostProcessing, and nothing failed.
	assert.Equal(t, core.StatePostProcessing, h.get(d.ID).State)
	for _, ev := range h.events() {
		assert.NotEqual(t, core.KindFailed, ev.Kind())
		assert.NotEqual(t, core.KindReady, ev.Kind())
	}
}

func TestShortOfRecoveryWithNoMoreVolumesFailsRepairInsufficient(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = shortfall()
	h.deferred.more = false
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateFailed, got.State)
	assert.Equal(t, core.FailureRepairInsufficient, got.FailureReason)
	assert.Equal(t, []string{"verify", "deferred"}, h.log.all())
}

func TestRepairShortfallErrorAlsoAsksForDeferredVolumes(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = damaged() // the verifier thought it repairable
	h.repairer.fn = func(context.Context, string, core.VerifyOutcome) (core.RepairOutcome, error) {
		return core.RepairOutcome{}, &core.RecoveryShortfall{Needed: 9, Available: 4}
	}
	h.deferred.more = true
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, []int{5}, h.deferred.minBlocks)
	assert.Equal(t, core.StatePostProcessing, h.get(d.ID).State)
	assert.NotContains(t, h.log.all(), "finish")
}

func TestDeferredVolumesErrorFailsInternal(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = shortfall()
	h.deferred.err = errors.New("boom")
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateFailed, got.State)
	assert.Equal(t, core.FailureInternal, got.FailureReason)
}

func TestShortOfRecoveryWithoutDeferredPortIsFinal(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.DeferredVolumes = nil })
	h.verifier.fn = shortfall()
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.FailureRepairInsufficient, h.get(d.ID).FailureReason)
}

func noPAR2() func(context.Context, string) (core.VerifyOutcome, error) {
	return func(context.Context, string) (core.VerifyOutcome, error) {
		return core.VerifyOutcome{HasPAR2: false}, nil
	}
}

func TestNoPAR2WithMissingOrDamagedArticlesFailsMissingArticles(t *testing.T) {
	for name, report := range map[string]core.FetchReport{
		"missing": {MissingArticles: 2},
		"damaged": {DamagedArticles: 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPPHarness(t)
			h.verifier.fn = noPAR2()
			h.reporter.report = report
			d := h.add("x", core.StateFetched, core.SourceNZB)

			require.NoError(t, h.pp.Process(context.Background(), d.ID))

			got := h.get(d.ID)
			assert.Equal(t, core.StateFailed, got.State)
			assert.Equal(t, core.FailureMissingArticles, got.FailureReason)
			assert.Equal(t, []string{"verify", "fetchreport"}, h.log.all())
		})
	}
}

func TestNoPAR2CleanFetchContinuesToReady(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = noPAR2()
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.StateReady, h.get(d.ID).State)
	assert.Equal(t, []string{"verify", "fetchreport", "detect", "finish"}, h.log.all())
}

func TestPAR2CleanIgnoresTheFetchReport(t *testing.T) {
	h := newPPHarness(t)
	h.reporter.report = core.FetchReport{MissingArticles: 5} // PAR2 says the files are fine
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.StateReady, h.get(d.ID).State)
	assert.NotContains(t, h.log.all(), "fetchreport")
}

func TestFetchReportErrorFailsInternal(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = noPAR2()
	h.reporter.err = errors.New("db down")
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.FailureInternal, h.get(d.ID).FailureReason)
}

func TestUnpackRunsOnlyWhenArchivesAreDetected(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		h := newPPHarness(t)
		d := h.add("x", core.StateFetched, core.SourceNZB)

		require.NoError(t, h.pp.Process(context.Background(), d.ID))

		assert.Equal(t, []string{"verify", "detect", "finish"}, h.log.all())
		assert.Empty(t, h.finisher.gotUnpacked)
		assert.Equal(t, "/work/x", h.finisher.gotDir)
	})
	t.Run("sets", func(t *testing.T) {
		h := newPPHarness(t)
		sets := []core.ArchiveSet{{Name: "a.rar", Opaque: 1}, {Name: "b.7z", Opaque: 2}}
		h.unpacker.sets = sets
		d := h.add("x", core.StateFetched, core.SourceNZB)

		require.NoError(t, h.pp.Process(context.Background(), d.ID))

		assert.Equal(t, []string{"verify", "detect", "unpack", "finish"}, h.log.all())
		assert.Equal(t, sets, h.unpacker.gotSets)
		assert.Equal(t, d.ID, h.unpacker.gotDLID)
		assert.Equal(t, "/work/x", h.unpacker.gotDir)
		assert.Equal(t, "/work/x/_unpack", h.unpacker.gotDst)
		assert.Equal(t, "/work/x/_unpack", h.finisher.gotUnpacked)
		assert.Equal(t, core.SourceNZB, h.finisher.gotDownload.SourceKind)
	})
}

func TestEachSentinelMapsToItsFailureReason(t *testing.T) {
	cases := []struct {
		err    error
		reason core.FailureReason
	}{
		{core.ErrInsufficientRecovery, core.FailureRepairInsufficient},
		{&core.RecoveryShortfall{Needed: 3, Available: 1}, core.FailureRepairInsufficient},
		{core.ErrRepairFailed, core.FailureRepairInsufficient},
		{core.ErrMissingArticles, core.FailureMissingArticles},
		{core.ErrArchiveUnsupported, core.FailureArchiveUnsupported},
		{core.ErrArchivePassword, core.FailureArchivePassword},
		{core.ErrArchiveCorrupt, core.FailureArchiveCorrupt},
		{core.ErrPathViolation, core.FailurePathViolation},
		{core.ErrDiskFull, core.FailureDiskFull},
		{fmt.Errorf("wrapped: %w", core.ErrDiskFull), core.FailureDiskFull},
		{errors.New("who knows"), core.FailureInternal},
	}
	// Each sentinel is returned from every port that can return it.
	ports := map[string]func(h *ppHarness, err error){
		"verify": func(h *ppHarness, err error) {
			h.verifier.fn = func(context.Context, string) (core.VerifyOutcome, error) { return core.VerifyOutcome{}, err }
		},
		"repair": func(h *ppHarness, err error) {
			h.verifier.fn = damaged()
			h.deferred.more = false
			h.repairer.fn = func(context.Context, string, core.VerifyOutcome) (core.RepairOutcome, error) {
				return core.RepairOutcome{}, err
			}
		},
		"detect": func(h *ppHarness, err error) {
			h.unpacker.detectFn = func(context.Context, string) ([]core.ArchiveSet, error) { return nil, err }
		},
		"unpack": func(h *ppHarness, err error) {
			h.unpacker.sets = []core.ArchiveSet{{Name: "a.rar"}}
			h.unpacker.unpackFn = func(context.Context, string, string, core.UnpackOptions) (core.UnpackResult, error) {
				return core.UnpackResult{}, err
			}
		},
		"finish": func(h *ppHarness, err error) {
			h.finisher.fn = func(context.Context, core.Download, string, string) (string, error) { return "", err }
		},
	}
	for portName, set := range ports {
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s/%v", portName, c.reason), func(t *testing.T) {
				h := newPPHarness(t)
				set(h, c.err)
				d := h.add("x", core.StateFetched, core.SourceNZB)

				require.NoError(t, h.pp.Process(context.Background(), d.ID))

				got := h.get(d.ID)
				assert.Equal(t, core.StateFailed, got.State)
				assert.Equal(t, c.reason, got.FailureReason)
			})
		}
	}
}

func TestUnknownErrorDetailIsRedactedAndTruncated(t *testing.T) {
	h := newPPHarness(t)
	h.unpacker.sets = []core.ArchiveSet{{Name: "a.rar"}}
	h.unpacker.unpackFn = func(context.Context, string, string, core.UnpackOptions) (core.UnpackResult, error) {
		return core.UnpackResult{}, errors.New(`open /srv/incomplete/Secret.Name/a.rar: permission denied; also "/etc/passwd" and C:\Users\me\x.rar failed`)
	}
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.FailureInternal, got.FailureReason)
	assert.Equal(t, `unpacking: open <dir>: permission denied; also "<dir>" and <dir> failed`, got.FailureDetail)

	h2 := newPPHarness(t)
	h2.verifier.fn = func(context.Context, string) (core.VerifyOutcome, error) {
		return core.VerifyOutcome{}, errors.New(strings.Repeat("é", 500))
	}
	d2 := h2.add("y", core.StateFetched, core.SourceNZB)
	require.NoError(t, h2.pp.Process(context.Background(), d2.ID))
	detail := h2.get(d2.ID).FailureDetail
	assert.LessOrEqual(t, utf8.RuneCountInString(detail), 200)
	assert.True(t, utf8.ValidString(detail))
}

func TestEmptyOutputPathFromFinisherFailsInternal(t *testing.T) {
	h := newPPHarness(t)
	h.finisher.fn = func(context.Context, core.Download, string, string) (string, error) { return "", nil }
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateFailed, got.State)
	assert.Equal(t, core.FailureInternal, got.FailureReason)
}

func TestTorrentWithoutArchivesIsReadyViaFinisherWithoutVerify(t *testing.T) {
	h := newPPHarness(t)
	h.verifier.fn = damaged() // must never be consulted
	d := h.add("Movie", core.StateFetched, core.SourceTorrent)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.StateReady, h.get(d.ID).State)
	assert.Equal(t, []string{"detect", "finish"}, h.log.all())
	assert.Empty(t, h.finisher.gotUnpacked)
	assert.Equal(t, core.SourceTorrent, h.finisher.gotDownload.SourceKind)
	for _, ev := range h.events() {
		if st, ok := ev.(core.PostProcessStage); ok {
			assert.NotEqual(t, core.StageVerifying, st.Stage)
			assert.NotEqual(t, core.StageRepairing, st.Stage)
		}
	}
}

func TestTorrentWithArchivesUnpacksIntoUnpackDirAndPassesItToFinisher(t *testing.T) {
	h := newPPHarness(t)
	h.unpacker.sets = []core.ArchiveSet{{Name: "a.rar"}}
	d := h.add("Movie", core.StateFetched, core.SourceTorrent)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, []string{"detect", "unpack", "finish"}, h.log.all())
	assert.Equal(t, "/work/Movie", h.unpacker.gotDir)
	assert.Equal(t, "/work/Movie/_unpack", h.unpacker.gotDst)
	assert.Equal(t, "/work/Movie/_unpack", h.finisher.gotUnpacked)
	assert.Equal(t, core.StateReady, h.get(d.ID).State)
}

func TestTorrentUnpackFailureFailsWithoutTouchingDataOrFinishing(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "movie.part01.rar")
	require.NoError(t, os.WriteFile(original, []byte("seed me"), 0o600))
	h := newPPHarness(t, func(c *core.PostProcessorConfig) {
		c.DirFor = func(core.Download) string { return dir }
	})
	h.unpacker.sets = []core.ArchiveSet{{Name: "movie"}}
	h.unpacker.unpackFn = func(context.Context, string, string, core.UnpackOptions) (core.UnpackResult, error) {
		return core.UnpackResult{}, core.ErrArchivePassword
	}
	d := h.add("Movie", core.StateFetched, core.SourceTorrent)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateFailed, got.State)
	assert.Equal(t, core.FailureArchivePassword, got.FailureReason)
	assert.NotContains(t, h.log.all(), "finish")
	data, err := os.ReadFile(original)
	require.NoError(t, err)
	assert.Equal(t, "seed me", string(data))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the orchestrator created and removed nothing")
}

func TestCancelledContextMidStageLeavesDownloadPostProcessingAndPublishesNoOutcome(t *testing.T) {
	for name, err := range map[string]error{
		"ctx error":     context.Canceled,
		"wrapped error": errors.New("read: operation was canceled"), // not recognisable as ctx's
	} {
		t.Run(name, func(t *testing.T) {
			h := newPPHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.unpacker.sets = []core.ArchiveSet{{Name: "a.rar"}}
			h.unpacker.unpackFn = func(ctx context.Context, _, _ string, _ core.UnpackOptions) (core.UnpackResult, error) {
				cancel()
				<-ctx.Done()
				return core.UnpackResult{}, err
			}
			d := h.add("x", core.StateFetched, core.SourceNZB)

			got := h.pp.Process(ctx, d.ID)

			require.ErrorIs(t, got, context.Canceled)
			assert.Equal(t, core.StatePostProcessing, h.get(d.ID).State)
			assert.NotContains(t, h.log.all(), "finish")
			for _, ev := range h.events() {
				assert.NotContains(t, []core.EventKind{core.KindReady, core.KindFailed}, ev.Kind())
				if u, ok := ev.(core.DownloadUpdated); ok {
					assert.Equal(t, core.StatePostProcessing, u.Download.State)
				}
			}
		})
	}
}

func TestProcessRejectsIllegalStatesWithoutRunningAnything(t *testing.T) {
	for _, state := range []core.State{core.StateAdded, core.StateQueued, core.StateFetching, core.StatePostProcessing, core.StateReady, core.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			h := newPPHarness(t)
			d := h.add("x", state, core.SourceNZB)

			err := h.pp.Process(context.Background(), d.ID)

			require.ErrorIs(t, err, core.ErrIllegalTransition)
			assert.Equal(t, state, h.get(d.ID).State)
			assert.Empty(t, h.log.all())
			assert.Empty(t, h.events())
		})
	}
}

func TestProcessUnknownDownloadIsNotFound(t *testing.T) {
	h := newPPHarness(t)

	err := h.pp.Process(context.Background(), "nope")

	require.ErrorIs(t, err, core.ErrNotFound)
}

func TestStateChangesGoThroughLegalTransitionsOnly(t *testing.T) {
	h := newPPHarness(t)
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	changes := h.repo.changes()
	require.Len(t, changes, 2)
	assert.Equal(t, core.StatePostProcessing, changes[0].To)
	assert.Equal(t, core.StateReady, changes[1].To)
	assert.Equal(t, time.Unix(2000, 0), changes[1].At, "uses the injected clock")
	assert.NoError(t, core.Transition(core.StateFetched, changes[0].To))
	assert.NoError(t, core.Transition(changes[0].To, changes[1].To))
}

func TestNewPostProcessorRejectsMissingRequiredPorts(t *testing.T) {
	mods := map[string]func(*core.PostProcessorConfig){
		"Downloads":     func(c *core.PostProcessorConfig) { c.Downloads = nil },
		"Bus":           func(c *core.PostProcessorConfig) { c.Bus = nil },
		"Verifier":      func(c *core.PostProcessorConfig) { c.Verifier = nil },
		"Repairer":      func(c *core.PostProcessorConfig) { c.Repairer = nil },
		"Unpacker":      func(c *core.PostProcessorConfig) { c.Unpacker = nil },
		"Finisher":      func(c *core.PostProcessorConfig) { c.Finisher = nil },
		"FetchReporter": func(c *core.PostProcessorConfig) { c.FetchReporter = nil },
		"DirFor":        func(c *core.PostProcessorConfig) { c.DirFor = nil },
	}
	for name, mod := range mods {
		t.Run(name, func(t *testing.T) {
			log := &ppLog{}
			cfg := core.PostProcessorConfig{
				Downloads: newPPRepo(), Bus: core.NewBus(),
				Verifier: &ppVerifier{log: log}, Repairer: &ppRepairer{log: log}, Unpacker: &ppUnpacker{log: log},
				Finisher: &ppFinisher{log: log}, FetchReporter: &ppReporter{log: log},
				DirFor: func(core.Download) string { return "" },
			}
			_, err := core.NewPostProcessor(cfg)
			require.NoError(t, err, "optional fields may be left out")
			mod(&cfg)

			_, err = core.NewPostProcessor(cfg)

			require.Error(t, err)
			assert.Contains(t, err.Error(), name)
		})
	}
}

func TestDefaultClockAndLoggerWork(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.Clock = nil; c.Logger = nil })
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.WithinDuration(t, time.Now(), h.get(d.ID).ReadyAt, time.Minute)
}

type ppFS struct {
	exists bool
	err    error
	seen   []string
}

func (f *ppFS) DirExists(dir string) (bool, error) {
	f.seen = append(f.seen, dir)
	return f.exists, f.err
}

func TestMissingWorkingDirectoryGoesStraightToFinish(t *testing.T) {
	fs := &ppFS{exists: false}
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.FS = fs })
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateReady, got.State)
	assert.Equal(t, "/complete/x", got.OutputPath)
	assert.Equal(t, []string{"/work/x"}, fs.seen)
	assert.Equal(t, []string{"finish"}, h.log.all(), "no verify, repair, detect or unpack")
	assert.Equal(t, "/work/x", h.finisher.gotDir)
	assert.Empty(t, h.finisher.gotUnpacked)
}

func TestMissingWorkingDirectoryWithFinisherErrorFailsInternalRedacted(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.FS = &ppFS{exists: false} })
	h.finisher.fn = func(context.Context, core.Download, string, string) (string, error) {
		return "", errors.New("no marker in /work/x")
	}
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	got := h.get(d.ID)
	assert.Equal(t, core.StateFailed, got.State)
	assert.Equal(t, core.FailureInternal, got.FailureReason)
	assert.NotContains(t, got.FailureDetail, "/work")
	assert.Equal(t, []string{"finish"}, h.log.all())
}

func TestWorkingDirectoryCheckErrorFailsInternal(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.FS = &ppFS{err: errors.New("io")} })
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.FailureInternal, h.get(d.ID).FailureReason)
}

func TestExistingWorkingDirectoryProceeds(t *testing.T) {
	h := newPPHarness(t, func(c *core.PostProcessorConfig) { c.FS = &ppFS{exists: true} })
	d := h.add("x", core.StateFetched, core.SourceNZB)

	require.NoError(t, h.pp.Process(context.Background(), d.ID))

	assert.Equal(t, core.StateReady, h.get(d.ID).State)
}
