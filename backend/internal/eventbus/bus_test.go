package eventbus

import (
	"sync"
	"testing"
	"time"
)

func TestPublishSubscribeRoundtrip(t *testing.T) {
	b := New()
	defer b.Close()

	sub := b.Subscribe(TopicCandleTick, 4, "test")
	defer sub.Close()

	b.Publish(TopicCandleTick, "hello")

	select {
	case got := <-sub.C:
		if got != "hello" {
			t.Fatalf("got %v want hello", got)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive event")
	}
}

func TestDropSlowSubscriberIncrementsCounter(t *testing.T) {
	b := New()
	defer b.Close()

	// buf=1: any second publish before drain must drop.
	sub := b.Subscribe(TopicCandleTick, 1, "slow")
	defer sub.Close()

	for i := 0; i < 100; i++ {
		b.Publish(TopicCandleTick, i)
	}

	if got := sub.Dropped(); got == 0 {
		t.Fatalf("expected dropped > 0, got 0")
	}
}

func TestPublishDoesNotBlockOnFullSubscriber(t *testing.T) {
	b := New()
	defer b.Close()

	sub := b.Subscribe(TopicCandleTick, 1, "blocker")
	defer sub.Close()

	// Fill the subscriber to capacity but never drain.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(TopicCandleTick, i)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked despite full subscriber — drop-slow policy broken")
	}
}

func TestCloseIsIdempotentAndClosesChannels(t *testing.T) {
	b := New()
	sub := b.Subscribe(TopicCandleTick, 1, "sub")

	b.Close()
	b.Close() // second call must not panic

	// Channel must be closed; reading returns zero value + ok=false.
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("expected subscriber channel closed after bus.Close")
		}
	case <-time.After(time.Second):
		t.Fatal("read on closed subscriber channel hung")
	}
}

func TestSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	b := New()
	b.Close()

	sub := b.Subscribe(TopicCandleTick, 1, "late")
	defer sub.Close()

	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("expected closed channel for post-Close subscription")
		}
	case <-time.After(time.Second):
		t.Fatal("subscribe-after-close did not return a closed channel")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := New()
	defer b.Close()

	sub := b.Subscribe(TopicCandleTick, 8, "unsub")
	b.Publish(TopicCandleTick, "before")

	// Drain.
	<-sub.C
	sub.Close()

	// After close, Publish must not panic and the subscriber must not
	// receive anything (channel is closed).
	b.Publish(TopicCandleTick, "after")

	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("received event after Close")
		}
	case <-time.After(100 * time.Millisecond):
		// Acceptable: channel was already drained-and-closed.
	}
}

func TestConcurrentPubSubNoRace(t *testing.T) {
	b := New()
	defer b.Close()

	const subs = 8
	const pubsPer = 200

	var wg sync.WaitGroup
	subscriptions := make([]*Subscription, subs)
	for i := 0; i < subs; i++ {
		subscriptions[i] = b.Subscribe(TopicCandleTick, 32, "concurrent")
	}
	defer func() {
		for _, s := range subscriptions {
			s.Close()
		}
	}()

	// Drain everything in the background.
	for _, s := range subscriptions {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case _, ok := <-s.C:
					if !ok {
						return
					}
				case <-time.After(500 * time.Millisecond):
					return
				}
			}
		}()
	}

	// Many publishers.
	var pubWg sync.WaitGroup
	for i := 0; i < 4; i++ {
		pubWg.Add(1)
		go func() {
			defer pubWg.Done()
			for j := 0; j < pubsPer; j++ {
				b.Publish(TopicCandleTick, j)
			}
		}()
	}
	pubWg.Wait()
	wg.Wait()
}
