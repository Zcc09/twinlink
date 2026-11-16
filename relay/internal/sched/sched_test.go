package sched_test

import (
	"sync"
	"testing"
	"time"

	"twinlink/relay/internal/sched"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

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

func keepAlive(s *sched.Scheduler, clk *fakeClock, ids ...uint8) {
	for _, id := range ids {
		s.MarkSeenAt(id, clk.Now())
	}
}

// TestSplitFollowsWeights is the headline scheduler property (PROTOCOL §5):
// deficit round robin splits bytes in proportion to the weights. 4:1 weights
// must produce an 80/20 byte split over 10k packets within 2%.
func TestSplitFollowsWeights(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	s.Add(0)
	s.Add(1)
	keepAlive(s, clk, 0, 1)
	s.SetRateBps(0, 4_000_000)
	s.SetRateBps(1, 1_000_000)

	const packets = 10000
	const bits = 8000.0 // 1000-byte datagrams
	dt := time.Duration(bits / 5_000_000 * float64(time.Second))

	counts := map[uint8]int{}
	for i := 0; i < packets; i++ {
		clk.Advance(dt)
		keepAlive(s, clk, 0, 1)
		id, ok := s.Pick(bits, nil)
		if !ok {
			t.Fatalf("pick %d found no link", i)
		}
		counts[id]++
	}

	got0 := float64(counts[0]) / packets
	got1 := float64(counts[1]) / packets
	t.Logf("split after %d packets: link0 %.1f%% (%d), link1 %.1f%% (%d)",
		packets, got0*100, counts[0], got1*100, counts[1])
	if counts[0]+counts[1] != packets {
		t.Fatalf("accounted for %d of %d packets", counts[0]+counts[1], packets)
	}
	if got0 < 0.78 || got0 > 0.82 {
		t.Fatalf("link0 carried %.2f%% of packets, want 80%% +/-2%%", got0*100)
	}
}

// TestSplitIsByteAccurateNotPacketAccurate: mixed packet sizes must still split
// bytes (not packets) in proportion to the weights.
func TestSplitIsByteAccurate(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	s.Add(0)
	s.Add(1)
	keepAlive(s, clk, 0, 1)
	s.SetRateBps(0, 3_000_000)
	s.SetRateBps(1, 1_000_000)

	bytesOn := map[uint8]float64{}
	sizes := []float64{1500, 200, 4000, 900}
	for i := 0; i < 20000; i++ {
		bits := sizes[i%len(sizes)] * 8
		clk.Advance(time.Duration(bits / 4_000_000 * float64(time.Second)))
		keepAlive(s, clk, 0, 1)
		id, ok := s.Pick(bits, nil)
		if !ok {
			t.Fatal("no link")
		}
		bytesOn[id] += bits / 8
	}
	share := bytesOn[0] / (bytesOn[0] + bytesOn[1])
	t.Logf("byte share link0 = %.3f", share)
	if share < 0.735 || share > 0.765 {
		t.Fatalf("link0 carried %.3f of bytes, want 0.75 +/-0.015", share)
	}
}

func TestDeadLinkReceivesNothing(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	s.Add(0)
	s.Add(1)
	s.SetRateBps(0, 1_000_000)
	s.SetRateBps(1, 9_000_000)
	keepAlive(s, clk, 0) // link 1 has never been heard from

	for i := 0; i < 1000; i++ {
		clk.Advance(time.Millisecond)
		s.MarkSeenAt(0, clk.Now())
		id, ok := s.Pick(8000, nil)
		if !ok || id != 0 {
			t.Fatalf("pick %d = %d (ok=%v), want link 0", i, id, ok)
		}
	}
	if s.Alive(1) {
		t.Fatal("link 1 reports alive without a datagram")
	}
	if w := s.Weight(1); w != 0 {
		t.Fatalf("dead link weight = %v, want 0", w)
	}
}

func TestAliveWindowIsThreeSeconds(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now))
	s.Add(0)
	s.Add(1)
	s.MarkSeenAt(0, clk.Now())
	s.MarkSeenAt(1, clk.Now())

	clk.Advance(2_999 * time.Millisecond)
	if !s.Alive(0) || !s.Alive(1) {
		t.Fatal("links died before 3 s")
	}
	clk.Advance(2 * time.Millisecond)
	if s.Alive(0) || s.Alive(1) {
		t.Fatal("links survived past 3 s of silence")
	}
	if _, ok := s.Pick(1000, nil); ok {
		t.Fatal("picked a dead link")
	}
}

func TestSingleAliveLinkTakesEverything(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	s.Add(0)
	s.Add(1)
	s.SetRateBps(0, 100_000)
	s.SetRateBps(1, 100_000)
	keepAlive(s, clk, 1)

	for i := 0; i < 500; i++ {
		clk.Advance(time.Millisecond)
		s.MarkSeenAt(1, clk.Now())
		id, ok := s.Pick(8000, nil)
		if !ok || id != 1 {
			t.Fatalf("pick = %d (ok=%v), want the only alive link", id, ok)
		}
	}
}

func TestTieBreakByRTTThenID(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	for _, id := range []uint8{0, 1, 2} {
		s.Add(id)
	}
	keepAlive(s, clk, 0, 1, 2)
	for _, id := range []uint8{0, 1, 2} {
		s.SetRateBps(id, 1_000_000)
	}
	// Equal credits (no time has passed), so the tie-break decides.
	s.SetRTT(0, 30*time.Millisecond)
	s.SetRTT(1, 30*time.Millisecond)
	s.SetRTT(2, 10*time.Millisecond)
	id, ok := s.Pick(8000, nil)
	if !ok || id != 2 {
		t.Fatalf("tie-break picked %d, want link 2 (lowest RTT)", id)
	}

	// All RTTs equal: lowest link id wins.
	s2 := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	for _, id := range []uint8{0, 1, 2} {
		s2.Add(id)
		s2.SetRateBps(id, 1_000_000)
		s2.SetRTT(id, 20*time.Millisecond)
	}
	keepAlive(s2, clk, 0, 1, 2)
	if id, ok := s2.Pick(8000, nil); !ok || id != 0 {
		t.Fatalf("tie-break picked %d, want link 0", id)
	}
}

func TestCreditIsChargedAndCapped(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	s.Add(0)
	s.SetRateBps(0, 8_000_000)
	keepAlive(s, clk, 0)

	// Credit accrues inside Pick (that is where time is measured), so a tiny
	// selection is used to observe it.
	clk.Advance(5 * time.Millisecond) // 40 000 bits of credit
	if _, ok := s.Pick(1, nil); !ok {
		t.Fatal("no link")
	}
	if c := s.Snapshot()[0].Credit; c < 39_000 || c > 40_000 {
		t.Fatalf("credit after 5 ms = %.0f, want ~40000", c)
	}
	s.Pick(8000, nil)
	if c := s.Snapshot()[0].Credit; c < 31_000 || c > 32_000 {
		t.Fatalf("credit after one 8000-bit packet = %.0f, want ~32000", c)
	}

	// Idle credit is capped so one link cannot bank minutes of deficit and then
	// take thousands of packets in a row.
	clk.Advance(time.Minute)
	s.MarkSeenAt(0, clk.Now()) // keep it alive so the accrual is legal at all
	s.Pick(1000, nil)
	capped := s.Snapshot()[0].Credit
	limit := 8_000_000 * sched.DefaultCreditCapSeconds
	if capped > limit+1 {
		t.Fatalf("credit grew to %.0f after a minute of idling, cap is %.0f", capped, limit)
	}
	if capped < limit-20_000 {
		t.Fatalf("credit = %.0f, expected it to sit at the %.0f cap", capped, limit)
	}
}

func TestRateEWMA(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now)) // 500 ms sample interval
	s.Add(0)
	keepAlive(s, clk, 0)

	// 10 Mbit/s over a 500 ms window.
	s.AddSentBytes(0, 625_000)
	clk.Advance(500 * time.Millisecond)
	s.Sample()
	got := s.Snapshot()[0].RateBps
	want := 0.3 * 10e6
	if got < want*0.99 || got > want*1.01 {
		t.Fatalf("rate after one window = %.0f, want %.0f (alpha 0.3 from 0)", got, want)
	}

	// A second window converges further; the window counter resets each sample.
	s.AddSentBytes(0, 625_000)
	clk.Advance(500 * time.Millisecond)
	s.Sample()
	got = s.Snapshot()[0].RateBps
	want = 0.3*10e6 + 0.7*0.3*10e6
	if got < want*0.99 || got > want*1.01 {
		t.Fatalf("rate after two windows = %.0f, want %.0f", got, want)
	}
}

func TestWeightFloor(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now))
	s.Add(0)
	keepAlive(s, clk, 0)
	s.SetRateBps(0, 1000) // below the 64 kbit/s floor
	if w := s.Weight(0); w != sched.DefaultMinWeightBps {
		t.Fatalf("weight = %v, want the %v floor", w, sched.DefaultMinWeightBps)
	}
	s.SetRateBps(0, 5_000_000)
	if w := s.Weight(0); w != 5_000_000 {
		t.Fatalf("weight = %v, want 5000000", w)
	}
}

func TestSendableFilterYieldsToOtherLinks(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	s.Add(0)
	s.Add(1)
	s.SetRateBps(0, 9_000_000)
	s.SetRateBps(1, 1_000_000)
	keepAlive(s, clk, 0, 1)

	// Link 0 is full: every pick must fall through to link 1, and the split must
	// come back to 9:1 as soon as link 0 has room again.
	for i := 0; i < 100; i++ {
		clk.Advance(time.Millisecond)
		keepAlive(s, clk, 0, 1)
		if id, ok := s.Pick(8000, func(id uint8) bool { return id != 0 }); !ok || id != 1 {
			t.Fatalf("pick = %d (ok=%v), want link 1 while link 0 is full", id, ok)
		}
	}
	if _, ok := s.Pick(8000, func(uint8) bool { return false }); ok {
		t.Fatal("picked a link although none was sendable")
	}
}

func TestRemoveAndReadd(t *testing.T) {
	clk := newFakeClock()
	s := sched.New(sched.WithClock(clk.Now), sched.WithSampleInterval(time.Hour))
	s.Add(0)
	s.Add(1)
	keepAlive(s, clk, 0, 1)
	s.Remove(0)
	if s.Len() != 1 {
		t.Fatalf("len = %d, want 1", s.Len())
	}
	if _, ok := s.Pick(8000, nil); !ok {
		t.Fatal("no link after removing one")
	}
	s.Add(0)
	if s.Len() != 2 {
		t.Fatalf("len = %d, want 2", s.Len())
	}
}
