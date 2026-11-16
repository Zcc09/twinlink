package proto

// Wrap-safe u32 sequence arithmetic (PROTOCOL §7).
//
// A sequence number is a point on a circle of 2^32 values. Two values are
// "close" only if their signed difference is small; comparing them with < is
// wrong across the wrap. Every comparison in this codebase goes through these
// helpers.

// SeqLess reports whether a is before b in wrap-safe order.
func SeqLess(a, b uint32) bool { return int32(a-b) < 0 }

// SeqLessEq reports whether a is at or before b in wrap-safe order.
func SeqLessEq(a, b uint32) bool { return int32(a-b) <= 0 }

// SeqEqual reports whether a and b are the same sequence number.
func SeqEqual(a, b uint32) bool { return a == b }

// SeqDiff returns the signed distance b-a, i.e. how far b is ahead of a.
func SeqDiff(a, b uint32) int32 { return int32(b - a) }

// SeqDistance returns how many sequence numbers separate a from b, walking
// forward from a. It is the gap the reorder buffer gives up on when it exceeds
// MAX_GAP.
func SeqDistance(a, b uint32) uint32 { return b - a }

// SeqAdd advances a by n, wrapping.
func SeqAdd(a uint32, n uint32) uint32 { return a + n }
