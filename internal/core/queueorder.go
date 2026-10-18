package core

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// compareQueueOrder orders Downloads of one Queue by QueuePosition, then
// AddedAt, then ID, so that the order is total and deterministic even when
// positions are duplicated or have gaps.
func compareQueueOrder(a, b Download) int {
	return cmp.Or(
		cmp.Compare(a.QueuePosition, b.QueuePosition),
		a.AddedAt.Compare(b.AddedAt),
		strings.Compare(string(a.ID), string(b.ID)),
	)
}

// MoveToTop returns the QueuePositions of a Queue's Downloads after id is
// moved in front of all the others. See reorder for the shared rules.
func MoveToTop(downloads []Download, id ID) (map[ID]int, error) {
	return reorder(downloads, id, func(_, _ int) int { return 0 })
}

// reorder moves id to the index target(i, n) of the Queue order, where i is
// its current index and n the number of Downloads. The Downloads are first
// ordered by QueuePosition, AddedAt and ID, so duplicated positions and gaps
// in the input are tolerated. The result assigns every Download a dense
// position 0..n-1 and keeps the others in their relative order. It returns
// an error wrapping ErrNotFound if no Download has the given id.
func reorder(downloads []Download, id ID, target func(i, n int) int) (map[ID]int, error) {
	ordered := slices.Clone(downloads)
	slices.SortFunc(ordered, compareQueueOrder)
	i := slices.IndexFunc(ordered, func(d Download) bool { return d.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("core: download %q is not in the queue: %w", id, ErrNotFound)
	}
	moved := ordered[i]
	ordered = slices.Delete(ordered, i, i+1)
	ordered = slices.Insert(ordered, target(i, len(downloads)), moved)
	positions := make(map[ID]int, len(ordered))
	for p, d := range ordered {
		positions[d.ID] = p
	}
	return positions, nil
}

// MoveToBottom returns the QueuePositions of a Queue's Downloads after id is
// moved behind all the others. See reorder for the shared rules.
func MoveToBottom(downloads []Download, id ID) (map[ID]int, error) {
	return reorder(downloads, id, func(_, n int) int { return n - 1 })
}

// MoveUp returns the QueuePositions of a Queue's Downloads after id is moved
// one place towards the top. A Download that is already first stays there.
// See reorder for the shared rules.
func MoveUp(downloads []Download, id ID) (map[ID]int, error) {
	return reorder(downloads, id, func(i, _ int) int { return max(i-1, 0) })
}

// MoveDown returns the QueuePositions of a Queue's Downloads after id is moved
// one place towards the bottom. A Download that is already last stays there.
// See reorder for the shared rules.
func MoveDown(downloads []Download, id ID) (map[ID]int, error) {
	return reorder(downloads, id, func(i, n int) int { return min(i+1, n-1) })
}
