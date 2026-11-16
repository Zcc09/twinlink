// Package reorder restores packet order for a sequence of DATA messages
// (PROTOCOL §6). It is pure Go with an injectable clock.
//
// A Buffer is not internally synchronized: the owner serialises access, which
// also guarantees the emission order matches the order packets are written to
// the tunnel.
package reorder

import (
	"container/heap"
	"time"

	"twinlink/relay/internal/proto"
)

// Bounds from PROTOCOL §6.
const (
	DefaultHold   = 40 * time.Millisecond
	DefaultMaxGap = uint32(64)
	// DefaultLimit caps the pending set even if the gap logic is bypassed by a
	// pathological sequence pattern. Reaching it forces a skip forward.
	DefaultLimit = 1024
	// DefaultFreezeGuard is the silence threshold above which the receiver
	// assumes *it* stopped running rather than the network losing a packet.
	// A suspended process (Android Doze, host stall, long preemption) sees every
	// pending packet as ancient on resume; without this guard the first drain
	// after the stall skips a hole whose packet is sitting in the other link's
	// socket buffer, i.e. every resume costs a burst of losses. A hold period of
	// silence cannot have delivered anything anyway, so nothing is lost by
	// restarting the hold clock then. Continuous traffic never trips it: at any
	// realistic tunnel rate arrivals are sub-millisecond apart.
	DefaultFreezeGuard = DefaultHold
)

// Stats counts what the buffer did.
type Stats struct {
	Delivered  uint64
	Duplicates uint64
	Skipped    uint64 // holes given up on
	Buffered   int
	// MaxHold is the largest age a hole reached before being skipped. It is the
	// number to look at when tuning: a hold that keeps hitting HOLD_MS means the
	// links' latency spread is eating the whole budget.
	MaxHold time.Duration
	// FreezeGuards counts how often the buffer recognised a receiver stall and
	// refreshed its pending ages instead of skipping.
	FreezeGuards uint64
	// Skip reasons, for diagnostics.
	SkippedByAge   uint64
	SkippedByGap   uint64
	SkippedByLimit uint64
}

type entry struct {
	seq     uint32
	payload []byte
	arrived time.Time
}

// bySeq orders entries wrap-safely. All entries live inside a window far
// smaller than 2^31, so the signed difference is exact.
type bySeq []*entry

func (h bySeq) Len() int           { return len(h) }
func (h bySeq) Less(i, j int) bool { return int32(h[i].seq-h[j].seq) < 0 }
func (h bySeq) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *bySeq) Push(x any)        { *h = append(*h, x.(*entry)) }
func (h *bySeq) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return e
}

// Buffer restores order.
type Buffer struct {
	hold   time.Duration
	maxGap uint32
	limit  int
	now    func() time.Time

	started     bool
	expected    uint32
	pending     bySeq
	seen        map[uint32]struct{}
	stats       Stats
	freezeGuard time.Duration
	lastArrival time.Time
	guardSpent  bool      // one refresh per quiet period, so a hole still expires
	graceUntil  time.Time // no age-based skips until the backlog has drained
}

// Option customises a Buffer.
type Option func(*Buffer)

// WithClock injects a clock.
func WithClock(now func() time.Time) Option { return func(b *Buffer) { b.now = now } }

// WithHold overrides HOLD_MS.
func WithHold(d time.Duration) Option { return func(b *Buffer) { b.hold = d } }

// WithMaxGap overrides MAX_GAP.
func WithMaxGap(g uint32) Option { return func(b *Buffer) { b.maxGap = g } }

// WithLimit overrides the pending-set hard cap.
func WithLimit(n int) Option { return func(b *Buffer) { b.limit = n } }

// WithFreezeGuard enables the receiver-stall guard: if no packet at all has
// arrived for d, pending ages are refreshed once instead of being treated as
// loss. d=0 disables it, which is the strict PROTOCOL §6 behaviour used by the
// protocol conformance tests.
func WithFreezeGuard(d time.Duration) Option { return func(b *Buffer) { b.freezeGuard = d } }

// WithStartSeq fixes the sequence number the stream begins at and marks the
// buffer as started.
//
// PROTOCOL §2: DATA sequence numbers start at 0 for every session and direction,
// so a receiver that is at the start of a session should say so. Without it the
// first *arriving* packet defines the baseline, and a reordered first packet is
// then indistinguishable from a stale duplicate and gets dropped.
func WithStartSeq(seq uint32) Option {
	return func(b *Buffer) {
		b.started = true
		b.expected = seq
	}
}

// New returns an empty buffer with the protocol defaults.
func New(opts ...Option) *Buffer {
	b := &Buffer{
		hold:   DefaultHold,
		maxGap: DefaultMaxGap,
		limit:  DefaultLimit,
		now:    time.Now,
		seen:   make(map[uint32]struct{}, 128),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Expected returns the next sequence number the buffer will release.
func (b *Buffer) Expected() uint32 { return b.expected }

// Started reports whether any packet has been seen.
func (b *Buffer) Started() bool { return b.started }

// Stats returns a copy of the counters.
func (b *Buffer) Stats() Stats {
	s := b.stats
	s.Buffered = len(b.pending)
	return s
}

// Push inserts a DATA packet. It reports false when the packet is a duplicate
// and was therefore dropped. The payload is retained until delivered, so the
// caller may reuse its read buffer.
func (b *Buffer) Push(seq uint32, payload []byte, now time.Time) bool {
	if !b.started {
		b.started = true
		b.expected = seq
	} else if b.stats.Delivered == 0 && len(b.pending) == 0 && proto.SeqLess(seq, b.expected) {
		// Nothing has been delivered yet, so the baseline is not yet history:
		// the first packet of a session may arrive after its successor. Re-basing
		// here is exactly "expected = highest_delivered + 1" with no delivery yet,
		// and it stops a reordered first packet from being mistaken for a duplicate.
		b.expected = seq
	}
	if proto.SeqLess(seq, b.expected) {
		b.stats.Duplicates++
		return false
	}
	if _, dup := b.seen[seq]; dup {
		b.stats.Duplicates++
		return false
	}
	if seq == b.expected {
		// Fast path: in order. Drain handles delivery.
		b.seen[seq] = struct{}{}
		heap.Push(&b.pending, &entry{seq: seq, payload: payload, arrived: now})
		return true
	}
	b.seen[seq] = struct{}{}
	// A gap in arrivals longer than the freeze guard means the receiver was not
	// running in between; the ages of what is already pending measure that stall,
	// not the network, so restart their hold clock before this packet's arrival
	// makes the drain look freshly active.
	if b.freezeGuard > 0 && !b.lastArrival.IsZero() && now.Sub(b.lastArrival) > b.freezeGuard {
		b.refreshPending(now, now.Sub(b.lastArrival))
		b.guardSpent = false
	}
	b.lastArrival = now
	heap.Push(&b.pending, &entry{seq: seq, payload: payload, arrived: now})
	return true
}

// Drain releases every packet that is deliverable now, in order, calling emit
// for each. Holes are skipped once they are older than HOLD_MS or wider than
// MAX_GAP.
func (b *Buffer) Drain(now time.Time, emit func(seq uint32, payload []byte)) {
	if !b.started {
		return
	}
	for {
		for len(b.pending) > 0 && b.pending[0].seq == b.expected {
			e := heap.Pop(&b.pending).(*entry)
			delete(b.seen, e.seq)
			b.expected++
			b.stats.Delivered++
			if emit != nil {
				emit(e.seq, e.payload)
			}
		}
		if len(b.pending) == 0 {
			return
		}
		// Receiver-stall guard (see DefaultFreezeGuard): if nothing at all has
		// arrived for far longer than a hold, the ages below are measuring the
		// stall, not the network. Refresh them once and let the normal rules run.
		if b.freezeGuard > 0 && !b.guardSpent && !b.lastArrival.IsZero() &&
			now.Sub(b.lastArrival) > b.freezeGuard {
			b.refreshPending(now, now.Sub(b.lastArrival))
			b.lastArrival = now
			b.guardSpent = true // exactly one refresh: a real hole still expires
			continue
		}
		if b.limit > 0 && len(b.pending) > b.limit {
			// Hard bound on memory: give up on everything before the oldest
			// pending packet and let the normal drain deliver from there.
			b.expected = b.pending[0].seq
			b.stats.Skipped++
			b.stats.SkippedByLimit++
			continue
		}
		oldest := b.pending[0]
		gap := oldest.seq - b.expected
		age := now.Sub(oldest.arrived)
		if gap > b.maxGap || (age > b.hold && !now.Before(b.graceUntil)) {
			if age > b.stats.MaxHold {
				b.stats.MaxHold = age
			}
			if gap > b.maxGap {
				b.stats.SkippedByGap++
			} else {
				b.stats.SkippedByAge++
			}
			b.expected = oldest.seq
			b.stats.Skipped++
			continue
		}
		return
	}
}

// PushAndDrain is the common path: insert one packet and release what is ready.
func (b *Buffer) PushAndDrain(seq uint32, payload []byte, now time.Time, emit func(seq uint32, payload []byte)) bool {
	ok := b.Push(seq, payload, now)
	b.Drain(now, emit)
	return ok
}

// refreshPending restarts the hold clock of everything waiting and grants one
// extra hold period before age-based skips resume.
//
// The extra period is what the backlog needs: on resume the packets still in the
// link queues and in the socket buffers are replayed in a burst, and a hole whose
// packet is a few queue positions behind them would otherwise be declared lost
// within a millisecond of the receiver waking up.
func (b *Buffer) refreshPending(now time.Time, stall time.Duration) {
	for _, e := range b.pending {
		e.arrived = now
	}
	// Scale the grace with the stall: a receiver that was gone for t has to replay
	// whatever was queued meanwhile, so t + one hold is the window it gets before
	// holes are treated as loss again.
	grace := 2 * b.hold
	if stall+b.hold > grace {
		grace = stall + b.hold
	}
	b.graceUntil = now.Add(grace)
	b.stats.FreezeGuards++
}

// Reset discards all state (used when a session restarts).
func (b *Buffer) Reset() {
	b.started = false
	b.expected = 0
	b.pending = nil
	b.lastArrival = time.Time{}
	b.guardSpent = false
	b.graceUntil = time.Time{}
	clear(b.seen)
}
