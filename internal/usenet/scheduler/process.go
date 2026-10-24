package scheduler

import (
	"context"
	"time"

	"github.com/jroosing/bitnews/internal/usenet/yenc"
)

// process fetches Article i, which sv's worker holds in flight, and records
// the outcome.
func (s *Scheduler) process(ctx context.Context, sv *srv, i int32) {
	art := s.articles[i]
	var lastErr error
	for attempt := 1; ; attempt++ {
		part, oc, err := s.fetch(ctx, sv, art.MessageID)
		switch oc {
		case outCancelled:
			return
		case outValid:
			s.finishValid(sv, i, part)
			return
		case outDamaged:
			s.finishTried(sv, i, &part, err)
			return
		case outDead:
			s.finishDead(sv, i, err)
			return
		case outTransient:
			lastErr = err
			if attempt < MaxAttempts {
				if !sleep(ctx, s.cfg.Backoff(attempt)) {
					return
				}
				continue
			}
		}
		if lastErr == nil || oc == outAbsent {
			lastErr = err
		}
		s.finishTried(sv, i, nil, lastErr)
		return
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// finishValid resolves the Article with a good copy from sv.
func (s *Scheduler) finishValid(sv *srv, i int32, part yenc.Part) {
	s.mu.Lock()
	s.arts[i].state = stateResolved
	delete(s.damaged, i)
	delete(s.lastErr, i)
	s.done.Add(1)
	s.resolved()
	s.mu.Unlock()
	s.cfg.OnResult(Result{Article: s.articles[i], Status: Done, Part: &part, Server: sv.cfg.Name})
}

// finishTried records that sv is done with Article i (keeping a damaged copy
// if it is the best so far) and moves the Article on.
func (s *Scheduler) finishTried(sv *srv, i int32, damaged *yenc.Part, err error) {
	var out []Result
	s.mu.Lock()
	s.arts[i].tried |= sv.bit
	if damaged != nil {
		if d := s.damaged[i]; d == nil || len(damaged.Data) > len(d.part.Data) {
			s.damaged[i] = &damagedCopy{part: *damaged, server: sv.cfg.Name}
		}
	}
	if err != nil {
		s.lastErr[i] = err
	}
	s.settle(i, true, &out)
	s.mu.Unlock()
	s.emit(out)
}

// finishDead handles a Server that turned out to be unusable.
func (s *Scheduler) finishDead(sv *srv, i int32, err error) {
	var out []Result
	s.mu.Lock()
	first := s.markDead(sv, &out)
	s.arts[i].tried |= sv.bit
	s.lastErr[i] = err
	s.settle(i, true, &out)
	s.mu.Unlock()
	s.emit(out)
	if first {
		s.cfg.OnServerDown(sv.cfg.Name, err)
	}
}

// markDead declares sv dead and re-examines the pending Articles of its level
// when nobody alive is left there to take them. It reports whether this call
// made the transition. The caller holds s.mu.
func (s *Scheduler) markDead(sv *srv, out *[]Result) bool {
	if sv.dead {
		return false
	}
	sv.dead = true
	for _, o := range s.servers {
		o.cond.Broadcast()
	}
	for i := range s.arts {
		a := &s.arts[i]
		if a.state == statePending && a.level == sv.level {
			s.settle(int32(i), false, out)
		}
	}
	return true
}
