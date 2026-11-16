package com.zcc09.twinlink.sched

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

class LinkSchedulerTest {

    private var now = 1_000_000L

    private fun scheduler() = LinkScheduler(clock = { now })

    @Test
    fun fourToOneWeightsSplitEightyTwenty() {
        val scheduler = scheduler()
        scheduler.register(0)
        scheduler.register(1)
        scheduler.onDatagram(0)
        scheduler.onDatagram(1)
        // 4:1 weights. The split is exactly the weight ratio when the paced offer matches the
        // weighted capacity: 2 ms per 1 kB packet = 4 Mbit/s = weight(0) + weight(1).
        scheduler.seedRate(0, 3_200_000)
        scheduler.seedRate(1, 800_000)

        var fast = 0
        var slow = 0
        repeat(10_000) { index ->
            now += 2
            // PONGs keep arriving on both links in the real thing; without them both go dead.
            if (index % 500 == 0) {
                scheduler.onDatagram(0)
                scheduler.onDatagram(1)
            }
            val picked = scheduler.pick(1000)
            assertNotNull(picked)
            if (picked!!.id == 0) fast++ else slow++
        }

        // 80/20 with a 3% band on 10k packets.
        assertEquals(0.80, fast / 10_000.0, 0.03)
        assertEquals(0.20, slow / 10_000.0, 0.03)
        assertEquals(10_000, fast + slow)
    }

    @Test
    fun deadLinkGetsNothing() {
        val scheduler = scheduler()
        scheduler.register(0)
        scheduler.register(1)
        scheduler.onDatagram(0)
        scheduler.onDatagram(1)

        now += 5_000 // link 1 silent for 5 s, well past the 3 s alive window
        scheduler.onDatagram(0)

        repeat(200) {
            now += 1
            assertEquals(0, scheduler.pick(1000)!!.id)
        }
        assertEquals(0L, scheduler.link(1)!!.sentPackets)
    }

    @Test
    fun singleAliveLinkCarriesEverything() {
        val scheduler = scheduler()
        scheduler.register(0)
        scheduler.onDatagram(0)
        repeat(100) {
            now += 1
            assertEquals(0, scheduler.pick(1200)!!.id)
        }
        assertEquals(100L, scheduler.link(0)!!.sentPackets)
    }

    @Test
    fun tiesBreakTowardTheLowerRtt() {
        val scheduler = scheduler()
        scheduler.register(0)
        scheduler.register(1)
        scheduler.onDatagram(0)
        scheduler.onDatagram(1)
        scheduler.onRttSample(0, 50_000)
        scheduler.onRttSample(1, 5_000)

        // Same weight (both at the 64 kbit/s floor) and the same credit: rtt decides.
        assertEquals(1, scheduler.pick(1000)!!.id)
    }

    @Test
    fun tiesWithEqualRttBreakTowardTheLowerId() {
        val scheduler = scheduler()
        scheduler.register(3)
        scheduler.register(2)
        scheduler.onDatagram(3)
        scheduler.onDatagram(2)
        scheduler.onRttSample(3, 20_000)
        scheduler.onRttSample(2, 20_000)
        assertEquals(2, scheduler.pick(1000)!!.id)
    }

    @Test
    fun creditFollowsMeasuredThroughputOnceSamplesArrive() {
        val scheduler = scheduler()
        scheduler.register(0)
        scheduler.register(1)
        scheduler.onDatagram(0)
        scheduler.onDatagram(1)

        // Link 0 carries 4x the bytes over the first sampling window.
        repeat(4) { scheduler.recordSent(0, 100_000) }
        scheduler.recordSent(1, 100_000)
        now += 600
        repeat(4) { scheduler.recordSent(0, 100_000) }
        scheduler.recordSent(1, 100_000)

        assertTrue("rate 0 should exceed rate 1", scheduler.link(0)!!.rateBps > scheduler.link(1)!!.rateBps)
        assertTrue(scheduler.link(0)!!.weight(now) > scheduler.link(1)!!.weight(now))
    }

    @Test
    fun revivedLinkDoesNotDumpAStaleCreditBurst() {
        val scheduler = scheduler()
        scheduler.register(0)
        scheduler.register(1)
        scheduler.onDatagram(0)
        scheduler.onDatagram(1)

        now += 4_000 // both go dead
        scheduler.pick(1000)
        now += 1
        scheduler.onDatagram(1) // only link 1 comes back

        assertEquals(0.0, scheduler.link(1)!!.creditBits(), 1e-6)
        repeat(20) {
            now += 1
            assertEquals(1, scheduler.pick(1000)!!.id)
        }
    }

    @Test
    fun noLinksMeansNoPick() {
        assertEquals(null, scheduler().pick(1000))
    }
}
