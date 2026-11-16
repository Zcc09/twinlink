package com.zcc09.twinlink.vpn

import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong

/**
 * Snapshot-able counters shared by the engine (writer) and the UI (1 s reader).
 * Pure Kotlin: atomics and a map only, no android imports.
 */
class TwinlinkStats {

    enum class Status { DISCONNECTED, CONNECTING, CONNECTED, ERROR }

    class Rate {
        private var lastBytes = 0L
        private var lastAt = 0L

        @Volatile
        var bps: Long = 0L
            private set

        fun sample(total: Long, now: Long) {
            if (lastAt == 0L) {
                lastAt = now
                lastBytes = total
                return
            }
            val dt = now - lastAt
            if (dt >= 500) {
                bps = ((total - lastBytes) * 8_000L) / dt
                lastBytes = total
                lastAt = now
            }
        }
    }

    class LinkCounters(val id: Int) {
        @Volatile
        var label: String = ""

        @Volatile
        var iface: String? = null

        val rxBytes = AtomicLong()
        val txBytes = AtomicLong()
        val rxPackets = AtomicLong()
        val txPackets = AtomicLong()

        @Volatile
        var rttUs: Long = -1L

        @Volatile
        var lastRxMs: Long = 0L

        internal val rxRate = Rate()
        internal val txRate = Rate()

        fun addRx(bytes: Int, now: Long) {
            val total = rxBytes.addAndGet(bytes.toLong())
            rxPackets.incrementAndGet()
            lastRxMs = now
            rxRate.sample(total, now)
        }

        fun addTx(bytes: Int, now: Long) {
            val total = txBytes.addAndGet(bytes.toLong())
            txPackets.incrementAndGet()
            txRate.sample(total, now)
        }
    }

    data class LinkSnapshot(
        val id: Int,
        val label: String,
        val iface: String?,
        val rxBps: Long,
        val txBps: Long,
        val rttUs: Long,
        val rxBytes: Long,
        val txBytes: Long,
        val rxPackets: Long,
        val txPackets: Long,
        val alive: Boolean
    )

    data class Snapshot(
        val status: Status,
        val error: String?,
        val session: Int,
        val mtu: Int,
        val uptimeMs: Long,
        val links: List<LinkSnapshot>,
        val totalRxBps: Long,
        val totalTxBps: Long,
        val rxBytes: Long,
        val txBytes: Long,
        val delivered: Long,
        val malformed: Long,
        val badPayload: Long,
        val pending: Int
    ) {
        companion object {
            fun idle(): Snapshot = Snapshot(
                status = Status.DISCONNECTED,
                error = null,
                session = 0,
                mtu = 0,
                uptimeMs = 0L,
                links = emptyList(),
                totalRxBps = 0L,
                totalTxBps = 0L,
                rxBytes = 0L,
                txBytes = 0L,
                delivered = 0L,
                malformed = 0L,
                badPayload = 0L,
                pending = 0
            )
        }
    }

    @Volatile
    var status: Status = Status.DISCONNECTED

    @Volatile
    var error: String? = null

    @Volatile
    var session: Int = 0

    @Volatile
    var mtu: Int = 0

    @Volatile
    var startedAtMs: Long = 0L

    val delivered = AtomicLong()
    val malformed = AtomicLong()
    val badPayload = AtomicLong()
    val sendFailures = AtomicLong()

    private val counters = ConcurrentHashMap<Int, LinkCounters>()

    fun reset(session: Int, now: Long = System.currentTimeMillis()) {
        this.session = session
        status = Status.CONNECTING
        error = null
        mtu = 0
        startedAtMs = now
        delivered.set(0)
        malformed.set(0)
        badPayload.set(0)
        sendFailures.set(0)
        counters.clear()
    }

    fun markConnected() {
        if (status != Status.ERROR) status = Status.CONNECTED
    }

    fun markDisconnected() {
        status = Status.DISCONNECTED
    }

    fun link(id: Int, label: String, iface: String?): LinkCounters {
        val c = counters.getOrPut(id) { LinkCounters(id) }
        c.label = label
        c.iface = iface
        return c
    }

    fun dropLink(id: Int) {
        counters.remove(id)
    }

    fun onTx(id: Int, bytes: Int, now: Long) {
        counters[id]?.addTx(bytes, now)
    }

    fun onRx(id: Int, bytes: Int, now: Long) {
        counters[id]?.addRx(bytes, now)
    }

    fun setRtt(id: Int, rttUs: Long) {
        counters[id]?.rttUs = rttUs
    }

    fun setPending(value: Int) {
        pendingCount = value
    }

    @Volatile
    private var pendingCount: Int = 0

    fun snapshot(now: Long = System.currentTimeMillis()): Snapshot {
        val links = counters.values.sortedBy { it.id }.map { c ->
            LinkSnapshot(
                id = c.id,
                label = c.label,
                iface = c.iface,
                rxBps = c.rxRate.bps,
                txBps = c.txRate.bps,
                rttUs = c.rttUs,
                rxBytes = c.rxBytes.get(),
                txBytes = c.txBytes.get(),
                rxPackets = c.rxPackets.get(),
                txPackets = c.txPackets.get(),
                alive = c.lastRxMs > 0 && now - c.lastRxMs < 3_000L
            )
        }
        return Snapshot(
            status = status,
            error = error,
            session = session,
            mtu = mtu,
            uptimeMs = if (startedAtMs == 0L) 0L else (now - startedAtMs).coerceAtLeast(0L),
            links = links,
            totalRxBps = links.sumOf { it.rxBps },
            totalTxBps = links.sumOf { it.txBps },
            rxBytes = links.sumOf { it.rxBytes },
            txBytes = links.sumOf { it.txBytes },
            delivered = delivered.get(),
            malformed = malformed.get(),
            badPayload = badPayload.get(),
            pending = pendingCount
        )
    }
}
