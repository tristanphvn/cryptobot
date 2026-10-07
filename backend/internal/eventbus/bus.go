// Package eventbus is an in-process, type-erased pub/sub used to decouple
// the exchange ingestion layer from persistence and websocket broadcast
// layers. Subscribers receive events on bounded channels; if a subscriber
// is too slow, events are dropped for that subscriber only — never for
// the publisher and never for other subscribers.
//
// This is the single most important property of the realtime pipeline:
// no consumer can ever stall the producer (the Binance read loop).
package eventbus

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"cryptobot/internal/database/models"
)

// Topic identifiers. Keep them as typed constants so the compiler catches typos.
type Topic string

const (
	TopicCandleTick   Topic = "candle.tick"   // every kline update (open candle)
	TopicCandleClosed Topic = "candle.closed" // only finalized candles
)

// CandleEvent is the unit transferred over the bus for kline data.
type CandleEvent struct {
	Candle   models.Candle
	IsClosed bool
	RecvAt   time.Time
}

// Subscription is what callers receive when they Subscribe. Read from C.
// Always call Close when done to release resources.
type Subscription struct {
	C       <-chan any
	closeFn func()
	dropped *uint64
}

func (s *Subscription) Close()          { s.closeFn() }
func (s *Subscription) Dropped() uint64 { return atomic.LoadUint64(s.dropped) }

type subscriber struct {
	ch      chan any
	dropped *uint64
	name    string
}

// Bus is a many-producer, many-subscriber pub/sub.
type Bus struct {
	mu     sync.RWMutex
	subs   map[Topic]map[*subscriber]struct{}
	closed bool
}

func New() *Bus {
	return &Bus{subs: make(map[Topic]map[*subscriber]struct{})}
}

// Subscribe registers a new consumer for topic. buf is the per-subscriber
// channel buffer — pick it based on expected burst size. name is used in
// drop logs for diagnostics.
func (b *Bus) Subscribe(topic Topic, buf int, name string) *Subscription {
	if buf < 1 {
		buf = 1
	}
	s := &subscriber{
		ch:      make(chan any, buf),
		dropped: new(uint64),
		name:    name,
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return &Subscription{C: s.ch, closeFn: func() {}, dropped: s.dropped}
	}
	if _, ok := b.subs[topic]; !ok {
		b.subs[topic] = make(map[*subscriber]struct{})
	}
	b.subs[topic][s] = struct{}{}
	b.mu.Unlock()

	log.Printf("[bus] subscribed: topic=%s name=%s buf=%d", topic, name, buf)

	closeFn := func() {
		b.mu.Lock()
		if subs, ok := b.subs[topic]; ok {
			if _, present := subs[s]; present {
				delete(subs, s)
				close(s.ch)
			}
		}
		b.mu.Unlock()
		log.Printf("[bus] unsubscribed: topic=%s name=%s dropped=%d", topic, name, atomic.LoadUint64(s.dropped))
	}
	return &Subscription{C: s.ch, closeFn: closeFn, dropped: s.dropped}
}

// Publish sends evt to every subscriber of topic. Non-blocking: if a
// subscriber's channel is full the event is dropped for that subscriber
// and a counter is incremented. Returns immediately.
func (b *Bus) Publish(topic Topic, evt any) {
	b.mu.RLock()
	subs := b.subs[topic]
	// Snapshot under lock to avoid iterating a mutating map.
	receivers := make([]*subscriber, 0, len(subs))
	for s := range subs {
		receivers = append(receivers, s)
	}
	b.mu.RUnlock()

	for _, s := range receivers {
		select {
		case s.ch <- evt:
		default:
			n := atomic.AddUint64(s.dropped, 1)
			// Throttle log spam: log only on powers of two.
			if n&(n-1) == 0 {
				log.Printf("[bus] DROP topic=%s subscriber=%s total_dropped=%d (consumer too slow)", topic, s.name, n)
			}
		}
	}
}

// Close shuts down the bus and closes every subscriber channel.
// Safe to call concurrently with Publish/Subscribe.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, subs := range b.subs {
		for s := range subs {
			close(s.ch)
		}
	}
	b.subs = nil
}

// RunWithContext is a tiny helper: closes the bus when ctx is done.
func (b *Bus) RunWithContext(ctx context.Context) {
	<-ctx.Done()
	b.Close()
}
