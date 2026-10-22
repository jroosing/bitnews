package scheduler

// minHeap is a binary min-heap of Article indices. It is a hand-rolled
// int32 heap to avoid the interface boxing of container/heap on the hot path.
type minHeap []int32

func (h *minHeap) push(v int32) {
	*h = append(*h, v)
	s := *h
	i := len(s) - 1
	for i > 0 {
		p := (i - 1) / 2
		if s[p] <= s[i] {
			break
		}
		s[p], s[i] = s[i], s[p]
		i = p
	}
}

func (h *minHeap) pop() int32 {
	s := *h
	top := s[0]
	n := len(s) - 1
	s[0] = s[n]
	*h = s[:n]
	s = *h
	i := 0
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		m := l
		if r := l + 1; r < n && s[r] < s[l] {
			m = r
		}
		if s[i] <= s[m] {
			break
		}
		s[i], s[m] = s[m], s[i]
		i = m
	}
	return top
}
