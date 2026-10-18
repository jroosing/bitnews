package filewriter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	byteNone = iota
	byteDone
	byteDamaged
	byteLost
)

// modelRanges converts a per-byte model into the canonical range set of the
// bytes in state want.
func modelRanges(model []byte, want byte) rangeSet {
	var rs rangeSet
	for i := 0; i < len(model); {
		if model[i] != want {
			i++
			continue
		}
		j := i
		for j < len(model) && model[j] == want {
			j++
		}
		rs = append(rs, Range{int64(i), int64(j)})
		i = j
	}
	return rs
}

func equalSets(a, b rangeSet) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FuzzRanges drives random good/damaged/lost sequences through the tracker
// and compares every step with a naive per-byte model.
func FuzzRanges(f *testing.F) {
	f.Add([]byte{0, 3, 10, 1, 5, 20, 2, 0, 63, 0, 8, 9})
	f.Add([]byte{1, 0, 64, 0, 10, 30, 1, 5, 50})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, ops []byte) {
		const size = 64
		model := make([]byte, size)
		var tr tracker

		for ; len(ops) >= 3; ops = ops[3:] {
			op, a, b := ops[0]%3, int64(ops[1])%(size+1), int64(ops[2])%(size+1)
			if a > b {
				a, b = b, a
			}
			r := Range{a, b}
			switch op {
			case 0: // good copy
				for i := a; i < b; i++ {
					model[i] = byteDone
				}
				tr.markGood(r)
			case 1: // damaged copy: only bytes without a good copy
				var wantPieces rangeSet
				for i := a; i < b; i++ {
					if model[i] != byteDone {
						model[i] = byteDamaged
						wantPieces.add(Range{i, i + 1})
					}
				}
				pieces := tr.damageable(r)
				if !equalSets(rangeSet(pieces), wantPieces) {
					t.Fatalf("damageable(%v) = %v, want %v", r, pieces, wantPieces)
				}
				tr.markDamaged(pieces)
			case 2: // lost
				for i := a; i < b; i++ {
					if model[i] == byteNone {
						model[i] = byteLost
					}
				}
				tr.markLost(r)
			}

			if !equalSets(tr.done, modelRanges(model, byteDone)) ||
				!equalSets(tr.damaged, modelRanges(model, byteDamaged)) ||
				!equalSets(tr.lost, modelRanges(model, byteLost)) {
				t.Fatalf("after op %d %v: done=%v damaged=%v lost=%v, model=%v", op, r, tr.done, tr.damaged, tr.lost, model)
			}
			var wantMissing, wantPending rangeSet
			for i, s := range model {
				if s == byteNone || s == byteLost {
					wantMissing.add(Range{int64(i), int64(i + 1)})
				}
				if s == byteNone {
					wantPending.add(Range{int64(i), int64(i + 1)})
				}
			}
			missing := tr.missing(size)
			if !equalSets(missing, wantMissing) || !equalSets(missing.minus(tr.lost), wantPending) {
				t.Fatalf("missing=%v want %v; pending=%v want %v", missing, wantMissing, missing.minus(tr.lost), wantPending)
			}
			require.True(t, disjoint(tr.done, tr.damaged, tr.lost))
		}
	})
}

// FuzzDecodeSnapshot feeds arbitrary bytes to the snapshot decoder: it must
// never panic, and everything it accepts must be canonical and re-encode to
// the same bytes.
func FuzzDecodeSnapshot(f *testing.F) {
	f.Add(encodeSnapshot(nil))
	f.Add(encodeSnapshot([]fileRecord{{key: 3, name: "a.bin", size: 100, done: rangeSet{{0, 10}, {20, 30}}, damaged: rangeSet{{40, 50}}, lost: rangeSet{{90, 100}}}}))
	f.Add([]byte("GNFW\x01\x00"))

	f.Fuzz(func(t *testing.T, data []byte) {
		recs, err := decodeSnapshot(data)
		if err != nil {
			return
		}
		for _, r := range recs {
			require.True(t, r.size >= 1 && r.size <= MaxFileSize)
			require.True(t, disjoint(r.done, r.damaged, r.lost))
			for _, s := range []rangeSet{r.done, r.damaged, r.lost} {
				var prev int64 = -1
				for _, rg := range s {
					require.True(t, rg.Start >= 0 && rg.End > rg.Start && rg.End <= r.size)
					require.Greater(t, rg.Start, prev)
					prev = rg.End
				}
			}
		}
		require.Equal(t, data, encodeSnapshot(recs))
	})
}
