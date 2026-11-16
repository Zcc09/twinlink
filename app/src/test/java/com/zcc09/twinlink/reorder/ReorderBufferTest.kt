package com.zcc09.twinlink.reorder

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class ReorderBufferTest {

    private var now = 10_000L

    private fun buffer(
        holdMs: Long = ReorderBuffer.HOLD_MS,
        maxGap: Int = ReorderBuffer.MAX_GAP,
        capacity: Int = ReorderBuffer.CAPACITY
    ) = ReorderBuffer(holdMs = holdMs, maxGap = maxGap, capacity = capacity, clock = { now })

    @Test
    fun inOrderDeliversImmediately() {
        val buffer = buffer()
        assertEquals(1, buffer.onData(0, byteArrayOf(0)).size)
        assertEquals(1, buffer.onData(1, byteArrayOf(1)).size)
        assertEquals(1, buffer.onData(2, byteArrayOf(2)).size)
        assertEquals(3L, buffer.delivered)
        assertEquals(0, buffer.pendingCount)
    }

    @Test
    fun outOfOrderIsHeldUntilTheHoleFills() {
        val buffer = buffer()
        assertEquals(1, buffer.onData(0, byteArrayOf(0)).size)

        // 2 arrives before 1: it must be held, not written.
        assertTrue(buffer.onData(2, byteArrayOf(2)).isEmpty())
        assertEquals(1, buffer.pendingCount)
        assertEquals(1, buffer.expected)

        // 1 fills the hole and both flush in order.
        val flushed = buffer.onData(1, byteArrayOf(1))
        assertEquals(2, flushed.size)
        assertEquals(1, flushed[0][0].toInt())
        assertEquals(2, flushed[1][0].toInt())
        assertEquals(3, buffer.expected)
        assertEquals(0, buffer.pendingCount)
    }

    @Test
    fun heldPacketIsReleasedByTheTickAfterTheHoldWindow() {
        val buffer = buffer()
        buffer.onData(0, byteArrayOf(0))
        assertTrue(buffer.onData(5, byteArrayOf(5)).isEmpty())

        now += ReorderBuffer.HOLD_MS - 1
        assertTrue(buffer.tick().isEmpty())

        now += 2
        val flushed = buffer.tick()
        assertEquals(1, flushed.size)
        assertEquals(6, buffer.expected)
        assertTrue(buffer.skipped > 0)
    }

    @Test
    fun holeIsSkippedWhenTheGapExceedsMaxGap() {
        val buffer = buffer()
        val flushed = buffer.onData(ReorderBuffer.MAX_GAP + 1, byteArrayOf(7))
        // gap 65 > 64: give up on the hole immediately, no waiting for the timer.
        assertEquals(1, flushed.size)
        assertEquals(7, flushed[0][0].toInt())
        assertEquals(ReorderBuffer.MAX_GAP + 2, buffer.expected)
        assertEquals(ReorderBuffer.MAX_GAP + 1L, buffer.skipped)
    }

    @Test
    fun duplicatesAreDropped() {
        val buffer = buffer()
        buffer.onData(0, byteArrayOf(0))
        buffer.onData(1, byteArrayOf(1))
        buffer.onData(2, byteArrayOf(2))

        // Already delivered.
        assertTrue(buffer.onData(1, byteArrayOf(99)).isEmpty())
        assertEquals(1L, buffer.duplicates)

        // Held, then repeated while still held.
        assertTrue(buffer.onData(4, byteArrayOf(4)).isEmpty())
        assertTrue(buffer.onData(4, byteArrayOf(44)).isEmpty())
        assertEquals(2L, buffer.duplicates)
        assertEquals(1, buffer.pendingCount)

        // The first copy wins.
        val flushed = buffer.onData(3, byteArrayOf(3))
        assertEquals(2, flushed.size)
        assertEquals(3, flushed[0][0].toInt())
        assertEquals(4, flushed[1][0].toInt())
    }

    @Test
    fun sequenceWrapsAcrossTheU32Boundary() {
        val buffer = buffer()
        buffer.reset(Int.MAX_VALUE - 1)
        val out = ArrayList<Byte>()
        out += buffer.onData(Int.MAX_VALUE - 1, byteArrayOf(1)).map { it[0] }
        out += buffer.onData(Int.MAX_VALUE, byteArrayOf(2)).map { it[0] }
        out += buffer.onData(Int.MIN_VALUE, byteArrayOf(3)).map { it[0] } // MAX + 1
        out += buffer.onData(Int.MIN_VALUE + 1, byteArrayOf(4)).map { it[0] }

        assertEquals("1, 2, 3, 4", out.joinToString())
        assertEquals(Int.MIN_VALUE + 2, buffer.expected)

        // A pre-wrap seq is a duplicate now.
        assertTrue(buffer.onData(Int.MAX_VALUE, byteArrayOf(9)).isEmpty())
        assertEquals(1L, buffer.duplicates)
    }

    @Test
    fun memoryStaysBoundedWhenTheHoleNeverFills() {
        val buffer = buffer(holdMs = 1_000_000L, maxGap = 1_000_000, capacity = 64)
        // expected stays 0 forever; every packet from 1 up is a widening hole.
        for (seq in 1..5_000) {
            buffer.onData(seq, byteArrayOf(seq.toByte()))
            assertTrue("pending grew past capacity at seq $seq", buffer.pendingCount <= 64)
        }
        assertTrue(buffer.delivered > 0)
    }

    @Test
    fun resetRestartsTheWindow() {
        val buffer = buffer()
        buffer.onData(3, byteArrayOf(3))
        assertEquals(1, buffer.pendingCount)
        buffer.reset(0)
        assertEquals(0, buffer.expected)
        assertEquals(0, buffer.pendingCount)
        assertEquals(1, buffer.onData(0, byteArrayOf(0)).size)
    }
}
