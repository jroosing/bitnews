package nzb_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gitlab.com/kyura4k/gitnews/internal/usenet/nzb"
)

func parseFixture(t *testing.T, name string) *nzb.NZB {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := nzb.Parse(f, nzb.DefaultLimits())
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return n
}

func fileNames(n *nzb.NZB) []string {
	var names []string
	for _, f := range n.Files {
		names = append(names, f.Name)
	}
	return names
}

func TestFixture_NewznabWithMeta(t *testing.T) {
	n := parseFixture(t, "newznab_meta.nzb")

	wantMeta := nzb.Meta{Name: "Some.Show.S02E05.1080p.WEB.h264-GRP", Password: "hunter2", Category: "TV > HD"}
	if n.Meta != wantMeta {
		t.Errorf("meta = %+v, want %+v", n.Meta, wantMeta)
	}
	wantNames := []string{
		"some.show.s02e05.1080p.web.h264-grp.nfo",
		"some.show.s02e05.1080p.web.h264-grp.rar",
		"some.show.s02e05.1080p.web.h264-grp.par2",
		"some.show.s02e05.1080p.web.h264-grp.vol00+02.par2",
	}
	if got := fileNames(n); !slices.Equal(got, wantNames) {
		t.Fatalf("names = %q, want %q", got, wantNames)
	}
	wantKinds := []nzb.Kind{nzb.KindData, nzb.KindData, nzb.KindPar2Index, nzb.KindPar2Volume}
	for i, f := range n.Files {
		if f.Kind != wantKinds[i] {
			t.Errorf("file %d kind = %v, want %v", i, f.Kind, wantKinds[i])
		}
	}
	if got := n.Files[3].RecoveryBlocks; got != 2 {
		t.Errorf("volume recovery blocks = %d, want 2", got)
	}

	nfo := n.Files[0]
	if nfo.Poster != "GRP <grp@poster.example>" {
		t.Errorf("poster = %q", nfo.Poster)
	}
	if !nfo.Date.Equal(time.Unix(1712345678, 0)) {
		t.Errorf("date = %v", nfo.Date)
	}
	if want := []string{"alt.binaries.teevee", "alt.binaries.hdtv.x264"}; !slices.Equal(nfo.Groups, want) {
		t.Errorf("groups = %q, want %q", nfo.Groups, want)
	}

	rar := n.Files[1]
	var total int64
	for _, s := range rar.Segments {
		total += s.Bytes
	}
	if len(rar.Segments) != 3 || total != 739948+739912+412088 {
		t.Errorf("rar segments = %d, bytes = %d", len(rar.Segments), total)
	}
	if n.SkippedSegments != 0 {
		t.Errorf("skipped = %d, want 0", n.SkippedSegments)
	}
}

func TestFixture_WithoutMeta(t *testing.T) {
	n := parseFixture(t, "no_meta.nzb")
	if n.Meta != (nzb.Meta{}) {
		t.Errorf("meta = %+v, want zero", n.Meta)
	}
	if len(n.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(n.Files))
	}
	f := n.Files[0]
	if f.Name != "abc-mr2a.r01" || f.Kind != nzb.KindData {
		t.Errorf("name = %q kind = %v, want abc-mr2a.r01 data", f.Name, f.Kind)
	}
	if f.Poster != "Joe Bloggs (bloggs@nowhere.example)" || len(f.Segments) != 2 {
		t.Errorf("poster = %q, segments = %d", f.Poster, len(f.Segments))
	}
}

func TestFixture_ObfuscatedSubjects(t *testing.T) {
	n := parseFixture(t, "obfuscated.nzb")
	wantNames := []string{
		"9f86d081884c7d659a2feaa0c55ad015",
		"9f86d081884c7d659a2feaa0c55ad015.par2",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	if got := fileNames(n); !slices.Equal(got, wantNames) {
		t.Fatalf("names = %q, want %q", got, wantNames)
	}
	wantKinds := []nzb.Kind{nzb.KindData, nzb.KindPar2Index, nzb.KindData}
	for i, f := range n.Files {
		if f.Kind != wantKinds[i] {
			t.Errorf("file %d kind = %v, want %v", i, f.Kind, wantKinds[i])
		}
	}
}

func TestFixture_DuplicateSegments(t *testing.T) {
	n := parseFixture(t, "duplicate_segments.nzb")
	segs := n.Files[0].Segments
	want := []nzb.Segment{
		{Number: 1, Bytes: 716800, MessageID: "part1of4.XyZ@upload.example"},
		{Number: 2, Bytes: 716800, MessageID: "part2of4.XyZ@upload.example"},
		{Number: 3, Bytes: 716800, MessageID: "part3of4.XyZ@reposted.example"},
	}
	if !slices.Equal(segs, want) {
		t.Errorf("segments = %+v\nwant %+v", segs, want)
	}
	if n.SkippedSegments != 1 {
		t.Errorf("skipped = %d, want 1 (the broken id)", n.SkippedSegments)
	}
	if n.Files[0].Name != "album.2020.flac-grp.7z.001" {
		t.Errorf("name = %q", n.Files[0].Name)
	}
}

func TestFixture_MissingBytes(t *testing.T) {
	n := parseFixture(t, "missing_bytes.nzb")
	segs := n.Files[0].Segments
	if len(segs) != 2 {
		t.Fatalf("segments = %d, want 2", len(segs))
	}
	for _, s := range segs {
		if s.Bytes != 0 {
			t.Errorf("segment %d bytes = %d, want 0", s.Number, s.Bytes)
		}
	}
}

func TestFixture_Par2IndexAndVolumes(t *testing.T) {
	n := parseFixture(t, "par2_set.nzb")
	type kb struct {
		kind   nzb.Kind
		blocks int
	}
	want := []kb{
		{nzb.KindData, 0},
		{nzb.KindPar2Index, 0},
		{nzb.KindPar2Volume, 1},
		{nzb.KindPar2Volume, 2},
		{nzb.KindPar2Volume, 4},
	}
	var got []kb
	for _, f := range n.Files {
		got = append(got, kb{f.Kind, f.RecoveryBlocks})
	}
	if !slices.Equal(got, want) {
		t.Errorf("kinds = %+v, want %+v", got, want)
	}
}
