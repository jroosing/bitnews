package gf16

import (
	"runtime"
	"sync"
)

// minParallelRange is the smallest byte range handed to one goroutine.
// Below it, goroutine start-up costs more than the work.
const minParallelRange = 64 << 10

// parallelRanges splits [0, n) into contiguous ranges, each a multiple of 8
// bytes except possibly the last, and calls fn for each range on its own
// goroutine. It returns when all calls have returned. Ranges never overlap,
// so fn may write to its range of a shared buffer without synchronisation.
func parallelRanges(n int, fn func(lo, hi int)) {
	workers := min(runtime.GOMAXPROCS(0), n/minParallelRange)
	if workers <= 1 {
		fn(0, n)
		return
	}
	step := (n/workers + 7) &^ 7
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += step {
		hi := min(lo+step, n)
		wg.Go(func() { fn(lo, hi) })
	}
	wg.Wait()
}

// parallelItems calls fn(i) for every i in [0, n) using the given number of
// goroutines, and returns when all calls have returned.
func parallelItems(n, workers int, fn func(i int)) {
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := w; i < n; i += workers {
				fn(i)
			}
		})
	}
	wg.Wait()
}

// MulAddParallel is MulAdd split across goroutines, one per contiguous range
// of the buffers. It is worthwhile for buffers of a few hundred KiB or more.
func MulAddParallel(dst, src []byte, c uint16) error {
	if len(dst) != len(src) || len(src)%2 != 0 {
		return ErrLength
	}
	t := newMulTable(c)
	parallelRanges(len(src), func(lo, hi int) { t.mulAdd(dst[lo:hi], src[lo:hi]) })
	return nil
}
