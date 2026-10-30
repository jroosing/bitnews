package blackbox_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Articles that are lost, damaged or hard to get, and Servers that help or
// do not.

// partRange is the byte range of part i (0-based) of f in the decoded file.
func partRange(f relFile, i int) (start, end int) {
	start = f.Offsets[i]
	end = len(f.Data)
	if i+1 < len(f.Offsets) {
		end = f.Offsets[i+1]
	}
	return start, end
}

// requireDamageDetail checks, for a Download that failed in the Go daemon's
// post-processing, the exact detail it gives.
func requireDamageDetail(t testing.TB, m map[string]any, missing, damaged int) {
	t.Helper()
	if m["state"] == "failed" {
		assert.Equal(t, fmt.Sprintf("verifying: missing articles: %d missing, %d damaged and no PAR2 set", missing, damaged), m["failure_detail"])
	}
}

func TestUsenetALostArticleLeavesAHoleAndTheDownloadIsStillFetched(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "lost.bin", 100, 8)
	f := rel.Files[0]
	lost := f.IDs[2]
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).Missing(lost) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	// The runner does not fail a Download for missing articles: it reaches
	// fetched with the file as complete as it could be and leaves the
	// decision to repair. The file has its full size; the hole is zeros.
	b, err := os.ReadFile(filepath.Join(s.u.workDir(id), "lost.bin"))
	require.NoError(t, err)
	require.Len(t, b, len(f.Data), "the file is created at its full size")
	from, to := partRange(f, 2)
	want := append([]byte(nil), f.Data...)
	clear(want[from:to])
	requireSameBytes(t, want, b, "everything but the lost article")
	assert.Equal(t, 1, s.srv.Requests(lost), "a lost article is asked for once per Server")

	fm := fjson(t, s.ev.of(id)[s.ev.firstIndex(id, stateIs(t, "fetched"))])
	assert.Equal(t, rel.Total, num(fm, "total_bytes"))
	assert.Equal(t, rel.Decoded()-int64(to-from), num(fm, "done_bytes"), "done_bytes counts the decoded bytes that arrived")
	final := settle(s.d, id)
	requireFetchedOrMissingArticles(t, final)
	requireDamageDetail(t, final, 1, 0)
}

func TestUsenetALostLastArticleStillGivesTheFullSizeFile(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "tail.bin", 101, 5)
	f := rel.Files[0]
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).Missing(f.IDs[4]) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	b, err := os.ReadFile(filepath.Join(s.u.workDir(id), "tail.bin"))
	require.NoError(t, err)
	assert.Len(t, b, len(f.Data))
	from, _ := partRange(f, 4)
	requireSameBytes(t, f.Data[:from], b[:from], "the articles before the lost one")
}

func TestUsenetAFileWhoseArticlesAreAllLostIsNeverCreated(t *testing.T) {
	t.Parallel()
	rel := newRelease(t, []relSpec{
		{Name: "good.bin", Data: randBytes(110, 7000)},
		{Name: "gone.bin", Data: randBytes(111, 7000)},
	})
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).Missing(rel.Files[1].IDs...) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	require.Eventually(t, func() bool { return len(s.u.workFiles(id)) > 0 }, 10*time.Second, pollEvery)
	assert.Contains(t, s.u.workFiles(id), "good.bin")
	assert.NotContains(t, s.u.workFiles(id), "gone.bin")
	b, err := os.ReadFile(filepath.Join(s.u.workDir(id), "good.bin"))
	require.NoError(t, err)
	requireSameBytes(t, rel.Files[0].Data, b, "the file that is there")
	final := settle(s.d, id)
	requireFetchedOrMissingArticles(t, final)
	requireDamageDetail(t, final, len(rel.Files[1].IDs), 0)
}

func TestUsenetADamagedArticleIsWrittenAndCountedNotDropped(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "damaged.bin", 120, 6)
	f := rel.Files[0]
	bad := f.IDs[3]
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).Corrupt(bad) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	b, err := os.ReadFile(filepath.Join(s.u.workDir(id), "damaged.bin"))
	require.NoError(t, err)
	require.Len(t, b, len(f.Data))
	from, to := partRange(f, 3)
	requireSameBytes(t, f.Data[:from], b[:from], "before the damaged article")
	requireSameBytes(t, f.Data[to:], b[to:], "after the damaged article")
	differ := 0
	for i := from; i < to; i++ {
		if b[i] != f.Data[i] {
			differ++
		}
	}
	assert.Equal(t, 1, differ, "the damaged article is written as it came: one wrong byte")
	assert.Equal(t, 1, s.srv.Requests(bad), "with no other Server there is nobody to ask again")

	fm := fjson(t, s.ev.of(id)[s.ev.firstIndex(id, stateIs(t, "fetched"))])
	assert.Equal(t, rel.Decoded(), num(fm, "done_bytes"), "a damaged article counts as received")
	final := settle(s.d, id)
	requireFetchedOrMissingArticles(t, final)
	requireDamageDetail(t, final, 0, 1)
}

func TestUsenetADamagedArticleIsAskedOfTheNextServerAndTheGoodCopyWins(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := oneFile(t, "heal.bin", 130, 6)
	bad := rel.Files[0].IDs[2]
	ca := newTestCA(t)
	s1 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).Corrupt(bad)
	s2 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(s1, specPriority(1)), specFor(s2, specPriority(2))})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	waitFetched(t, ev, id)

	u.requireFiles(t, id, rel)
	requireNotFailed(t, settle(d, id))
	assert.Equal(t, 1, s1.Requests(bad))
	assert.Equal(t, 1, s2.Requests(bad), "only the damaged article went to the second Server")
	assert.Equal(t, 1, s2.Total())
	for _, a := range rel.IDs() {
		assert.Equal(t, 1, s1.Requests(a))
	}
}

func TestUsenetArticlesTheFirstServerLacksComeFromTheSecondByPriority(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := oneFile(t, "failover.bin", 140, 12)
	ids := rel.IDs()
	lacking := []string{ids[1], ids[4], ids[5], ids[11]}
	ca := newTestCA(t)
	s1 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).Missing(lacking...)
	s2 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel)
	// The lower priority number is the main Server, whatever the order of
	// the rows: create the fill Server first.
	u := newUsenetSetup(t, ca, []serverSpec{specFor(s2, specPriority(2), specConns(3)), specFor(s1, specPriority(1), specConns(2))})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	poll := pollConsistency(d, id)
	waitFetched(t, ev, id)
	poll.finish(t)

	u.requireFiles(t, id, rel)
	requireFetchEvents(t, ev, id, rel)
	requireNotFailed(t, settle(d, id))
	for _, a := range ids {
		assert.Equal(t, 1, s1.Requests(a), "the main Server is asked first, once, for every article: %s", a)
	}
	for _, a := range ids {
		want := 0
		if contains(lacking, a) {
			want = 1
		}
		assert.Equal(t, want, s2.Requests(a), "the fill Server only gets what the main one lacks: %s", a)
	}
	assert.Equal(t, len(lacking), s2.Total())
	assert.GreaterOrEqual(t, s2.Dials(), 1, "the fill Server was connected to")
	assert.LessOrEqual(t, s1.Peak(), 2)
	assert.LessOrEqual(t, s2.Peak(), 3)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestUsenetAnArticleNoServerHasIsAskedOfEveryServerOnce(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := oneFile(t, "nobody.bin", 150, 5)
	lost := rel.Files[0].IDs[1]
	ca := newTestCA(t)
	s1 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).Missing(lost)
	s2 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).Missing(lost)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(s1, specPriority(1)), specFor(s2, specPriority(2))})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	waitFetched(t, ev, id)

	assert.Equal(t, 1, s1.Requests(lost))
	assert.Equal(t, 1, s2.Requests(lost))
	final := settle(d, id)
	requireFetchedOrMissingArticles(t, final)
	requireDamageDetail(t, final, 1, 0)
}

func TestUsenetAServerWhoseRetentionIsShorterThanTheArticleAgeIsNotAsked(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := oneFile(t, "old.bin", 160, 6, relDate(time.Now().Add(-100*24*time.Hour)))
	ca := newTestCA(t)
	short := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel)
	long := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel)
	u := newUsenetSetup(t, ca, []serverSpec{
		specFor(short, specPriority(1), specRetention(30)),
		specFor(long, specPriority(2), specRetention(0)),
	})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	waitFetched(t, ev, id)

	u.requireFiles(t, id, rel)
	assert.Zero(t, short.Total(), "the article is older than the retention of the first Server")
	assert.Equal(t, rel.Segments(), long.Total())
}

func TestUsenetADisabledServerIsNeverConnectedTo(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := oneFile(t, "disabled.bin", 170, 5)
	ca := newTestCA(t)
	off := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel)
	on := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(off, specPriority(1), specDisabled()), specFor(on, specPriority(2))})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	waitFetched(t, ev, id)

	u.requireFiles(t, id, rel)
	assert.Zero(t, off.Dials(), "not even a connection")
	assert.Equal(t, rel.Segments(), on.Total())
}

func TestUsenetServersOfTheSamePriorityShareTheLoad(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := oneFile(t, "shared.bin", 180, 40)
	ca := newTestCA(t)
	s1 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).SetLatency(30 * time.Millisecond)
	s2 := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).SetLatency(30 * time.Millisecond)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(s1, specPriority(1), specConns(2)), specFor(s2, specPriority(1), specConns(2))})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	waitFetched(t, ev, id)

	u.requireFiles(t, id, rel)
	assert.Equal(t, rel.Segments(), s1.Total()+s2.Total(), "each article is fetched once, from either Server")
	assert.Positive(t, s1.Total())
	assert.Positive(t, s2.Total())
	assert.LessOrEqual(t, s1.Peak(), 2)
	assert.LessOrEqual(t, s2.Peak(), 2)
}

func TestUsenetTheConnectionsOfAServerNeverExceedItsLimit(t *testing.T) {
	t.Parallel()
	requireCATrust(t)
	rel := oneFile(t, "limit.bin", 190, 30)
	ca := newTestCA(t)
	srv := startNNTP(t, ca).Credentials(serverUsername, serverPassword).Install(rel).SetLatency(20 * time.Millisecond)
	u := newUsenetSetup(t, ca, []serverSpec{specFor(srv, specConns(3))})
	d := u.start()
	ev := d.watch()

	id := d.addRelease(rel)["id"].(string)
	waitFetched(t, ev, id)

	u.requireFiles(t, id, rel)
	assert.Equal(t, 3, srv.Peak(), "all three connections are used when there is work")
}

func TestUsenetAConnectionThatDropsInTheMiddleOfAnArticleIsRetried(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "drop.bin", 200, 8)
	flaky := rel.Files[0].IDs[3]
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).Drop(flaky, 3, 1) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	s.u.requireFiles(t, id, rel)
	requireNotFailed(t, settle(s.d, id))
	assert.Equal(t, 2, s.srv.Requests(flaky), "one drop, one retry")
	assert.Equal(t, 1, s.srv.Completed(flaky))
	for _, a := range rel.IDs() {
		if a != flaky {
			assert.Equal(t, 1, s.srv.Requests(a), a)
		}
	}
}

func TestUsenetAnArticleThatAlwaysDropsTheConnectionIsAskedThreeTimesThenLost(t *testing.T) {
	t.Parallel()
	rel := oneFile(t, "dropper.bin", 210, 6)
	flaky := rel.Files[0].IDs[2]
	s := startOneServer(t, func(s *nntpServer) { s.Install(rel).Drop(flaky, 2, 0) })

	id := s.d.addRelease(rel)["id"].(string)
	waitFetched(t, s.ev, id)

	assert.Equal(t, 3, s.srv.Requests(flaky), "three attempts on a Server, then it counts as tried")
	final := settle(s.d, id)
	requireFetchedOrMissingArticles(t, final)
	requireDamageDetail(t, final, 1, 0)
}
