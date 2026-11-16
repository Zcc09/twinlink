// Package sched implements the Twinlink v1 outbound scheduler (PROTOCOL §5):
// deficit round robin over per-link weights derived from measured throughput.
//
// The package is pure Go with an injectable clock so its behavior is exactly
// reproducible in tests.
package sched

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Tunables from PROTOCOL §5.
const (
	DefaultMinWeightBps     = 64000.0
	DefaultAliveWindow      = 3 * time.Second
	DefaultSampleInterval   = 500 * time.Millisecond
	DefaultRateAlpha        = 0.3
	DefaultRTTAlpha         = 0.2
	DefaultCreditCapSeconds = 0.25
	creditEpsilon           = 1e-9
)

// Link is the observable state of one scheduler link.
type Link struct {
	ID       uint8
	RateBps  float64
	RTT      time.Duration
	LastSeen time.Time // zero means "never heard from"

	credit      float64
	windowBytes uint64
	windowStart time.Time
}

// Snapshot is a copy of a link's state, safe to keep after the lock is released.
type Snapshot struct {
	ID        uint8
	RateBps   float64
	WeightBps float64
	RTT       time.Duration
	Alive     bool
	Credit    float64
}

// Scheduler is a deficit-round-robin arbiter over a fixed set of links.
type Scheduler struct {
	mu    sync.Mutex
	now   func() time.Time
	links map[uint8]*Link
	order []uint8

	lastTick         time.Time
	minWeight        float64
	aliveWindow      time.Duration
	sampleInterval   time.Duration
	rateAlpha        float64
	rttAlpha         float64
	creditCapSeconds float64
}

// Option customises a Scheduler.
type Option func(*Scheduler)

// WithClock injects a clock (used by tests and by replay tooling).
func WithClock(now func() time.Time) Option { return func(s *Scheduler) { s.now = now } }

// WithMinWeight overrides the 64 kbps floor.
func WithMinWeight(bps float64) Option { return func(s *Scheduler) { s.minWeight = bps } }

// WithAliveWindow overrides the 3 s liveness window.
func WithAliveWindow(d time.Duration) Option { return func(s *Scheduler) { s.aliveWindow = d } }

// WithSampleInterval overrides the 500 ms rate sampling interval.
func WithSampleInterval(d time.Duration) Option { return func(s *Scheduler) { s.sampleInterval = d } }

// New returns an empty scheduler.
func New(opts ...Option) *Scheduler {
	s := &Scheduler{
		now:              time.Now,
		links:            make(map[uint8]*Link),
		minWeight:        DefaultMinWeightBps,
		aliveWindow:      DefaultAliveWindow,
		sampleInterval:   DefaultSampleInterval,
		rateAlpha:        DefaultRateAlpha,
		rttAlpha:         DefaultRTTAlpha,
		creditCapSeconds: DefaultCreditCapSeconds,
	}
	for _, o := range opts {
		o(s)
	}
	s.lastTick = s.now()
	return s
}

// Add registers a link. Adding an existing id is a no-op.
func (s *Scheduler) Add(id uint8) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.links[id]; ok {
		return
	}
	s.links[id] = &Link{ID: id, windowStart: s.now()}
	s.order = append(s.order, id)
	sort.Slice(s.order, func(i, j int) bool { return s.order[i] < s.order[j] })
}

// Remove drops a link (session teardown).
func (s *Scheduler) Remove(id uint8) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.links[id]; !ok {
		return
	}
	delete(s.links, id)
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// MarkAlive records that a datagram arrived on the link right now.
func (s *Scheduler) MarkAlive(id uint8) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[id]; ok {
		l.LastSeen = s.now()
	}
}

// MarkSeenAt records activity at an explicit instant (tests).
func (s *Scheduler) MarkSeenAt(id uint8, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[id]; ok {
		l.LastSeen = t
	}
}

// ObserveRTT folds a fresh round-trip measurement into the link's EWMA.
func (s *Scheduler) ObserveRTT(id uint8, rtt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[id]
	if !ok {
		return
	}
	if l.RTT == 0 {
		l.RTT = rtt
		return
	}
	l.RTT = time.Duration(float64(l.RTT)*(1-s.rttAlpha) + float64(rtt)*s.rttAlpha)
}

// SetRTT sets the link RTT outright (tests, or a first measurement).
func (s *Scheduler) SetRTT(id uint8, rtt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[id]; ok {
		l.RTT = rtt
	}
}

// SetRateBps sets the link throughput estimate outright (tests, or an external
// estimator feeding the scheduler).
func (s *Scheduler) SetRateBps(id uint8, bps float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[id]; ok {
		l.RateBps = bps
	}
}

// AddSentBytes records bytes handed to the link in the current sample window.
// This is the "achieved throughput" input of PROTOCOL §5: with a blocking
// socket write, bytes written per window cannot exceed what the link drains.
func (s *Scheduler) AddSentBytes(id uint8, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.links[id]; ok {
		l.windowBytes += uint64(n)
	}
}

// Sample folds any completed 500 ms windows into the rate EWMAs.
func (s *Scheduler) Sample() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sampleLocked(s.now())
}

func (s *Scheduler) sampleLocked(now time.Time) {
	for _, id := range s.order {
		l := s.links[id]
		elapsed := now.Sub(l.windowStart)
		if elapsed < s.sampleInterval {
			continue
		}
		secs := elapsed.Seconds()
		if secs <= 0 {
			continue
		}
		measured := float64(l.windowBytes) * 8 / secs
		l.RateBps = s.rateAlpha*measured + (1-s.rateAlpha)*l.RateBps
		l.windowBytes = 0
		l.windowStart = now
	}
}

// Weight returns the link weight in bits/s: max(rate, 64 kbps) when alive, else 0.
func (s *Scheduler) Weight(id uint8) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[id]
	if !ok || !s.aliveLocked(l, s.now()) {
		return 0
	}
	if l.RateBps < s.minWeight {
		return s.minWeight
	}
	return l.RateBps
}

// Alive reports whether the link has been heard from inside the 3 s window.
func (s *Scheduler) Alive(id uint8) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[id]
	return ok && s.aliveLocked(l, s.now())
}

func (s *Scheduler) aliveLocked(l *Link, now time.Time) bool {
	if l.LastSeen.IsZero() {
		return false
	}
	return now.Sub(l.LastSeen) <= s.aliveWindow
}

// Ready reports whether any link could take a packet right now.
func (s *Scheduler) Ready(sendable func(uint8) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		l := s.links[id]
		if s.aliveLocked(l, s.now()) && (sendable == nil || sendable(id)) {
			return true
		}
	}
	return false
}

// Pick chooses the link for a packet of bits bits and charges it to that link's
// deficit, per PROTOCOL §5.
//
// sendable, when non-nil, filters links that cannot accept more data right now
// (a full transmit queue). Filtering before the selection is what keeps the
// relay work-conserving without dropping packets: a saturated link simply
// yields to the others and its credit keeps accruing.
func (s *Scheduler) Pick(bits float64, sendable func(uint8) bool) (uint8, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.sampleLocked(now)

	dt := now.Sub(s.lastTick).Seconds()
	if dt > 0 {
		s.lastTick = now
		for _, id := range s.order {
			l := s.links[id]
			if !s.aliveLocked(l, now) {
				l.credit = 0
				continue
			}
			w := l.RateBps
			if w < s.minWeight {
				w = s.minWeight
			}
			l.credit += w * dt
			if cap := w * s.creditCapSeconds; l.credit > cap {
				l.credit = cap
			}
		}
	}

	var best *Link
	for _, id := range s.order {
		l := s.links[id]
		if !s.aliveLocked(l, now) {
			continue
		}
		if sendable != nil && !sendable(id) {
			continue
		}
		if best == nil {
			best = l
			continue
		}
		switch {
		case l.credit > best.credit+creditEpsilon:
			best = l
		case math.Abs(l.credit-best.credit) <= creditEpsilon:
			// Tie: lower RTT wins, then lower link id.
			if l.RTT < best.RTT || (l.RTT == best.RTT && l.ID < best.ID) {
				best = l
			}
		}
	}
	if best == nil {
		return 0, false
	}
	best.credit -= bits
	return best.ID, true
}

// Snapshot returns the state of every link, in id order.
func (s *Scheduler) Snapshot() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Snapshot, 0, len(s.order))
	for _, id := range s.order {
		l := s.links[id]
		w := 0.0
		if s.aliveLocked(l, now) {
			w = l.RateBps
			if w < s.minWeight {
				w = s.minWeight
			}
		}
		out = append(out, Snapshot{
			ID:        l.ID,
			RateBps:   l.RateBps,
			WeightBps: w,
			RTT:       l.RTT,
			Alive:     s.aliveLocked(l, now),
			Credit:    l.credit,
		})
	}
	return out
}

// Len returns the number of registered links.
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.links)
}
