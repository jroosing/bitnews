package core

import (
	"context"
	"errors"
	"sync"
)

// ErrResync is the reason a Subscription is closed with when the Bus could
// not deliver a lifecycle event to it. The subscriber missed events, so it
// must refetch its state: SSE clients reconnect.
var ErrResync = errors.New("subscriber missed events: resync")

// Bus delivers Events to subscribers inside the process. Create one with
// NewBus.
type Bus struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// NewBus returns an empty Bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[*Subscription]struct{})}
}

// Subscription is one subscriber's view of a Bus.
type Subscription struct {
	bus    *Bus
	events chan Event

	mu   sync.Mutex // guards err and stop
	err  error
	stop func() bool // detaches the subscription from its context
}

// SubscriberBuffer is the capacity of each subscriber's channel.
const SubscriberBuffer = 256

// Subscribe registers a new subscriber that receives every Event published
// from now on. The subscription ends, and its channel is closed, when ctx is
// cancelled, when Close is called, or when the Bus closes it with ErrResync.
func (b *Bus) Subscribe(ctx context.Context) *Subscription {
	s := &Subscription{bus: b, events: make(chan Event, SubscriberBuffer)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	stop := context.AfterFunc(ctx, s.Close)
	s.mu.Lock()
	s.stop = stop
	s.mu.Unlock()
	return s
}

// Close ends the subscription and closes its channel. It is safe to call more
// than once and concurrently with Publish. Close does not change the reason
// returned by Err if the Bus already closed the subscription.
func (s *Subscription) Close() {
	s.mu.Lock()
	stop := s.stop
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.bus.remove(s, nil)
}

// Err says why the subscription ended. It returns nil while the subscription
// is open and after the subscriber closed it itself, and ErrResync once the
// Bus closed it for missing a lifecycle event. Read it after Events is
// closed.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Events returns the channel the subscriber reads from.
func (s *Subscription) Events() <-chan Event { return s.events }

// Publish hands ev to every subscriber and never blocks. A subscriber whose
// buffer is full misses ev: silently if it is a progress event, otherwise
// the Bus closes that subscriber with ErrResync.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.events <- ev:
		default:
			if !ev.Kind().IsProgress() {
				b.remove(s, ErrResync)
			}
		}
	}
}

// remove closes s with the given reason, unless it is already closed. The
// caller holds b.mu.
func (b *Bus) remove(s *Subscription, reason error) {
	if _, ok := b.subs[s]; !ok {
		return
	}
	delete(b.subs, s)
	s.mu.Lock()
	s.err = reason
	s.mu.Unlock()
	close(s.events)
}
