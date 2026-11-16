package com.zcc09.twinlink.sched

import kotlin.math.abs
import kotlin.math.max

/**
 * Deficit round robin over per-link weights (PROTOCOL.md §5), used in both directions.
 *
 * Per link:
 *  - `alive` while a datagram has been seen in the last [aliveMs] (3 s),
 *  - `rttUs` EWMA (alpha 0.2) from PING/PONG,
 *  - `rateBps` EWMA (alpha 0.3) of achieved throughput, sampled every [sampleMs] (500 ms),
 *  - `weight` = max(rateBps, [minWeight]) while alive, 0 while dead.
 *
 * Every pick advances each live link's credit by `weight * dt`, hands the packet to the link with
 * the largest credit and debits `bytes * 8` from it. Ties break toward the lower rtt, then the
 * lower link id. Pure Kotlin with an injected clock.
 */
class LinkScheduler(
    private val clock: () -> Long = { System.nanoTime() / 1_000_000L },
    val aliveMs: Long = 3_000L,
    val sampleMs: Long = 500L,
    private val rttAlpha: Double = 0.2,
    private val rateAlpha: Double = 0.3,
    val minWeightBps: Long = 64_000L
) {
    inner class Link internal constructor(val id: Int) {
        var rttUs: Long = -1L
            internal set
        var rateBps: Double = 0.0
            internal set
        internal var lastSeenMs: Long = Long.MIN_VALUE / 4
        internal var credit: Double = 0.0
        internal var lastTickMs: Long = 0L
        internal var windowStartMs: Long = 0L
        internal var windowBytes: Long = 0L
        internal var wasAlive: Boolean = false

        var sentPackets: Long = 0
            internal set
        var sentBytes: Long = 0
            internal set

        fun isAlive(now: Long = System.nanoTime() / 1_000_000L): Boolean = now - lastSeenMs <= aliveMs

        fun weight(now: Long): Long = if (isAlive(now)) max(rateBps, minWeightBps.toDouble()).toLong() else 0L

        fun creditBits(): Double = credit
    }

    private val links = LinkedHashMap<Int, Link>()

    fun register(id: Int, now: Long = clock()): Link {
        links[id]?.let { return it }
        val link = Link(id)
        link.lastTickMs = now
        link.windowStartMs = now
        link.lastSeenMs = now
        links[id] = link
        return link
    }

    fun unregister(id: Int) {
        links.remove(id)
    }

    fun link(id: Int): Link? = links[id]

    fun links(): List<Link> = links.values.toList()

    fun clear() = links.clear()

    /** Any received datagram proves the link is up. */
    fun onDatagram(id: Int, now: Long = clock()) {
        val link = links[id] ?: register(id, now)
        link.lastSeenMs = now
        if (!link.wasAlive) {
            // Do not hand a revived link a pile of credit earned while it was dead.
            link.credit = 0.0
            link.lastTickMs = now
            link.wasAlive = true
        }
    }

    fun onRttSample(id: Int, rttUs: Long) {
        val link = links[id] ?: return
        if (rttUs < 0) return
        link.rttUs = if (link.rttUs < 0) rttUs else (rttAlpha * rttUs + (1 - rttAlpha) * link.rttUs).toLong()
    }

    /** Counts bytes actually handed to the link for the achieved-throughput EWMA. */
    fun recordSent(id: Int, bytes: Int, now: Long = clock()) {
        val link = links[id] ?: return
        link.windowBytes += bytes.toLong()
        sampleRate(link, now)
    }

    /** Test/diagnostic hook: seed the measured rate directly. */
    fun seedRate(id: Int, bps: Long, now: Long = clock()) {
        val link = links[id] ?: register(id, now)
        link.rateBps = bps.toDouble()
        link.windowBytes = 0L
        link.windowStartMs = now
    }

    /** Picks a link for a [bytes]-sized packet, or null when every link is dead. */
    fun pick(bytes: Int, now: Long = clock()): Link? {
        if (links.isEmpty()) return null
        var best: Link? = null
        for (link in links.values) {
            advance(link, now)
            sampleRate(link, now)
            val weight = link.weight(now)
            if (weight <= 0L) {
                link.wasAlive = false
                continue
            }
            link.wasAlive = true
            val incumbent = best
            if (incumbent == null) {
                best = link
                continue
            }
            val diff = link.credit - incumbent.credit
            if (diff > EPS) {
                best = link
            } else if (abs(diff) <= EPS && betterTie(link, incumbent)) {
                best = link
            }
        }
        val chosen = best ?: return null
        chosen.credit -= bytes.toDouble() * 8.0
        chosen.sentPackets++
        chosen.sentBytes += bytes.toLong()
        chosen.windowBytes += bytes.toLong()
        return chosen
    }

    /** Snapshot for stats/UI. */
    fun snapshot(now: Long = clock()): List<LinkState> = links.values.map {
        LinkState(
            id = it.id,
            alive = it.weight(now) > 0L,
            rttUs = it.rttUs,
            rateBps = it.rateBps.toLong(),
            weight = it.weight(now),
            creditBits = it.credit,
            sentPackets = it.sentPackets,
            sentBytes = it.sentBytes
        )
    }

    private fun advance(link: Link, now: Long) {
        if (now <= link.lastTickMs) return
        val dtSeconds = (now - link.lastTickMs) / 1000.0
        val weight = link.weight(now).toDouble()
        if (weight > 0.0) {
            // PROTOCOL §5 has no cap: credit is bits, earned at the link's weight and spent per
            // packet. A per-link cap would hand everything to whichever link has the bigger cap.
            link.credit += weight * dtSeconds
        }
        link.lastTickMs = now
    }

    private fun sampleRate(link: Link, now: Long) {
        val elapsed = now - link.windowStartMs
        if (elapsed < sampleMs) return
        val measured = if (elapsed > 0) link.windowBytes * 8.0 * 1000.0 / elapsed else 0.0
        link.rateBps = rateAlpha * measured + (1 - rateAlpha) * link.rateBps
        link.windowBytes = 0L
        link.windowStartMs = now
    }

    private fun betterTie(candidate: Link, incumbent: Link): Boolean {
        val a = if (candidate.rttUs < 0) Long.MAX_VALUE / 4 else candidate.rttUs
        val b = if (incumbent.rttUs < 0) Long.MAX_VALUE / 4 else incumbent.rttUs
        if (a != b) return a < b
        return candidate.id < incumbent.id
    }

    companion object {
        private const val EPS = 1e-9
    }

    data class LinkState(
        val id: Int,
        val alive: Boolean,
        val rttUs: Long,
        val rateBps: Long,
        val weight: Long,
        val creditBits: Double,
        val sentPackets: Long,
        val sentBytes: Long
    )
}
