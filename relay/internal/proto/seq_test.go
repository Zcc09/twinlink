package proto_test

import (
	"testing"

	"twinlink/relay/internal/proto"
)

// TestSeqWrapSafe covers the comparison that matters at 2^32 (PROTOCOL §7):
// plain `<` is wrong across the wrap, `int32(a-b) < 0` is not.
func TestSeqWrapSafe(t *testing.T) {
	cases := []struct {
		a, b uint32
		less bool
	}{
		{0, 1, true},
		{1, 0, false},
		{5, 5, false},
		{0xfffffffe, 0xffffffff, true},
		{0xffffffff, 0, true},           // the wrap itself
		{0xffffffff, 0xffffffff, false}, // equal
		{0, 0xffffffff, false},          // 0 is after 0xffffffff
		{0xffffff00, 0x00000100, true},  // 512 apart, across the wrap
		{0x00000100, 0xffffff00, false}, // and the other way round
		// exactly 2^31 apart is genuinely ambiguous and is not tested here.
	}
	for _, c := range cases {
		if got := proto.SeqLess(c.a, c.b); got != c.less {
			t.Errorf("SeqLess(%#08x, %#08x) = %v, want %v", c.a, c.b, got, c.less)
		}
		if got := proto.SeqLessEq(c.a, c.b); got != (c.less || c.a == c.b) {
			t.Errorf("SeqLessEq(%#08x, %#08x) = %v", c.a, c.b, got)
		}
	}
}

// TestSeqWalkAcrossWrap walks 5000 sequence numbers through the wrap and checks
// that the wrap-safe order agrees with real order at every step.
func TestSeqWalkAcrossWrap(t *testing.T) {
	start := uint32(0xffffffff - 2500)
	prev := start
	for i := 1; i < 5000; i++ {
		cur := start + uint32(i)
		if !proto.SeqLess(prev, cur) {
			t.Fatalf("SeqLess(%#08x, %#08x) is false, want true", prev, cur)
		}
		if proto.SeqLess(cur, prev) {
			t.Fatalf("SeqLess(%#08x, %#08x) is true, want false", cur, prev)
		}
		if d := proto.SeqDistance(prev, cur); d != 1 {
			t.Fatalf("SeqDistance(%#08x, %#08x) = %d, want 1", prev, cur, d)
		}
		prev = cur
	}
}

func TestSeqDistanceAcrossWrap(t *testing.T) {
	cases := []struct {
		a, b uint32
		want uint32
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0xffffffff, 0, 1},
		{0xfffffffe, 2, 4},
		{0xfffffff0, 0x10, 32},
		{10, 5, 0xfffffffb}, // b is behind a: the raw unsigned distance
	}
	for _, c := range cases {
		if got := proto.SeqDistance(c.a, c.b); got != c.want {
			t.Errorf("SeqDistance(%#08x, %#08x) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if d := proto.SeqDiff(0xffffffff, 1); d != 2 {
		t.Errorf("SeqDiff(0xffffffff, 1) = %d, want 2", d)
	}
	if d := proto.SeqDiff(5, 3); d != -2 {
		t.Errorf("SeqDiff(5, 3) = %d, want -2", d)
	}
	if got := proto.SeqAdd(0xffffffff, 1); got != 0 {
		t.Errorf("SeqAdd(0xffffffff, 1) = %d, want 0", got)
	}
	if !proto.SeqEqual(7, 7) || proto.SeqEqual(7, 8) {
		t.Error("SeqEqual is wrong")
	}
}
