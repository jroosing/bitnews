package blackbox_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fetch scenarios: an NZB goes in through the API, the daemon fetches the
// articles from the fake NNTP server over TLS and writes the decoded files.
// See usenet_harness_test.go for what is asserted and why.

// oneServer is the common set-up: one fake server, one Server row, the daemon
// running, the events watched.
type oneServer struct {
	ca  *testCA
	srv *nntpServer
	u   *usenetSetup
	d   *daemon
	ev  *eventLog
}

func startOneServer(t *testing.T, srvMods ...func(*nntpServer)) oneServer {
	t.Helper()
	requireCATrust(t)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword)
	for _, m := range srvMods {
		m(srv)
	}
	u := newUsenetSetup(t, ca, []serverSpec{specFor(srv)})
	d := u.start()
	return oneServer{ca: ca, srv: srv, u: u, d: d, ev: d.watch()}
}

// pollConsistency polls the get and list routes of a Download while it is
// fetched and records what is wrong.
type consistency struct {
	stop   chan struct{}
	done   chan struct{}
	errors []string
}

func pollConsistency(d *daemon, id string) *consistency {
	c := &consistency{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		lastDone := int64(-1)
		bad := func(format string, a ...any) { c.errors = append(c.errors, fmt.Sprintf(format, a...)) }
		for {
			select {
			case <-c.stop:
				return
			default:
			}
			r := d.apiGet("/api/v1/downloads/" + id)
			if r.status != http.StatusOK {
				bad("get answered %d", r.status)
				return
			}
			got := obj(d.t, r)
			l := d.listed(id)
			switch {
			case l == nil:
				bad("the download is in the get route but not in the list")
			case lifecycleRank(l["state"].(string)) < lifecycleRank(got["state"].(string)):
				bad("the list shows %v after the get route showed %v", l["state"], got["state"])
			case num(l, "total_bytes") != num(got, "total_bytes"):
				bad("total_bytes differs between list and get")
			}
			if st := got["state"]; st == "fetching" || st == "fetched" {
				if dn := num(got, "done_bytes"); dn < lastDone {
					bad("done_bytes went from %d to %d", lastDone, dn)
				} else {
					lastDone = dn
				}
				if num(got, "done_bytes") > num(got, "total_bytes") {
					bad("done_bytes %d is over total_bytes %d", num(got, "done_bytes"), num(got, "total_bytes"))
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	return c
}

func (c *consistency) finish(t testing.TB) {
	close(c.stop)
	<-c.done
	assert.Empty(t, c.errors)
}

func TestUsenetASingleFileNZBIsFetchedAndDecodedByteForByte(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "Some.Movie.2024.mkv", 1, 12)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).SetLatency(15 * time.Millisecond) })

	dl := s.d.addRelease(rel)
	id := dl["id"].(string)
	poll := pollConsistency(s.d, id)
	waitFetched(t, s.ev, id)
	poll.finish(t)

	s.u.requireFiles(t, id, rel)
	requireFetchEvents(t, s.ev, id, rel)
	final := settle(s.d, id)
	requireNotFailed(t, final)
	assert.Equal(t, rel.Total, num(final, "total_bytes"))
	assert.Equal(t, rel.Decoded(), num(final, "done_bytes"))
	assert.NotNil(t, final["fetched_at"])
	for _, a := range rel.IDs() {
		assert.Equal(t, 1, s.srv.Requests(a), "each article is asked for once: %s", a)
	}
	assert.LessOrEqual(t, s.srv.Peak(), 2, "never more connections than the Server allows")
	assert.GreaterOrEqual(t, s.srv.Peak(), 1)
}

func TestUsenetAnArticleSplitIntoOnePartIsFetched(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "tiny.bin", 2, 1)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	s.u.requireFiles(t, id, rel)
	requireFetchEvents(t, s.ev, id, rel)
	assert.Equal(t, 1, s.srv.Total())
}

func TestUsenetSeveralFilesAreAllFetchedWithTheirOwnNames(t *testing.T) {
	t.Parallel()
	rel := newRelease(t, []relSpec{
		{Name: "movie.part1.rar", Data: randBytes(10, 9500)},
		{Name: "movie.part2.rar", Data: randBytes(11, 7000)},
		{Name: "movie.nfo", Data: randBytes(12, 300)},
		{Name: "empty-ish.txt", Data: []byte("x")},
		{Name: "with space & (brackets).txt", Data: randBytes(13, 4100)},
		{Name: "ünïcode-名前.bin", Data: randBytes(14, 5200)},
	})
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	id := s.d.addRelease(rel)["id"].(string)
	poll := pollConsistency(s.d, id)
	waitFetched(t, s.ev, id)
	poll.finish(t)

	s.u.requireFiles(t, id, rel)
	requireFetchEvents(t, s.ev, id, rel)
	requireNotFailed(t, settle(s.d, id))
	assert.Equal(t, rel.Segments(), s.srv.Total(), "one request per article")
}

func TestUsenetAFileOfZeroBytesCountsAsAMissingArticle(t *testing.T) {
	t.Parallel()
	// The file writer refuses a part of size 0 ("invalid part"), so the
	// article counts as missing: the empty file is never created and,
	// without a PAR2 set, post-processing fails the Download.
	rel := newRelease(t, []relSpec{
		{Name: "a.bin", Data: randBytes(20, 4000)},
		{Name: "zero.dat", Data: nil},
	})
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	s.u.requireFiles(t, id, release{Files: rel.Files[:1]})
	assert.Equal(t, 1, s.srv.Requests(rel.Files[1].IDs[0]), "the article was fetched")
	assert.NotContains(t, s.u.workFiles(id), "zero.dat")
	requireFetchedOrMissingArticles(t, settle(s.d, id))
}

func TestUsenetNamesComeFromTheYEncHeaderNotTheSubject(t *testing.T) {
	t.Parallel()
	// An obfuscated post: the subject and the yEnc header carry unrelated
	// names. The file is named after the yEnc header.
	rel := newRelease(t, []relSpec{
		{Name: "9d3f0c2ab17e4466a1f0", Subject: `[1/2] - "8e7b5a0c91d2f3a4b5c6" yEnc (1/4) 12000`, Data: randBytes(30, 11000)},
		{Name: "Real.Name.S01E01.mkv", Subject: `d41d8cd98f00b204e9800998ecf8427e`, Data: randBytes(31, 6100)},
		{Name: "no-extension", Subject: `"also-different.bin"`, Data: randBytes(32, 3100)},
	})
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	s.u.requireFiles(t, id, rel)
	requireNotFailed(t, settle(s.d, id))
}

func TestUsenetAHostileFileNameStaysInsideTheWorkDir(t *testing.T) {
	t.Parallel()
	data := randBytes(40, 3500)
	rel := newRelease(t, []relSpec{{Name: "../../escape.bin", Subject: `"safe.bin" yEnc`, Data: data}})
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	var hit fileHit
	eventually(t, 15*time.Second, "the file", func() bool {
		var ok bool
		hit, ok = s.u.findByContent(id, data)
		return ok
	})
	assert.Equal(t, ".._.._escape.bin", filepath.Base(hit.Path), "separators become underscores")
	// Nothing was written outside the Download's own directories.
	for _, outside := range []string{s.u.roots.usenetIncomplete + "/../escape.bin", s.u.roots.usenetIncomplete + "/escape.bin",
		filepath.Join(s.u.roots.usenetIncomplete, id, "..", "escape.bin")} {
		assert.NoFileExists(t, outside)
	}
	entries, err := os.ReadDir(s.u.roots.usenetIncomplete)
	require.NoError(t, err)
	for _, e := range entries {
		assert.True(t, e.IsDir(), "only Download directories in the incomplete root: %s", e.Name())
	}
}

func TestUsenetTwoFilesWithTheSameNameBothSurvive(t *testing.T) {
	t.Parallel()
	a, b := randBytes(50, 3300), randBytes(51, 3400)
	rel := newRelease(t, []relSpec{{Name: "dup.bin", Data: a}, {Name: "dup.bin", Data: b}})
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	var ha, hb fileHit
	eventually(t, 15*time.Second, "both files", func() bool {
		var ok1, ok2 bool
		ha, ok1 = s.u.findByContent(id, a)
		hb, ok2 = s.u.findByContent(id, b)
		return ok1 && ok2
	})
	names := []string{filepath.Base(ha.Path), filepath.Base(hb.Path)}
	assert.ElementsMatch(t, []string{"dup.bin", "dup.1.bin"}, names, "the second file gets a counter before the extension")
}

func TestUsenetAPar2VolumeIsNotFetchedUpFront(t *testing.T) {
	t.Parallel()
	data := randBytes(60, 8000)
	vol := randBytes(61, 6000)
	idx := randBytes(62, 700)
	rel := newRelease(t, []relSpec{
		{Name: "release.bin", Data: data},
		{Name: "release.vol00+02.par2", Data: vol},
		{Name: "release.par2", Data: idx},
	})
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	dl := s.d.addRelease(rel)
	id := dl["id"].(string)
	var upFront int64
	for _, f := range rel.Files {
		if f.Name != "release.vol00+02.par2" {
			for _, e := range f.Encoded {
				upFront += int64(e)
			}
		}
	}
	assert.Equal(t, upFront, num(dl, "total_bytes"), "the recovery volume is not part of the up-front size")
	waitFetched(t, s.ev, id)

	for _, a := range rel.Files[1].IDs {
		assert.Zero(t, s.srv.Requests(a), "recovery volumes wait until repair needs them")
	}
	for _, a := range append(append([]string{}, rel.Files[0].IDs...), rel.Files[2].IDs...) {
		assert.Equal(t, 1, s.srv.Requests(a))
	}
	// The index and the data file are written; the volume is not.
	s.u.requireFiles(t, id, release{Files: []relFile{rel.Files[0]}})
	_, found := s.u.findByContent(id, vol)
	assert.False(t, found, "the recovery volume was not fetched")
}

func TestUsenetTheNZBIsKeptInTheWorkDirWhileItIsFetched(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "keep.bin", 70, 20)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).SetLatency(40 * time.Millisecond) })

	dl := s.d.addRelease(rel, "name", "Kept")
	id := dl["id"].(string)
	eventually(t, longWait, "fetching", func() bool { return s.d.stateOf(id) == "fetching" })

	got, err := os.ReadFile(filepath.Join(s.u.workDir(id), "Kept.nzb"))
	require.NoError(t, err)
	assert.Equal(t, rel.NZB, got)
	assert.Equal(t, int64(len(rel.NZB)), int64(len(got)))
	waitFetched(t, s.ev, id)
}

func TestUsenetTheAPIDoesNotLeakTheServerPassword(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "secret.bin", 80, 4)
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	for _, f := range s.ev.all() {
		assert.NotContains(t, f.data, serverPassword)
	}
	assert.NotContains(t, s.d.apiGet("/api/v1/servers").body, serverPassword)
	assert.NotContains(t, s.d.apiGet("/api/v1/downloads").body, serverPassword)
	assert.NotContains(t, s.d.stdout.String()+s.d.stderr.String(), serverPassword, "no log line has the password")
}
