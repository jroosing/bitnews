package blackbox_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
	"github.com/jroosing/bitnews/internal/postproc/tool/tooltest"
	"github.com/jroosing/bitnews/internal/store"
)

// Test support of the TestPostProc* tests: the whole post-processing pipeline
// (PAR2 verify and repair, unpack, cleanup, tidy and move) judged through the
// HTTP API, the event stream and the files the daemon leaves behind.
//
// How to run them. They need the pinned tools (bin/tools/7zz and
// bin/tools/par2, `make tools`; scripts/get-postproc-tools.sh) both for the
// daemon, which finds them on PATH (TestMain puts bin/tools first on PATH),
// and for the tests, which build PAR2 sets with the real `par2 c` and
// archives with the real `7zz a`. Like the TestUsenet* tests they need a fake
// NNTP server the daemon trusts, so they are skipped for the Go daemon on
// macOS (see requireCATrust). On Linux, in Docker, against the Go daemon:
//
//	GOOS=linux go test -c -o blackbox.test ./internal/blackbox/
//	docker run ... -v <linux tools>:/src/bin/tools:ro ... blackbox.test -test.run TestPostProc
//
// and against another binary with BITNEWS_E2E_BINARY=/abs/path/to/bitnews.
//
// Every test has its own config dir, fake server and daemon process.

// The daemon creates the files it fetches with mode 0666 and its directories
// with 0777, which its UMASK (default 002, ADR-0006) cuts down, while it sets
// the unpacked files to 0644 and the directories to 0755 itself. The tests
// compare modes exactly, so the rig starts the daemon with UMASK=022 unless a
// test asks for the default.
const ppUmask = "022"

// ppRig is a daemon with one fake NNTP server, an open event log and the
// data roots of the config dir.
type ppRig struct {
	*usenetSetup
	srv *nntpServer
	d   *daemon
	ev  *eventLog
	// opts are the options the daemon was started with, for a restart.
	opts []startOption
}

type ppOptions struct {
	tune   []func(*core.Settings)
	start  []startOption
	before []func(*ppRig)
}

type ppOption func(*ppOptions)

// ppTune changes the Settings the daemon starts with.
func ppTune(f func(*core.Settings)) ppOption {
	return func(o *ppOptions) { o.tune = append(o.tune, f) }
}

// ppStart adds options for the daemon process.
func ppStart(opts ...startOption) ppOption {
	return func(o *ppOptions) { o.start = append(o.start, opts...) }
}

// ppBefore runs f on the stopped config dir, before the daemon starts.
func ppBefore(f func(*ppRig)) ppOption {
	return func(o *ppOptions) { o.before = append(o.before, f) }
}

// ppSlots limits how many post-processing scenarios run at once. They start
// real tools and a daemon each, and the older tests of the package are
// sensitive to a machine that is busy with a hundred of them.
var ppSlots = make(chan struct{}, 3)

// newPPRig starts a daemon that fetches from one fake server. Call it after
// t.Parallel(): it skips the test when the daemon cannot trust the test CA.
func newPPRig(t *testing.T, opts ...ppOption) *ppRig {
	t.Helper()
	requireCATrust(t)
	ppSlots <- struct{}{}
	t.Cleanup(func() { <-ppSlots })
	var o ppOptions
	for _, f := range opts {
		f(&o)
	}
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword)
	r := &ppRig{srv: srv, opts: o.start}
	r.usenetSetup = newUsenetSetup(t, ca, []serverSpec{specFor(srv)}, o.tune...)
	for _, f := range o.before {
		f(r)
	}
	r.startDaemon()
	return r
}

// startDaemon (re)starts the daemon on the config dir and opens the event log.
func (r *ppRig) startDaemon() {
	r.t.Helper()
	r.d = r.start(append([]startOption{withEnv("UMASK", ppUmask)}, r.opts...)...)
	r.ev = r.d.watch()
}

// serve makes the fake server serve rel.
func (r *ppRig) serve(rel release) release {
	r.srv.Install(rel)
	return rel
}

// add uploads the NZB of rel under the file name name+".nzb" and returns the
// id of the new Download.
func (r *ppRig) add(rel release, name string, fields ...string) string {
	r.t.Helper()
	resp := r.d.upload(name+".nzb", rel.NZB, fields...)
	require.Equal(r.t, http.StatusCreated, resp.status, resp.body)
	return obj(r.t, resp)["id"].(string)
}

// terminal waits until the Download is ready or failed and returns it.
func (r *ppRig) terminal(id string) map[string]any {
	r.t.Helper()
	var m map[string]any
	eventually(r.t, longWait, "download "+id+" to be ready or failed", func() bool {
		m = r.d.download(id)
		return m["state"] == "ready" || m["state"] == "failed"
	})
	return m
}

// ready waits for the Download to be ready and returns its output path.
func (r *ppRig) ready(id string) string {
	r.t.Helper()
	m := r.terminal(id)
	require.Equal(r.t, "ready", m["state"], "failure: %v: %v", m["failure_reason"], m["failure_detail"])
	out, _ := m["output_path"].(string)
	require.NotEmpty(r.t, out, "a ready download has an output path")
	return out
}

// failed waits for the Download to fail with this reason and returns it.
func (r *ppRig) failed(id, reason string) map[string]any {
	r.t.Helper()
	m := r.terminal(id)
	require.Equal(r.t, "failed", m["state"], "the download should have failed: %v", m)
	require.Equal(r.t, reason, m["failure_reason"], "detail: %v", m["failure_detail"])
	assert.Nil(r.t, m["ready_at"])
	assert.NotContains(r.t, m, "output_path", "a failed download has no output")
	return m
}

// waitState waits until the Download is in the state.
func (r *ppRig) waitState(id, state string) {
	r.t.Helper()
	eventually(r.t, longWait, fmt.Sprintf("download %s to be %s", id, state), func() bool {
		return r.d.stateOf(id) == state
	})
}

// work is where the Download's files are while it is worked on.
func (r *ppRig) work(id string) string { return r.workDir(id) }

// addCategoryDirs stores a Category in the stopped config dir. Empty dirs mean
// no override for that protocol.
func addCategoryDirs(t *testing.T, cfg configDir, name, usenetDir, torrentDir string) core.ID {
	t.Helper()
	db := openDB(t, cfg)
	c := core.Category{ID: core.NewID(), Name: name, UsenetDir: usenetDir, TorrentDir: torrentDir}
	require.NoError(t, store.NewCategoryRepo(db).Create(t.Context(), c))
	require.NoError(t, db.Close())
	return c.ID
}

// ---- the event trace ----

// trace is the story of one Download as the event stream told it, one label
// per event that matters: "state:<s>" (repeats collapsed), "fetched",
// "stage:<s>", "ready" and "failed:<reason>". Progress events are left out.
func (l *eventLog) trace(id string) []string {
	var out []string
	add := func(s string) {
		if strings.HasPrefix(s, "state:") && len(out) > 0 && out[len(out)-1] == s {
			return
		}
		out = append(out, s)
	}
	for _, f := range l.of(id) {
		m := fjson(l.t, f)
		switch f.name {
		case "download_added", "download_updated":
			add("state:" + m["state"].(string))
		case "fetched", "ready":
			add(f.name)
		case "postprocess_stage":
			add("stage:" + m["stage"].(string))
		case "failed":
			add("failed:" + m["reason"].(string))
		}
	}
	return out
}

// traceUntil waits for the trace to end with last ("ready" or "failed:...")
// and returns it.
func (l *eventLog) traceUntil(id, last string) []string {
	var tr []string
	eventually(l.t, longWait, "the event "+last+" of "+id, func() bool {
		tr = l.trace(id)
		return len(tr) > 0 && tr[len(tr)-1] == last
	})
	return tr
}

// The trace of a Download that was fetched once and then post-processed.
func ppTrace(stages ...string) []string {
	out := []string{"state:queued", "state:fetching", "state:fetched", "fetched", "state:post_processing"}
	for _, s := range stages {
		out = append(out, "stage:"+s)
	}
	return out
}

// ---- trees ----

// treeEntry is one entry of a directory tree: what the output of a Download
// must look like, byte for byte.
type treeEntry struct {
	dir  bool
	mode fs.FileMode
	data []byte
}

// ppFile is a regular file of mode 0644, what the daemon leaves.
func ppFile(data []byte) treeEntry { return treeEntry{mode: 0o644, data: data} }

// ppFileMode is a regular file of another mode.
func ppFileMode(data []byte, mode fs.FileMode) treeEntry { return treeEntry{mode: mode, data: data} }

// ppDir is a directory of mode 0755.
func ppDir() treeEntry { return treeEntry{dir: true, mode: 0o755} }

// tree maps slash paths relative to the root to entries.
type tree map[string]treeEntry

// readTree reads everything below root without following links. A symlink
// is reported with its type bits in the mode and its target as data.
func readTree(t testing.TB, root string) tree {
	t.Helper()
	out := tree{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			out[rel] = treeEntry{dir: true, mode: fi.Mode().Perm()}
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = treeEntry{mode: fi.Mode().Perm(), data: b}
		default:
			target, _ := os.Readlink(p)
			out[rel] = treeEntry{mode: fi.Mode().Type() | fi.Mode().Perm(), data: []byte(target)}
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// markerRE is the content of .bitnews-done: the Download id, then the time.
func markerRE(id string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(id) + `\n\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\n$`)
}

// requireTree compares the tree below root with want, exactly: the same
// paths, the same kinds, sizes, bytes and permission bits.
func requireTree(t testing.TB, root string, want tree) {
	t.Helper()
	got := readTree(t, root)
	var problems []string
	for _, p := range sortedTreeKeys(want) {
		w := want[p]
		g, ok := got[p]
		switch {
		case !ok:
			problems = append(problems, "missing: "+p)
		case g.dir != w.dir:
			problems = append(problems, fmt.Sprintf("kind of %s: want dir=%t, got dir=%t", p, w.dir, g.dir))
		case g.mode != w.mode:
			problems = append(problems, fmt.Sprintf("mode of %s: want %v, got %v", p, w.mode, g.mode))
		case !w.dir && !bytes.Equal(g.data, w.data):
			problems = append(problems, fmt.Sprintf("content of %s: want %d bytes, got %d (first difference at %d)", p, len(w.data), len(g.data), firstDiff(w.data, g.data)))
		}
	}
	for _, p := range sortedTreeKeys(got) {
		if _, ok := want[p]; !ok {
			problems = append(problems, "unexpected: "+p)
		}
	}
	if len(problems) > 0 {
		t.Fatalf("the tree below %s differs:\n  %s", root, strings.Join(problems, "\n  "))
	}
}

func sortedTreeKeys(m tree) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func firstDiff(a, b []byte) int {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return i
}

// requireOutput checks the finished output dir of a Download: the marker
// file .bitnews-done that the Finisher leaves for a retry after a crash (id
// on the first line, the time on the second, mode 0644), the mode of the dir
// itself, and then every other entry against want.
func requireOutput(t testing.TB, out, id string, want tree) {
	t.Helper()
	fi, err := os.Lstat(out)
	require.NoError(t, err, "the output dir")
	require.True(t, fi.IsDir())
	assert.Equal(t, fs.FileMode(0o755), fi.Mode().Perm(), "mode of the output dir")
	got := readTree(t, out)
	marker, ok := got[".bitnews-done"]
	require.True(t, ok, "the done marker is in the output; the tree is %v", sortedTreeKeys(got))
	assert.Equal(t, fs.FileMode(0o644), marker.mode)
	assert.Regexp(t, markerRE(id), string(marker.data))
	w := tree{}
	for k, v := range want {
		w[k] = v
	}
	w[".bitnews-done"] = marker
	requireTree(t, out, w)
}

// ---- tools ----

// runTool runs a pinned tool in dir and fails the test when it fails.
func runTool(t testing.TB, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(tooltest.Path(t, name), args...)
	cmd.Dir = dir
	cmd.Env = []string{"LC_ALL=C", "HOME=" + dir}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %v\n%s", name, args, out)
	return string(out)
}

// writeFiles writes the files (name to content) below dir.
func writeFiles(t testing.TB, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, data, 0o644))
	}
}

// readDirSpecs reads the regular files of dir whose names pass keep as
// release files, sorted by name.
func readDirSpecs(t testing.TB, dir string, keep func(name string) bool) []relSpec {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []relSpec
	for _, e := range ents {
		if !e.Type().IsRegular() || !keep(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		out = append(out, relSpec{Name: e.Name(), Data: b})
	}
	return out
}

// par2Set is a PAR2 set made by the real `par2 c`.
type par2Set struct {
	// Index is <name>.par2: the packets without recovery data.
	Index relSpec
	// Volumes are <name>.volNN+MM.par2, in name order.
	Volumes []relSpec
	// Blocks is the MM of each volume.
	Blocks []int
}

// makePar2 creates the PAR2 set of the files with `par2 c -s<block>
// -c<recovery> -n<volumes>`. A block equal to the article size (3000) makes
// block i of a file the bytes of article i, so one lost article costs
// exactly one block. The set is named name+".par2".
func makePar2(t testing.TB, name string, files []relSpec, block, recovery, volumes int) par2Set {
	t.Helper()
	dir := t.TempDir()
	args := []string{"c", fmt.Sprintf("-s%d", block), fmt.Sprintf("-c%d", recovery), fmt.Sprintf("-n%d", volumes), "--", name + ".par2"}
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f.Name), f.Data, 0o644))
		args = append(args, f.Name)
	}
	runTool(t, dir, "par2", args...)
	isPar2 := func(n string) bool { return strings.HasSuffix(n, ".par2") }
	specs := readDirSpecs(t, dir, isPar2)
	var set par2Set
	volRE := regexp.MustCompile(`\.vol\d+\+(\d+)\.par2$`)
	for _, s := range specs {
		if m := volRE.FindStringSubmatch(s.Name); m != nil {
			var n int
			_, err := fmt.Sscan(m[1], &n)
			require.NoError(t, err)
			set.Volumes = append(set.Volumes, s)
			set.Blocks = append(set.Blocks, n)
		} else {
			set.Index = s
		}
	}
	require.NotEmpty(t, set.Index.Name, "par2 wrote no index file")
	return set
}

// OneFile merges the index and the first volume into the single file
// <name>.par2 that holds both the file descriptions and the recovery
// blocks: the form of a PAR2 set that has no separate recovery volumes, so
// none of its files is deferred.
func (s par2Set) OneFile(t testing.TB, name string) relSpec {
	t.Helper()
	require.Len(t, s.Volumes, 1, "make the set with one volume")
	return relSpec{Name: name + ".par2", Data: s.Volumes[0].Data}
}

// all lists the index and the volumes.
func (s par2Set) all() []relSpec { return append([]relSpec{s.Index}, s.Volumes...) }

// flipByte returns a copy of data with the byte at i inverted.
func flipByte(data []byte, i int) []byte {
	out := slices.Clone(data)
	out[i] ^= 0xff
	return out
}

// sevenZipSet makes archives with the real `7zz a` and returns the files it
// wrote (several for a split set), in name order. out is the name of the
// first file ("x.7z", "x.zip"); extra are arguments such as -v3k or -pSECRET.
func sevenZipSet(t testing.TB, out string, files map[string][]byte, extra ...string) []relSpec {
	t.Helper()
	src, dst := t.TempDir(), t.TempDir()
	writeFiles(t, src, files)
	for n := range files {
		if strings.HasSuffix(n, ".sh") {
			require.NoError(t, os.Chmod(filepath.Join(src, n), 0o755)) // executable files are stored as such
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, strings.SplitN(n, "/", 2)[0])
	}
	slices.Sort(names)
	names = slices.Compact(names)
	args := append([]string{"a", "-bd", "-y"}, extra...)
	args = append(args, filepath.Join(dst, out))
	args = append(args, names...)
	runTool(t, src, "7zz", args...)
	return readDirSpecs(t, dst, func(string) bool { return true })
}

// ---- a tool that waits ----

// toolShim is a script placed first on PATH under the name of a pinned tool.
// When the daemon runs it, the shim records that it started, waits until the
// test opens the gate and then runs the real tool. It lets a test hold a
// Download inside a stage for as long as it likes, without sleeping. The
// script uses only shell builtins and /bin/sleep, because the daemon runs
// tools with an empty environment.
type toolShim struct {
	t    testing.TB
	name string
	bin  string // directory to put first on PATH
	dir  string // state: started.<pid> files and the gate
}

func newToolShim(t testing.TB, name string) *toolShim {
	t.Helper()
	real := tooltest.Path(t, name)
	s := &toolShim{t: t, name: name, bin: t.TempDir(), dir: t.TempDir()}
	script := fmt.Sprintf(`#!/bin/sh
: > '%[1]s'/started.$$
while [ ! -e '%[1]s'/gate ]; do /bin/sleep 0.05; done
exec '%[2]s' "$@"
`, s.dir, real)
	require.NoError(t, os.WriteFile(filepath.Join(s.bin, name), []byte(script), 0o755))
	return s
}

// onPath is the daemon option that puts the shim first on PATH.
func (s *toolShim) onPath() startOption {
	return withEnv("PATH", s.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// started is how many times the shim was started, and with which pids.
func (s *toolShim) started() []string {
	ents, err := os.ReadDir(s.dir)
	require.NoError(s.t, err)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "started.") {
			out = append(out, strings.TrimPrefix(e.Name(), "started."))
		}
	}
	slices.Sort(out)
	return out
}

// waitStarted waits until the shim was started n times.
func (s *toolShim) waitStarted(n int) {
	s.t.Helper()
	eventually(s.t, longWait, fmt.Sprintf("the %s shim to be started %d times", s.name, n), func() bool {
		return len(s.started()) >= n
	})
}

// open lets the shim, and every later start of it, run the real tool.
func (s *toolShim) open() {
	s.t.Helper()
	require.NoError(s.t, os.WriteFile(filepath.Join(s.dir, "gate"), nil, 0o644))
}

// ---- small things ----

// bytesOf returns n deterministic bytes.
func bytesOf(seed uint64, n int) []byte { return randBytes(seed, n) }

// idsOfNames lists the message ids of the files of rel with these names.
func idsOfNames(rel release, names ...string) []string {
	var out []string
	for _, f := range rel.Files {
		if slices.Contains(names, f.Name) {
			out = append(out, f.IDs...)
		}
	}
	return out
}

// fileOfRel returns the release file with this name.
func fileOfRel(t testing.TB, rel release, name string) relFile {
	t.Helper()
	for _, f := range rel.Files {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("release has no file %q", name)
	return relFile{}
}

// requestedAny reports whether the server was asked for any of the ids.
func requestedAny(s *nntpServer, ids []string) bool {
	for _, id := range ids {
		if s.Requests(id) > 0 {
			return true
		}
	}
	return false
}

// statfsFree is the number of bytes a normal user can still write on the
// disk of dir.
func statfsFree(t testing.TB, dir string) uint64 {
	t.Helper()
	var st syscall.Statfs_t
	require.NoError(t, syscall.Statfs(dir, &st))
	return uint64(st.Bavail) * uint64(st.Bsize) //nolint:gosec // both are non-negative
}
