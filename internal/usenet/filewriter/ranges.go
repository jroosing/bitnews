package filewriter

import (
	"slices"
	"sort"
)

// Range is a half-open, zero-based byte range: offsets Start <= i < End.
type Range struct {
	Start, End int64
}

// Len returns the number of bytes in r.
func (r Range) Len() int64 { return r.End - r.Start }

// Sum returns the total number of bytes covered by rs.
func Sum(rs []Range) int64 {
	var n int64
	for _, r := range rs {
		n += r.Len()
	}
	return n
}

// rangeSet is a sorted list of non-empty, disjoint and non-adjacent ranges.
type rangeSet []Range

// add inserts r, merging it with every range it overlaps or touches.
func (s *rangeSet) add(r Range) {
	if r.Len() <= 0 {
		return
	}
	rs := *s
	// first range that overlaps or touches r from the left
	i := sort.Search(len(rs), func(i int) bool { return rs[i].End >= r.Start })
	// first range that starts strictly after r.End
	j := sort.Search(len(rs), func(j int) bool { return rs[j].Start > r.End })
	if i == j {
		*s = slices.Insert(rs, i, r)
		return
	}
	r.Start = min(r.Start, rs[i].Start)
	r.End = max(r.End, rs[j-1].End)
	rs[i] = r
	*s = slices.Delete(rs, i+1, j)
}

// remove deletes r from the set, splitting ranges that straddle its ends.
func (s *rangeSet) remove(r Range) {
	if r.Len() <= 0 {
		return
	}
	rs := *s
	i := sort.Search(len(rs), func(i int) bool { return rs[i].End > r.Start })
	j := sort.Search(len(rs), func(j int) bool { return rs[j].Start >= r.End })
	if i >= j {
		return
	}
	var keep [2]Range
	n := 0
	if rs[i].Start < r.Start {
		keep[n] = Range{rs[i].Start, r.Start}
		n++
	}
	if rs[j-1].End > r.End {
		keep[n] = Range{r.End, rs[j-1].End}
		n++
	}
	*s = slices.Replace(rs, i, j, keep[:n]...)
}

// gaps returns the parts of r that the set does not cover, in order.
func (s rangeSet) gaps(r Range) []Range {
	if r.Len() <= 0 {
		return nil
	}
	var out []Range
	pos := r.Start
	i := sort.Search(len(s), func(i int) bool { return s[i].End > r.Start })
	for ; i < len(s) && s[i].Start < r.End; i++ {
		if s[i].Start > pos {
			out = append(out, Range{pos, s[i].Start})
		}
		pos = max(pos, s[i].End)
	}
	if pos < r.End {
		out = append(out, Range{pos, r.End})
	}
	return out
}

// minus returns the ranges of s that are not covered by other.
func (s rangeSet) minus(other rangeSet) rangeSet {
	var out rangeSet
	for _, r := range s {
		out = append(out, other.gaps(r)...)
	}
	return out
}

// tracker is the bookkeeping of one file. done, damaged and lost never
// overlap each other.
type tracker struct {
	done, damaged, lost rangeSet
}

// markGood records r as correctly written and clears any damaged or lost
// state inside it.
func (t *tracker) markGood(r Range) {
	t.done.add(r)
	t.damaged.remove(r)
	t.lost.remove(r)
}

// damageable returns the parts of r that a damaged copy may be written to:
// those not already holding a good copy.
func (t *tracker) damageable(r Range) []Range { return t.done.gaps(r) }

// markDamaged records the given pieces (from damageable) as damaged.
func (t *tracker) markDamaged(pieces []Range) {
	for _, p := range pieces {
		t.damaged.add(p)
		t.lost.remove(p)
	}
}

// markLost records the parts of r that are neither done nor damaged as
// permanently unavailable.
func (t *tracker) markLost(r Range) {
	for _, g := range t.damaged.gaps(r) {
		for _, p := range t.done.gaps(g) {
			t.lost.add(p)
		}
	}
}

// missing returns every byte of [0, size) that is neither done nor damaged.
func (t *tracker) missing(size int64) rangeSet {
	return rangeSet(t.damaged.gaps(Range{0, size})).minus(t.done)
}
