package relay_test

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// The two emulated WAN links the benchmark bonds.
var benchSpecs = []simSpec{
	{RateBps: 50e6, RTT: 10 * time.Millisecond}, // link 0: fast
	{RateBps: 20e6, RTT: 50 * time.Millisecond}, // link 1: slow
}

// transfer drives one rig and returns the measured-window goodput in MB/s.
//
// warmup packets are not measured: they exist so the 500 ms rate EWMA and the
// deficit credits settle, exactly as they must before a real relay's split is
// meaningful.
func transfer(tb testing.TB, specs []simSpec, warmup, measured int, strict bool) (mbps float64, res runResult) {
	tb.Helper()
	r := newRig(tb, specs, warmup, warmup+measured)
	defer r.close()
	res = r.run(120 * time.Second)
	if len(res.Problems) > 0 && strict {
		tb.Fatalf("%d delivery problems: %v", len(res.Problems), res.Problems[:min(3, len(res.Problems))])
	}
	if res.Delivered != warmup+measured {
		tb.Fatalf("delivered %d/%d", res.Delivered, warmup+measured)
	}
	if res.MeasuredElapsed <= 0 {
		tb.Fatalf("measured window was empty")
	}
	mbps = float64(res.MeasuredBytes) / res.MeasuredElapsed.Seconds() / 1e6
	return mbps, res
}

var (
	baselineOnce sync.Once
	fastOnlyMBps float64
	slowOnlyMBps float64
)

// BenchmarkAggregate bonds two shaped links (50 Mbit/s + 20 Mbit/s) through the
// real relay data path and reports the achieved aggregate goodput.
//
// It also measures each link on its own (with the same shaping) and reports both
// the single-link numbers and the speed-up, and fails if bonding does not beat
// the better single link.
func BenchmarkAggregate(b *testing.B) {
	baselineOnce.Do(func() {
		fastOnlyMBps, _ = transfer(b, benchSpecs[:1], 500, 5000, false)
		slowOnlyMBps, _ = transfer(b, benchSpecs[1:], 500, 5000, false)
		b.Logf("single link 0 (50 Mbit/s, 10 ms): %.2f MB/s", fastOnlyMBps)
		b.Logf("single link 1 (20 Mbit/s, 50 ms): %.2f MB/s", slowOnlyMBps)
	})

	var sum float64
	var last runResult
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mb, res := transfer(b, benchSpecs, 3000, 5000, false)
		sum += mb
		last = res
	}
	b.StopTimer()

	bonded := sum / float64(b.N)
	single := fastOnlyMBps
	if slowOnlyMBps > single {
		single = slowOnlyMBps
	}
	speedup := bonded / single

	var split []string
	for i, n := range last.ByLink {
		split = append(split, fmt.Sprintf("link%d=%.2f MB/s", i, float64(n)/last.MeasuredElapsed.Seconds()/1e6))
	}
	b.Logf("bonded aggregate: %.2f MB/s (%s)", bonded, joinStrings(split, ", "))
	b.Logf("single-link best: %.2f MB/s -> speed-up %.2fx of %.2f MB/s theoretical",
		single, speedup, float64(70e6)/8/1e6)
	b.Logf("relay counters: %+v", last.Stats.Counters)

	b.ReportMetric(bonded, "MB/s")
	b.ReportMetric(fastOnlyMBps, "MB/s-link0-only")
	b.ReportMetric(slowOnlyMBps, "MB/s-link1-only")
	b.ReportMetric(speedup, "speedup-vs-best-link")

	if bonded <= single {
		b.Errorf("bonding did not beat the best single link: %.2f <= %.2f MB/s", bonded, single)
	}
	if bonded < 0.8*float64(70e6)/8/1e6 {
		b.Logf("note: aggregate %.2f MB/s is below 80%% of the 8.75 MB/s capacity sum", bonded)
	}
}

// TestBondingSummary is the human-readable version of BenchmarkAggregate: run
//
//	go test ./internal/relay -run TestBondingSummary -v
//
// to see the three numbers side by side.
func TestBondingSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput summary skipped in -short mode")
	}
	fast, _ := transfer(t, benchSpecs[:1], 500, 4000, false)
	slow, _ := transfer(t, benchSpecs[1:], 500, 4000, false)
	bonded, res := transfer(t, benchSpecs, 3000, 4000, true)

	el := res.MeasuredElapsed.Seconds()
	line0 := float64(res.ByLink[0]) / el / 1e6
	line1 := float64(res.ByLink[1]) / el / 1e6

	t.Logf("link 0 alone (50 Mbit/s, 10 ms): %6.2f MB/s", fast)
	t.Logf("link 1 alone (20 Mbit/s, 50 ms): %6.2f MB/s", slow)
	t.Logf("bonded                          : %6.2f MB/s  (link0 %.2f + link1 %.2f)", bonded, line0, line1)
	t.Logf("speed-up vs best single link    : %6.2fx", bonded/maxf(fast, slow))
	t.Logf("delivered %d packets in %s, dup=%d skipped=%d problems=%d",
		res.Delivered, res.MeasuredElapsed.Round(time.Millisecond), res.Duplicates, res.Skipped, len(res.Problems))

	if bonded <= maxf(fast, slow) {
		t.Fatalf("bonding did not beat the best single link: %.2f <= %.2f MB/s", bonded, maxf(fast, slow))
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
