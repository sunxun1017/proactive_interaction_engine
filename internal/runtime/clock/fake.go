package clock

import (
	"sync"
	"time"

	"proactive-interaction-engine/internal/application/port"
)

// Fake advances only when Advance is called.
type Fake struct {
	mu        sync.Mutex
	now       time.Time
	schedules []*schedule
}

type schedule struct {
	clock    *Fake
	channel  chan time.Time
	next     time.Time
	interval time.Duration
	stopped  bool
}

type fakeTimer struct{ schedule *schedule }
type fakeTicker struct{ schedule *schedule }

func NewFake(now time.Time) *Fake { return &Fake{now: now} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) NewTimer(duration time.Duration) port.Timer {
	return fakeTimer{schedule: f.add(duration, 0)}
}

func (f *Fake) NewTicker(duration time.Duration) port.Ticker {
	return fakeTicker{schedule: f.add(duration, duration)}
}

func (f *Fake) add(duration, interval time.Duration) *schedule {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := &schedule{
		clock:    f,
		channel:  make(chan time.Time, 1),
		next:     f.now.Add(duration),
		interval: interval,
	}
	f.schedules = append(f.schedules, item)
	return item
}

// Advance moves time forward and delivers due timers without wall-clock waits.
func (f *Fake) Advance(duration time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if duration < 0 {
		panic("fake clock cannot move backwards")
	}
	f.now = f.now.Add(duration)
	for _, item := range f.schedules {
		if item.stopped || item.next.After(f.now) {
			continue
		}
		select {
		case item.channel <- item.next:
		default:
		}
		if item.interval == 0 {
			item.stopped = true
			continue
		}
		for !item.next.After(f.now) {
			item.next = item.next.Add(item.interval)
		}
	}
}

func (t fakeTimer) C() <-chan time.Time  { return t.schedule.channel }
func (t fakeTimer) Stop() bool           { return t.schedule.stop() }
func (t fakeTicker) C() <-chan time.Time { return t.schedule.channel }
func (t fakeTicker) Stop()               { t.schedule.stop() }

func (s *schedule) stop() bool {
	s.clock.mu.Lock()
	defer s.clock.mu.Unlock()
	wasActive := !s.stopped
	s.stopped = true
	return wasActive
}
