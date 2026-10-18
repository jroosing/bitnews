package filewriter

import (
	"container/list"
	"sync"
)

// pool bounds the number of open file handles with an LRU. An entry is
// evicted only when its lock can be taken without waiting, so a handle is
// never closed under a writer that is using it, and eviction can never
// deadlock with a goroutine that holds another entry's lock.
type pool struct {
	max int

	mu  sync.Mutex
	lru *list.List // of *entry, most recently used first
}

func newPool(max int) *pool { return &pool{max: max, lru: list.New()} }

// use marks e as open and most recently used and returns eviction
// candidates (least recently used first) if the pool is over its limit.
// The caller holds e.mu.
func (p *pool) use(e *entry) []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.elem == nil {
		e.elem = p.lru.PushFront(e)
	} else {
		p.lru.MoveToFront(e.elem)
	}
	if p.lru.Len() <= p.max {
		return nil
	}
	var cands []*entry
	for el := p.lru.Back(); el != nil; el = el.Prev() {
		if c := el.Value.(*entry); c != e {
			cands = append(cands, c)
		}
	}
	return cands
}

// drop removes e from the LRU. The caller holds e.mu.
func (p *pool) drop(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.elem != nil {
		p.lru.Remove(e.elem)
		e.elem = nil
	}
}

func (p *pool) over() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lru.Len() > p.max
}
