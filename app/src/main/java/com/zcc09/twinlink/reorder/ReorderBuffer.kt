package com.zcc09.twinlink.reorder

import com.zcc09.twinlink.proto.seqCompare

/**
 * Receiver-side reorder buffer (PROTOCOL.md §6).
 *
 * `expected` is the next seq to hand to the TUN. Packets that are ahead are held until the hole
 * in front of them fills, or until either bound is exceeded:
 *  - the oldest held packet has waited more than [holdMs] (default 40), or
 *  - the hole is wider than [maxGap] packets (default 64).
 * Then the buffer gives up on the hole and skips forward to the lowest held seq.
 *
 * All comparisons are wrap-safe (u32 subtraction), and memory is bounded by [capacity].
 * Pure Kotlin: the clock is injected so the unit tests can drive time.
 */
class ReorderBuffer(
    val holdMs: Long = HOLD_MS,
    val maxGap: Int = MAX_GAP,
    val capacity: Int = CAPACITY,
    private val clock: () -> Long = { System.currentTimeMillis() }
) {
    companion object {
        const val HOLD_MS = 40L
        const val MAX_GAP = 64
        const val CAPACITY = 512
    }

    private class Item(val seq: Int, val payload: ByteArray, val at: Long)

    // Sorted by wrap-safe distance ahead of `expected`; index 0 is the closest one.
    private val pending = ArrayList<Item>(32)

    var expected: Int = 0
        private set

    var delivered: Long = 0
        private set

    var duplicates: Long = 0
        private set

    var skipped: Long = 0
        private set

    val pendingCount: Int get() = pending.size

    fun reset(startSeq: Int = 0) {
        pending.clear()
        expected = startSeq
        delivered = 0
        duplicates = 0
        skipped = 0
    }

    /** Accepts one DATA payload and returns every payload that is now in order. */
    fun onData(seq: Int, payload: ByteArray, now: Long = clock()): List<ByteArray> {
        val out = ArrayList<ByteArray>(4)
        accept(seq, payload, now)
        drain(now, out)
        return out
    }

    /** Time-driven drain: call on a short tick so held packets are released without new arrivals. */
    fun tick(now: Long = clock()): List<ByteArray> {
        val out = ArrayList<ByteArray>(4)
        drain(now, out)
        return out
    }

    private fun accept(seq: Int, payload: ByteArray, now: Long) {
        if (seqCompare(seq, expected) < 0) {
            duplicates++
            return
        }
        // Already held: keep the first copy, drop the duplicate.
        for (i in pending.indices) {
            if (pending[i].seq == seq) {
                duplicates++
                return
            }
        }
        // Insert keeping the list sorted by distance ahead of `expected`.
        var idx = pending.size
        for (i in pending.indices) {
            if (seqCompare(pending[i].seq, seq) > 0) {
                idx = i
                break
            }
        }
        pending.add(idx, Item(seq, payload, now))

        // Bounded memory: flush forward rather than grow without limit.
        if (pending.size > capacity) {
            val head = pending[0]
            skipped += seqCompare(head.seq, expected).toLong().coerceAtLeast(0)
            expected = head.seq
            duplicates++
        }
    }

    private fun drain(now: Long, out: MutableList<ByteArray>) {
        while (pending.isNotEmpty()) {
            val head = pending[0]
            val cmp = seqCompare(head.seq, expected)
            if (cmp == 0) {
                pending.removeAt(0)
                out.add(head.payload)
                delivered++
                expected = head.seq + 1
                continue
            }
            if (cmp < 0) {
                // Stale entry (expected moved past it).
                pending.removeAt(0)
                duplicates++
                continue
            }
            val age = now - head.at
            if (age > holdMs || cmp > maxGap) {
                skipped += cmp.toLong()
                expected = head.seq
                continue
            }
            return
        }
    }
}
