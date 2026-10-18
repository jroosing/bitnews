package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/kyura4k/gitnews/internal/usenet/filewriter"
	"gitlab.com/kyura4k/gitnews/internal/usenet/nzb"
	"gitlab.com/kyura4k/gitnews/internal/usenet/scheduler"
)

// segKey identifies one Article of a Download: the file index in the NZB and
// the segment number.
type segKey struct {
	File int
	Seg  int
}

// plan is what a fetch has to do for one NZB.
type plan struct {
	// articles are all Articles to fetch, in dispatch order.
	articles []scheduler.Article
	byKey    map[segKey]scheduler.Article
	// files counts the fetched files (deferred ones excluded).
	files int
	// encoded is the sum of the NZB segment sizes of the fetched files.
	encoded int64
	// filesWithArticles lists the file keys that have Articles.
	fileKeys []int
	// segsOf counts the Articles of each fetched file.
	segsOf map[int]int
}

// buildPlan turns n into Articles. Recovery volumes (.volNN+MM.par2) are
// deferred: M3-07 un-defers them when Repair needs more blocks.
func buildPlan(n *nzb.NZB) plan {
	p := plan{byKey: map[segKey]scheduler.Article{}, segsOf: map[int]int{}}
	for fi, f := range n.Files {
		if f.Kind == nzb.KindPar2Volume || len(f.Segments) == 0 {
			continue
		}
		p.files++
		p.fileKeys = append(p.fileKeys, fi)
		for _, s := range f.Segments {
			a := scheduler.Article{
				FileKey: fi, SegmentNumber: s.Number, MessageID: s.MessageID,
				ExpectedBytes: s.Bytes, Date: f.Date,
			}
			p.articles = append(p.articles, a)
			p.byKey[segKey{fi, s.Number}] = a
			p.encoded += s.Bytes
			p.segsOf[fi]++
		}
	}
	return p
}

// remaining returns the Articles whose segment is not resolved yet.
func (p plan) remaining(resolved map[segKey]segStatus) []scheduler.Article {
	out := make([]scheduler.Article, 0, len(p.articles))
	for _, a := range p.articles {
		if _, done := resolved[segKey{a.FileKey, a.SegmentNumber}]; !done {
			out = append(out, a)
		}
	}
	return out
}

// segStatus is the final outcome of one Article.
type segStatus uint8

const (
	segDone segStatus = iota + 1
	segDamaged
	segMissing
)

// wipeDir deletes the files a previous, unrecoverable run left directly in
// dir, keeping the stored NZB. Directories are left alone.
func wipeDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runner: read %s: %w", dir, err)
	}
	for _, e := range ents {
		if e.IsDir() || strings.EqualFold(filepath.Ext(e.Name()), ".nzb") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("runner: clear %s: %w", dir, err)
		}
	}
	return nil
}

// fileKey converts an NZB file index to the writer's key.
func fileKey(i int) filewriter.FileKey { return filewriter.FileKey(i) }
