package reorder_test

import (
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"twinlink/relay/internal/reorder"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1700000000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sink struct {
	seqs []uint32
	data [][]byte
}

func (s *sink) emit(seq uint32, p []byte) {
	s.seqs = append(s.seqs, seq)
	s.data = append(s.data, append([]byte(nil), p...))
}

func TestInOrderPassthrough(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink
	for i := uint32(0); i < 100; i++ {
		b.PushAndDrain(i, []byte{byte(i)}, clk.Now(), out.emit)
		clk.Advance(time.Millisecond)
	}
	if len(out.seqs) != 100 {
		t.Fatalf("delivered %d, want 100", len(out.seqs))
	}
	for i, s := range out.seqs {
		if s != uint32(i) {
			t.Fatalf("position %d delivered seq %d", i, s)
		}
		if out.data[i][0] != byte(i) {
			t.Fatalf("payload %d = %d", i, out.data[i][0])
		}
	}
	st := b.Stats()
	if st.Duplicates != 0 || st.Skipped != 0 || st.Buffered != 0 {
		t.Fatalf("stats = %+v, want a clean run", st)
	}
}

func TestOutOfOrderHeldThenReleased(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink

	b.PushAndDrain(0, []byte("a"), clk.Now(), out.emit)
	b.PushAndDrain(2, []byte("c"), clk.Now(), out.emit) // held: 1 is missing
	if len(out.seqs) != 1 {
		t.Fatalf("delivered %d packets, want 1 (2 must be held)", len(out.seqs))
	}
	if st := b.Stats(); st.Buffered != 1 {
		t.Fatalf("buffered = %d, want 1", st.Buffered)
	}

	clk.Advance(10 * time.Millisecond) // still inside HOLD_MS
	b.PushAndDrain(1, []byte("b"), clk.Now(), out.emit)
	if len(out.seqs) != 3 {
		t.Fatalf("delivered %d packets, want 3", len(out.seqs))
	}
	for i, want := range []uint32{0, 1, 2} {
		if out.seqs[i] != want {
			t.Fatalf("delivery order %v, want [0 1 2]", out.seqs)
		}
	}
	if st := b.Stats(); st.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", st.Skipped)
	}
}

func TestHoleSkippedAfterHold(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink

	b.PushAndDrain(0, []byte("a"), clk.Now(), out.emit)
	b.PushAndDrain(2, []byte("c"), clk.Now(), out.emit)
	clk.Advance(39 * time.Millisecond)
	b.Drain(clk.Now(), out.emit)
	if len(out.seqs) != 1 {
		t.Fatalf("delivered %d, want 1 (hold must not expire before 40 ms)", len(out.seqs))
	}
	clk.Advance(2 * time.Millisecond) // now 41 ms
	b.Drain(clk.Now(), out.emit)
	if len(out.seqs) != 2 || out.seqs[1] != 2 {
		t.Fatalf("delivery %v, want [0 2] after the hole aged out", out.seqs)
	}
	if st := b.Stats(); st.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", st.Skipped)
	}

	// The packet that was given up on is now a duplicate and must be dropped.
	if b.Push(1, []byte("b"), clk.Now()) {
		t.Fatal("late packet was accepted after the buffer skipped its hole")
	}
	if st := b.Stats(); st.Duplicates != 1 {
		t.Fatalf("duplicates = %d, want 1", st.Duplicates)
	}
}

func TestHoleSkippedWhenGapExceedsMax(t *testing.T) {
	clk := newFakeClock()

	// A gap of exactly MAX_GAP is still held: it may be ordinary reordering.
	held := reorder.New(reorder.WithClock(clk.Now))
	var out1 sink
	held.PushAndDrain(0, []byte("a"), clk.Now(), out1.emit) // expected becomes 1
	held.PushAndDrain(65, []byte("b"), clk.Now(), out1.emit)
	if len(out1.seqs) != 1 {
		t.Fatalf("gap 64 was skipped eagerly: delivered %v", out1.seqs)
	}
	if st := held.Stats(); st.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", st.Skipped)
	}

	// A gap of 65 is given up on immediately, without waiting for HOLD_MS.
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink
	b.PushAndDrain(0, []byte("a"), clk.Now(), out.emit) // expected becomes 1
	b.PushAndDrain(66, []byte("c"), clk.Now(), out.emit)
	if len(out.seqs) != 2 {
		t.Fatalf("delivered %v, want [0 66] (gap 65 must skip at once)", out.seqs)
	}
	if out.seqs[1] != 66 {
		t.Fatalf("delivered seq %d second, want 66", out.seqs[1])
	}
	if st := b.Stats(); st.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", st.Skipped)
	}
}

func TestDuplicatesDropped(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink

	b.PushAndDrain(0, []byte("a"), clk.Now(), out.emit)
	if b.Push(0, []byte("a"), clk.Now()) {
		t.Error("in-order duplicate accepted")
	}
	b.PushAndDrain(2, []byte("c"), clk.Now(), out.emit)
	if b.Push(2, []byte("c"), clk.Now()) {
		t.Error("pending duplicate accepted")
	}
	clk.Advance(50 * time.Millisecond)
	b.Drain(clk.Now(), out.emit) // skips 1, delivers 2
	if b.Push(1, []byte("b"), clk.Now()) {
		t.Error("stale duplicate accepted after skip")
	}
	if st := b.Stats(); st.Duplicates != 3 {
		t.Errorf("duplicates = %d, want 3", st.Duplicates)
	}
	if len(out.seqs) != 2 {
		t.Errorf("delivered %d packets, want 2", len(out.seqs))
	}
}

func TestWrapAt2To32(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink

	start := uint32(0xfffffffd)
	// Out of order across the wrap: fd, 0, fe, ff, 1, 2
	for _, s := range []uint32{start, 0, start + 1, start + 2, 1, 2} {
		b.PushAndDrain(s, []byte{byte(s)}, clk.Now(), out.emit)
	}
	want := []uint32{0xfffffffd, 0xfffffffe, 0xffffffff, 0, 1, 2}
	if len(out.seqs) != len(want) {
		t.Fatalf("delivered %v, want %v", out.seqs, want)
	}
	for i := range want {
		if out.seqs[i] != want[i] {
			t.Fatalf("delivered %v, want %v", out.seqs, want)
		}
	}
	if st := b.Stats(); st.Duplicates != 0 || st.Skipped != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestWrapGapDistanceIsWrapSafe(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink

	b.PushAndDrain(0xfffffff0, []byte("a"), clk.Now(), out.emit)
	// 0x20 - 0xfffffff0 is a gap of 0x30 = 48 (< MAX_GAP), across the wrap.
	b.PushAndDrain(0x20, []byte("b"), clk.Now(), out.emit)
	if len(out.seqs) != 1 {
		t.Fatalf("delivered %v, want only the first packet (gap 48 must be held)", out.seqs)
	}
	clk.Advance(41 * time.Millisecond)
	b.Drain(clk.Now(), out.emit)
	if len(out.seqs) != 2 || out.seqs[1] != 0x20 {
		t.Fatalf("delivered %v, want [4294967280 32]", out.seqs)
	}
}

func TestBufferBoundedUnderFlood(t *testing.T) {
	clk := newFakeClock()
	// A huge gap allowance plus a small hard limit: only the limit can bound
	// the pending set here.
	b := reorder.New(reorder.WithClock(clk.Now), reorder.WithMaxGap(1<<20), reorder.WithLimit(64))
	var out sink

	b.PushAndDrain(0, nil, clk.Now(), out.emit)
	peak := 0
	for i := uint32(1); i <= 50000; i++ {
		b.PushAndDrain(i*3, nil, clk.Now(), out.emit) // every packet leaves a 3-wide hole
		if n := b.Stats().Buffered; n > peak {
			peak = n
		}
		if n := b.Stats().Buffered; n > 64 {
			t.Fatalf("buffer grew to %d entries, limit is 64", n)
		}
	}
	if peak < 60 {
		t.Fatalf("peak buffer %d: the test never exercised the limit", peak)
	}
	if b.Stats().Skipped == 0 {
		t.Fatal("no holes were skipped, so nothing was bounded")
	}
	t.Logf("peak buffered %d, delivered %d, skipped %d", peak, b.Stats().Delivered, b.Stats().Skipped)
}

func TestBufferBoundedWithDefaultLimits(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink
	for i := uint32(0); i < 20000; i++ {
		b.PushAndDrain(i*1000, nil, clk.Now(), out.emit) // MAX_GAP tripped every time
		if n := b.Stats().Buffered; n > 66 {
			t.Fatalf("buffer grew to %d with default limits", n)
		}
	}
}

// TestRandomReorderWithinBounds is the realistic case: packets are displaced by
// up to 30 positions and every hole fills well inside HOLD_MS, so nothing may be
// lost and delivery must be perfectly ordered.
func TestRandomReorderWithinBounds(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now), reorder.WithStartSeq(0))
	var out sink

	const n = 2000
	rng := rand.New(rand.NewSource(42))
	type arrival struct {
		step int
		seq  uint32
	}
	arr := make([]arrival, n)
	for i := 0; i < n; i++ {
		arr[i] = arrival{step: i + rng.Intn(30), seq: uint32(i)}
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].step != arr[j].step {
			return arr[i].step < arr[j].step
		}
		return arr[i].seq < arr[j].seq
	})
	for _, a := range arr {
		b.PushAndDrain(a.seq, []byte{byte(a.seq)}, clk.Now(), out.emit)
		clk.Advance(time.Millisecond)
	}
	clk.Advance(50 * time.Millisecond)
	b.Drain(clk.Now(), out.emit)

	if len(out.seqs) != n {
		t.Fatalf("delivered %d/%d packets", len(out.seqs), n)
	}
	for i, s := range out.seqs {
		if s != uint32(i) {
			t.Fatalf("position %d delivered seq %d", i, s)
		}
	}
	if st := b.Stats(); st.Skipped != 0 || st.Duplicates != 0 {
		t.Fatalf("stats = %+v, want no skips and no duplicates", st)
	}
}

// TestFreezeGuard covers the documented extension: after the receiver itself
// stopped running, the first drain must not mistake the stall for loss.
func TestFreezeGuard(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now), reorder.WithFreezeGuard(reorder.DefaultFreezeGuard))
	var out sink

	b.PushAndDrain(0, []byte("a"), clk.Now(), out.emit)
	b.PushAndDrain(2, []byte("c"), clk.Now(), out.emit) // hole at 1
	clk.Advance(200 * time.Millisecond)                 // the whole process was frozen
	b.Drain(clk.Now(), out.emit)
	if len(out.seqs) != 1 {
		t.Fatalf("delivered %v after a 200 ms freeze, want just the first packet", out.seqs)
	}
	if st := b.Stats(); st.FreezeGuards != 1 || st.Skipped != 0 {
		t.Fatalf("stats = %+v, want one freeze guard and no skip", st)
	}

	// The refresh also grants a recovery grace (stall + one hold = 240 ms here)
	// for the backlog to drain: nothing is skipped inside it.
	clk.Advance(41 * time.Millisecond)
	b.Drain(clk.Now(), out.emit)
	if len(out.seqs) != 1 {
		t.Fatalf("delivered %v inside the recovery grace, want 1 packet", out.seqs)
	}
	clk.Advance(100 * time.Millisecond) // 341 ms: grace runs to 440 ms
	b.Drain(clk.Now(), out.emit)
	if len(out.seqs) != 1 {
		t.Fatalf("delivered %v inside the recovery grace, want 1 packet", out.seqs)
	}

	// Now it really is a loss: past the grace, the hole is given up on.
	clk.Advance(110 * time.Millisecond) // 451 ms
	b.Drain(clk.Now(), out.emit)
	if len(out.seqs) != 2 || out.seqs[1] != 2 {
		t.Fatalf("delivered %v, want [0 2]", out.seqs)
	}
	if st := b.Stats(); st.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", st.Skipped)
	}

	// With the guard disabled the same stall is treated as loss, which is the
	// strict PROTOCOL §6 behaviour.
	strict := reorder.New(reorder.WithClock(clk.Now))
	var out2 sink
	strict.PushAndDrain(0, nil, clk.Now(), out2.emit)
	strict.PushAndDrain(2, nil, clk.Now(), out2.emit)
	clk.Advance(200 * time.Millisecond)
	strict.Drain(clk.Now(), out2.emit)
	if len(out2.seqs) != 2 {
		t.Fatalf("strict buffer delivered %v, want [0 2] (skip on age)", out2.seqs)
	}
	if st := strict.Stats(); st.Skipped != 1 || st.FreezeGuards != 0 {
		t.Fatalf("strict stats = %+v", st)
	}
}

func TestDefaults(t *testing.T) {
	if reorder.DefaultHold != 40*time.Millisecond {
		t.Errorf("HOLD_MS = %v, want 40ms", reorder.DefaultHold)
	}
	if reorder.DefaultMaxGap != 64 {
		t.Errorf("MAX_GAP = %d, want 64", reorder.DefaultMaxGap)
	}
}

func TestReset(t *testing.T) {
	clk := newFakeClock()
	b := reorder.New(reorder.WithClock(clk.Now))
	var out sink
	b.PushAndDrain(100, []byte("a"), clk.Now(), out.emit)
	b.Reset()
	if b.Started() {
		t.Fatal("buffer still started after Reset")
	}
	b.PushAndDrain(5, []byte("b"), clk.Now(), out.emit)
	if len(out.seqs) != 2 || out.seqs[1] != 5 {
		t.Fatalf("delivery after reset %v", out.seqs)
	}
}
