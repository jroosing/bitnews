package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jroosing/bitnews/internal/core"
)

// receive returns the next event on sub or fails the test if none arrives.
func receive(t *testing.T, sub *core.Subscription) core.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		require.True(t, ok, "subscription closed")
		return ev
	case <-time.After(time.Second):
		t.Fatal("no event received")
		return nil
	}
}

func TestSubscriberReceivesPublishedEvents(t *testing.T) {
	bus := core.NewBus()
	sub := bus.Subscribe(context.Background())

	bus.Publish(core.Removed{ID: "a"})
	bus.Publish(core.Fetched{ID: "b"})

	assert.Equal(t, core.Removed{ID: "a"}, receive(t, sub))
	assert.Equal(t, core.Fetched{ID: "b"}, receive(t, sub))
}

func TestProgressEventsAreDroppedWhenSubscriberIsFull(t *testing.T) {
	bus := core.NewBus()
	sub := bus.Subscribe(context.Background())

	for range core.SubscriberBuffer {
		bus.Publish(core.DownloadProgress{ID: "a"})
	}
	bus.Publish(core.DownloadProgress{ID: "dropped"}) // must not block or close

	for range core.SubscriberBuffer {
		assert.Equal(t, core.ID("a"), receive(t, sub).(core.DownloadProgress).ID)
	}
	bus.Publish(core.Fetched{ID: "b"})
	assert.Equal(t, core.Fetched{ID: "b"}, receive(t, sub), "the dropped progress event is gone and the subscription is still open")
}

func TestLifecycleOverflowClosesSubscriberWithResync(t *testing.T) {
	bus := core.NewBus()
	sub := bus.Subscribe(context.Background())

	for range core.SubscriberBuffer {
		bus.Publish(core.Fetched{ID: "a"})
	}
	bus.Publish(core.Ready{ID: "a"}) // cannot be delivered

	for range core.SubscriberBuffer {
		assert.Equal(t, core.Fetched{ID: "a"}, receive(t, sub))
	}
	select {
	case _, ok := <-sub.Events():
		assert.False(t, ok, "channel must be closed after the buffered events")
	case <-time.After(time.Second):
		t.Fatal("subscription was not closed")
	}
	assert.ErrorIs(t, sub.Err(), core.ErrResync)
}

func TestCancellingContextUnsubscribesAndClosesChannel(t *testing.T) {
	bus := core.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	sub := bus.Subscribe(ctx)

	cancel()

	select {
	case _, ok := <-sub.Events():
		assert.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("channel was not closed after the context was cancelled")
	}
	assert.NoError(t, sub.Err(), "a subscriber that left by itself is not told to resync")
	bus.Publish(core.Removed{ID: "a"}) // safe after close
}

func TestCloseIsIdempotentAndStopsDelivery(t *testing.T) {
	bus := core.NewBus()
	sub := bus.Subscribe(context.Background())
	other := bus.Subscribe(context.Background())

	sub.Close()
	sub.Close()
	bus.Publish(core.Removed{ID: "a"})

	_, ok := <-sub.Events()
	assert.False(t, ok)
	assert.Equal(t, core.Removed{ID: "a"}, receive(t, other), "other subscribers are unaffected")
}

func TestCloseAfterResyncKeepsTheResyncReason(t *testing.T) {
	bus := core.NewBus()
	sub := bus.Subscribe(context.Background())
	for range core.SubscriberBuffer + 1 {
		bus.Publish(core.Ready{ID: "a"})
	}

	sub.Close()

	assert.ErrorIs(t, sub.Err(), core.ErrResync)
}

func TestSubscribingWithCancelledContextYieldsClosedSubscription(t *testing.T) {
	bus := core.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sub := bus.Subscribe(ctx)

	select {
	case _, ok := <-sub.Events():
		assert.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("channel was not closed")
	}
}

func TestSlowSubscriberNeverBlocksPublishers(t *testing.T) {
	bus := core.NewBus()
	_ = bus.Subscribe(context.Background()) // never reads
	reader := bus.Subscribe(context.Background())

	const publishers, perPublisher = 8, 1000
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for range publishers {
			wg.Go(func() {
				for i := range perPublisher {
					if i%2 == 0 {
						bus.Publish(core.DownloadProgress{ID: "a"})
					} else {
						bus.Publish(core.Fetched{ID: "a"})
					}
				}
			})
		}
		wg.Wait()
	}()

	// Keep one subscriber draining so the test also covers a fast reader
	// alongside the stuck one.
	go func() {
		for range reader.Events() {
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishers blocked behind a slow subscriber")
	}
}

func TestPublishIsRaceFreeWithSubscribeAndClose(t *testing.T) {
	bus := core.NewBus()
	ctx := t.Context()

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 500 {
				bus.Publish(core.Ready{ID: "a"})
				bus.Publish(core.DownloadProgress{ID: "a"})
			}
		})
		wg.Go(func() {
			for range 200 {
				sub := bus.Subscribe(ctx)
				sub.Close()
				sub.Close()
			}
		})
		wg.Go(func() {
			for range 200 {
				c, cancelSub := context.WithCancel(ctx)
				sub := bus.Subscribe(c)
				cancelSub()
				for range sub.Events() {
				}
			}
		})
	}
	wg.Wait()
}
