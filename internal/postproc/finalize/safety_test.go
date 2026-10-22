package finalize_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/kyura4k/gitnews/internal/core"
	"gitlab.com/kyura4k/gitnews/internal/fsutil"
	"gitlab.com/kyura4k/gitnews/internal/postproc/finalize"
)

// The orchestrator's Finisher port.
type finisherPort interface {
	Finish(ctx context.Context, d core.Download, dir string, unpackedDir string) (outputPath string, err error)
}

var _ finisherPort = (*finalize.Finisher)(nil)

func TestCrossDeviceMoveEmitsWarning(t *testing.T) {
	e := newEnv(t)
	e.mover = func(src, dst string) (fsutil.MoveResult, error) {
		res, err := fsutil.MoveDir(src, dst)
		res.CrossDevice = true // pretend the copy path was taken
		return res, err
	}
	d := usenetDownload("Show")
	out, err := finishUsenet(t, e, d)
	require.NoError(t, err)

	w := e.warns()
	require.Len(t, w, 1)
	require.Equal(t, "cross_device", w[0].Kind)
	require.Equal(t, d.ID, w[0].DownloadID)
	require.Equal(t, out, w[0].Path)
	require.Contains(t, w[0].Message, "hardlinks")
}

func TestSameDeviceMoveEmitsNoWarning(t *testing.T) {
	e := newEnv(t)
	_, err := finishUsenet(t, e, usenetDownload("Show"))
	require.NoError(t, err)
	require.Empty(t, e.warns())
}

func TestMoveFailureIsReturned(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("disk on fire")
	e.mover = func(string, string) (fsutil.MoveResult, error) { return fsutil.MoveResult{}, boom }
	_, err := finishUsenet(t, e, usenetDownload("Show"))
	require.ErrorIs(t, err, boom)
}

func TestMarkerIsWrittenBeforeTheMoveWithMode0644(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	out, err := finishUsenet(t, e, d)
	require.NoError(t, err)

	p := filepath.Join(out, ".bitnews-done")
	fi, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), fi.Mode().Perm()&0o644)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Contains(t, string(b), string(d.ID)+"\n")
}

func TestNothingChangesInsideFinalDirAfterTheMove(t *testing.T) {
	e := newEnv(t)
	var atMove map[string]string
	e.mover = func(src, dst string) (fsutil.MoveResult, error) {
		atMove = listing(t, src)
		return fsutil.MoveDir(src, dst)
	}
	out, err := finishUsenet(t, e, usenetDownload("Show"))
	require.NoError(t, err)
	require.Equal(t, atMove, listing(t, out), "cleanup and marker happen before the move")
}

func TestRetryAfterCrashReturnsAlreadyMovedDir(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a.mkv": "a"})
	f := e.finisher()

	first, err := f.Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.NoDirExists(t, dir)
	before := hashTree(t, first)

	again, err := f.Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, before, hashTree(t, first), "retry changes nothing")
}

func TestRetryAfterCrashFindsUniqueVariant(t *testing.T) {
	e := newEnv(t)
	taken := filepath.Join(e.settings.Paths.Usenet.Complete, "Show")
	writeFiles(t, taken, map[string]string{"other.mkv": "other"})
	d := usenetDownload("Show")
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a.mkv": "a"})
	f := e.finisher()

	first, err := f.Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, taken+".1", first)

	again, err := f.Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, first, again)
}

func TestRetryDoesNotAdoptAnotherDownloadsDir(t *testing.T) {
	e := newEnv(t)
	other := usenetDownload("Show")
	_, err := finishUsenet(t, e, other)
	require.NoError(t, err)

	d := usenetDownload("Show") // same name, different Download, dir lost
	_, err = e.finisher().Finish(context.Background(), d, filepath.Join(e.settings.Paths.Usenet.Incomplete, "gone"), "")
	require.Error(t, err)
}

func TestRetryWithPartiallyCleanedDirResumes(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	dir := e.usenetDir("w")
	// Crash happened after the archives were deleted and half the output was merged.
	writeFiles(t, dir, map[string]string{"a.mkv": "merged", "keep.nfo": "nfo", "_unpack/b.mkv": "pending"})

	out, err := e.finisher().Finish(context.Background(), d, dir, filepath.Join(dir, "_unpack"))
	require.NoError(t, err)
	got := listing(t, out)
	delete(got, MarkerName)
	require.Equal(t, map[string]string{"a.mkv": "merged", "b.mkv": "pending", "keep.nfo": "nfo"}, got)
}

func TestRetryWhenUnpackedDirAlreadyMergedAndRemoved(t *testing.T) {
	e := newEnv(t)
	d := usenetDownload("Show")
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a.mkv": "merged", "x.par2": "p"})

	out, err := e.finisher().Finish(context.Background(), d, dir, filepath.Join(dir, "_unpack"))
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(out, "x.par2"))
	require.FileExists(t, filepath.Join(out, "a.mkv"))
}

func TestMissingUnpackedDirWithArchivesLeftDeletesNothing(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"movie.rar": "rar", "movie.par2": "p"})

	_, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, filepath.Join(dir, "_unpack"))
	require.Error(t, err)
	require.FileExists(t, filepath.Join(dir, "movie.rar"))
	require.FileExists(t, filepath.Join(dir, "movie.par2"))
}

func TestCancelBeforeAnythingDoesNothing(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a.mkv": "a", "a.par2": "p"})
	before := hashTree(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.finisher().Finish(ctx, usenetDownload("Show"), dir, "")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, hashTree(t, dir))
}

func TestCancelBeforeMoveLeavesDirInPlace(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a.mkv": "a", "a.par2": "p"})
	ctx, cancel := context.WithCancel(context.Background())
	d := usenetDownload("Show")
	d.CategoryID = core.NewID()
	f := finalize.New(finalize.Deps{
		Settings: func(context.Context) (core.Settings, error) { return e.settings, nil },
		// Cancelled while the destination is resolved, before anything destructive.
		Categories: func(context.Context, core.ID) (core.Category, error) {
			cancel()
			return core.Category{Name: "tv"}, nil
		},
	})

	_, err := f.Finish(ctx, d, dir, "")
	require.ErrorIs(t, err, context.Canceled)
	require.FileExists(t, filepath.Join(dir, "a.mkv"))
	require.FileExists(t, filepath.Join(dir, "a.par2"))
	require.NoFileExists(t, filepath.Join(dir, MarkerName))
	require.NoDirExists(t, e.settings.Paths.Usenet.Complete)
}

func TestCancelDuringMoveStepIsReportedAndNothingIsMoved(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.mover = func(src, dst string) (fsutil.MoveResult, error) {
		t.Fatal("must not move after cancellation")
		return fsutil.MoveResult{}, nil
	}
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a.mkv": "a"})
	d := usenetDownload("Show")
	d.CategoryID = core.NewID()
	f := finalize.New(finalize.Deps{
		Settings: func(context.Context) (core.Settings, error) { return e.settings, nil },
		Categories: func(context.Context, core.ID) (core.Category, error) {
			cancel()
			return core.Category{}, core.ErrNotFound
		},
		Mover: e.mover,
	})
	_, err := f.Finish(ctx, d, dir, "")
	require.ErrorIs(t, err, context.Canceled)
	require.DirExists(t, dir)
}

func TestCleanupNeverTouchesFilesOutsideDir(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("Show")
	parent := e.settings.Paths.Usenet.Incomplete
	sibling := filepath.Join(parent, "Show2") // shares a name prefix with dir
	writeFiles(t, parent, map[string]string{"parent.par2": "p", "parent.nzb": "n", "parent.rar": "r"})
	writeFiles(t, sibling, map[string]string{"x.par2": "p", "x.rar": "r", "x.nzb": "n"})
	outside := filepath.Join(e.base, "outside")
	writeFiles(t, outside, map[string]string{"victim.par2": "p", "victim.rar": "r", "victim.nzb": "n"})
	writeFiles(t, dir, map[string]string{"a.mkv": "a", "a.par2": "p", "a.rar": "r", "_unpack/b.mkv": "b"})
	// Symlinks named like garbage, pointing at the outside files and dirs.
	require.NoError(t, os.Symlink(filepath.Join(outside, "victim.par2"), filepath.Join(dir, "link.par2")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "victim.rar"), filepath.Join(dir, "link.rar")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "_unpack.tmp")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "dirlink.par2")))
	beforeParent := listing(t, parent)
	delete(beforeParent, "Show")
	for k := range beforeParent {
		if len(k) >= 5 && k[:5] == "Show/" {
			delete(beforeParent, k)
		}
	}
	beforeOutside := listing(t, outside)

	out, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, filepath.Join(dir, "_unpack"))
	require.NoError(t, err)

	afterParent := listing(t, parent)
	delete(afterParent, "Show")
	require.Equal(t, beforeParent, afterParent)
	require.Equal(t, beforeOutside, listing(t, outside))
	// The symlinks themselves were neither followed nor deleted.
	for _, l := range []string{"link.par2", "link.rar", "_unpack.tmp", "dirlink.par2"} {
		fi, err := os.Lstat(filepath.Join(out, l))
		require.NoError(t, err, l)
		require.NotZero(t, fi.Mode()&os.ModeSymlink, l)
	}
	require.FileExists(t, filepath.Join(out, "b.mkv"))
	require.NoFileExists(t, filepath.Join(out, "a.rar"))
}

func TestUnpackedDirThatIsASymlinkIsRefused(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("w")
	outside := filepath.Join(e.base, "outside")
	writeFiles(t, outside, map[string]string{"secret": "s"})
	writeFiles(t, dir, map[string]string{"a.rar": "r"})
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "_unpack")))

	_, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, filepath.Join(dir, "_unpack"))
	require.ErrorIs(t, err, fsutil.ErrSymlink)
	require.FileExists(t, filepath.Join(dir, "a.rar"))
	require.FileExists(t, filepath.Join(outside, "secret"))
}

func TestUnpackedDirOverlappingDownloadDirIsRefused(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"a.rar": "r"})
	for _, un := range []string{dir, filepath.Dir(dir), filepath.Join(dir, "_unpack.tmp", "x")} {
		_, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, un)
		require.Error(t, err, un)
		require.FileExists(t, filepath.Join(dir, "a.rar"))
	}
}

func TestUnpackedEntryNeverOverwritesAKeptFile(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("w")
	writeFiles(t, dir, map[string]string{"movie.nfo": "kept", "a.rar": "r", "_unpack/movie.nfo": "from archive"})

	out, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, filepath.Join(dir, "_unpack"))
	require.NoError(t, err)
	got := listing(t, out)
	require.Equal(t, "kept", got["movie.nfo"])
	require.Equal(t, "from archive", got["movie.1.nfo"])
}

func TestUnpackedDirOutsideDownloadDirIsMergedAndRemoved(t *testing.T) {
	e := newEnv(t)
	dir := e.usenetDir("w")
	un := filepath.Join(e.settings.Paths.Usenet.Incomplete, "w.unpacked")
	writeFiles(t, dir, map[string]string{"a.rar": "r", "a.nfo": "nfo"})
	writeFiles(t, un, map[string]string{"film.mkv": "film"})

	out, err := e.finisher().Finish(context.Background(), usenetDownload("Show"), dir, un)
	require.NoError(t, err)
	got := listing(t, out)
	delete(got, MarkerName)
	require.Equal(t, map[string]string{"a.nfo": "nfo", "film.mkv": "film"}, got)
	require.NoDirExists(t, un)
}

func TestExistingForeignMarkerIsKeptWithWarning(t *testing.T) {
	e := newEnv(t)
	dir := e.torrentDir("Rel")
	writeFiles(t, dir, map[string]string{MarkerName: "i am an original", "a": "a"})
	d := torrentDownload("Rel")

	out, err := e.finisher().Finish(context.Background(), d, dir, "")
	require.NoError(t, err)
	require.Equal(t, "i am an original", listing(t, out)[MarkerName])
	require.Len(t, e.warns(), 1)
	require.Equal(t, "marker_conflict", e.warns()[0].Kind)
}

func TestDirMustExistAndBeADirectory(t *testing.T) {
	e := newEnv(t)
	f := e.finisher()
	_, err := f.Finish(context.Background(), usenetDownload("S"), "", "")
	require.Error(t, err)
	_, err = f.Finish(context.Background(), usenetDownload("S"), filepath.Join(e.base, "nope"), "")
	require.Error(t, err)
	file := filepath.Join(e.base, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	_, err = f.Finish(context.Background(), usenetDownload("S"), file, "")
	require.Error(t, err)
	link := filepath.Join(e.base, "link")
	require.NoError(t, os.Symlink(e.base, link))
	_, err = f.Finish(context.Background(), usenetDownload("S"), link, "")
	require.ErrorIs(t, err, fsutil.ErrSymlink)
}

func TestUnwritableCompleteFolderFailsNamingTheFolderAndTheUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	e := newEnv(t)
	complete := e.settings.Paths.Usenet.Complete
	require.NoError(t, os.MkdirAll(complete, 0o755))
	require.NoError(t, os.Chmod(complete, 0o555))
	t.Cleanup(func() { _ = os.Chmod(complete, 0o755) })

	_, err := finishUsenet(t, e, usenetDownload("Show"))

	require.ErrorIs(t, err, fs.ErrPermission)
	require.Contains(t, err.Error(), complete)
	require.Contains(t, err.Error(), fmt.Sprintf("uid %d", os.Geteuid()))
}
