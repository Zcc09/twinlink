package com.zcc09.twinlink.vpn

import com.zcc09.twinlink.net.LinkEndpoint
import com.zcc09.twinlink.net.LinkPool
import com.zcc09.twinlink.proto.ControlJson
import com.zcc09.twinlink.proto.Header
import com.zcc09.twinlink.proto.Proof
import com.zcc09.twinlink.proto.Proto
import com.zcc09.twinlink.reorder.ReorderBuffer
import com.zcc09.twinlink.sched.LinkScheduler
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicInteger

/**
 * The tunnel core: TUN reader -> DATA -> scheduler -> one link socket; one receiver thread per
 * link -> reorder -> TUN write; HELLO every 30 s and PING every 1 s on every link.
 *
 * Deliberately free of android.* imports — the links and the TUN arrive as interfaces, so the
 * whole encode/schedule/reorder path is exercised by plain JVM unit tests.
 */
class TwinlinkEngine(
    private val tun: TunIo,
    private val pool: LinkPool,
    val session: Int,
    private val secret: String,
    private val name: String = "android",
    private val clockMs: () -> Long = { System.currentTimeMillis() },
    private val clockUs: () -> Long = { System.nanoTime() / 1_000L },
    val stats: TwinlinkStats = TwinlinkStats()
) {
    private val scheduler = LinkScheduler(clockMs)
    private val reorder = ReorderBuffer(clock = clockMs)
    private val txSeq = AtomicInteger(0)
    private val writeLock = Any()
    private val rxLock = Any()
    private val endpoints = ConcurrentHashMap<Int, LinkEndpoint>()
    private val receivers = ConcurrentHashMap<Int, Receiver>()

    @Volatile
    private var running = false

    @Volatile
    private var readerThread: Thread? = null

    @Volatile
    private var controlThread: Thread? = null

    /** PROTOCOL §4/§5 timers; overridable so a test can compress them. */
    var helloIntervalMs: Long = 30_000L
    var pingIntervalMs: Long = 1_000L
    var tickMs: Long = 10L

    val deliveredCount: Long get() = synchronized(reorder) { reorder.delivered }
    val pendingCount: Int get() = synchronized(reorder) { reorder.pendingCount }
    val txCount: Int get() = txSeq.get()
    val activeLinkIds: List<Int> get() = endpoints.keys.sorted()
    val isRunning: Boolean get() = running

    fun start() {
        if (running) return
        running = true
        stats.reset(session, clockMs())
        txSeq.set(0)
        synchronized(reorder) { reorder.reset(0) }
        refreshLinks()
        // Handshake first so the relay has registered every source address before DATA
        // arrives - but on its own thread: these are socket writes, and on Android the
        // caller is the service's main thread where any network I/O throws
        // NetworkOnMainThreadException.
        spawn("tl-handshake") {
            sendHelloAll()
            sendPingAll()
        }
        readerThread = spawn("tl-tun-reader") { readerLoop() }
        controlThread = spawn("tl-control") { controlLoop() }
    }

    fun stop() {
        if (!running) return
        running = false
        for (receiver in receivers.values) receiver.alive = false
        readerThread?.interrupt()
        controlThread?.interrupt()
        try {
            readerThread?.join(500)
        } catch (_: InterruptedException) {
        }
        try {
            controlThread?.join(500)
        } catch (_: InterruptedException) {
        }
        readerThread = null
        controlThread = null
        receivers.clear()
        endpoints.clear()
        scheduler.clear()
        stats.setPending(0)
        stats.markDisconnected()
    }

    // ---------------------------------------------------------------- TUN -> links

    private fun readerLoop() {
        val buf = ByteArray(Proto.MTU + 64)
        while (running) {
            val n = try {
                tun.read(buf)
            } catch (_: Throwable) {
                break
            }
            if (n < 0) break
            if (n == 0) {
                sleepQuiet(1)
                continue
            }
            if (n < 20) continue
            if (n > Proto.MAX_PAYLOAD) {
                stats.malformed.incrementAndGet()
                continue
            }
            sendData(buf, n)
        }
    }

    private fun sendData(buf: ByteArray, n: Int) {
        val now = clockMs()
        val link = scheduler.pick(n, now) ?: return
        val endpoint = endpoints[link.id] ?: return
        val seq = txSeq.getAndIncrement()
        val datagram = ByteArray(Proto.HEADER_LEN + n)
        Header(Proto.TYPE_DATA, 0, session, seq, n, link.id).encodeInto(datagram, 0)
        System.arraycopy(buf, 0, datagram, Proto.HEADER_LEN, n)
        if (endpoint.send(datagram, datagram.size)) {
            scheduler.recordSent(link.id, datagram.size, now)
            stats.onTx(link.id, datagram.size, now)
        } else {
            stats.sendFailures.incrementAndGet()
        }
    }

    // ---------------------------------------------------------------- links -> TUN

    private inner class Receiver(private val endpoint: LinkEndpoint) : Runnable {
        @Volatile
        var alive = true

        private val buf = ByteArray(Proto.MAX_DATAGRAM + 64)

        override fun run() {
            while (alive && running) {
                val n = try {
                    endpoint.receive(buf, 200)
                } catch (_: Throwable) {
                    -1
                }
                if (n <= 0) continue
                try {
                    handle(endpoint, buf, n)
                } catch (_: Throwable) {
                    // One bad datagram must never kill the link.
                }
            }
        }
    }

    private fun handle(endpoint: LinkEndpoint, buf: ByteArray, n: Int) {
        val header = Header.decode(buf, 0, n)
        if (header == null) {
            stats.malformed.incrementAndGet()
            return
        }
        if (header.session != session) {
            stats.malformed.incrementAndGet()
            return
        }
        val now = clockMs()
        scheduler.onDatagram(endpoint.linkId, now)
        stats.onRx(endpoint.linkId, n, now)
        val at = Proto.HEADER_LEN

        when (header.type) {
            Proto.TYPE_DATA -> {
                if (!Proto.isIPv4(buf, at)) {
                    stats.badPayload.incrementAndGet()
                    return
                }
                val payload = buf.copyOfRange(at, n)
                // The reorder decision and the TUN write must be one atomic step:
                // with one lock per operation, two link threads can each be handed
                // "the next packet in order" and then write them in the opposite
                // order, which is exactly the reordering this buffer exists to remove.
                synchronized(writeLock) {
                    val ready = synchronized(reorder) { reorder.onData(header.seq, payload, now) }
                    writeReady(ready)
                }
                stats.setPending(pendingCount)
            }

            Proto.TYPE_PING -> sendControl(
                endpoint,
                Proto.TYPE_PONG,
                buf.copyOfRange(at, at + header.length)
            )

            Proto.TYPE_PONG -> {
                if (header.length >= 8) {
                    val sentUs = readU64(buf, at)
                    val rtt = clockUs() - sentUs
                    if (rtt in 1..30_000_000L) {
                        endpoint.rttUs = rtt
                        scheduler.onRttSample(endpoint.linkId, rtt)
                        stats.setRtt(endpoint.linkId, rtt)
                    }
                }
            }

            Proto.TYPE_HELLO_ACK -> {
                val ack = ControlJson.parseHelloAck(buf, at, header.length)
                if (ack != null) {
                    stats.mtu = ack.mtu
                    stats.markConnected()
                }
            }

            Proto.TYPE_BYE -> {
                val reason = ControlJson.parseError(buf, at, header.length) ?: "session closed by relay"
                stats.error = reason
                stats.status = TwinlinkStats.Status.ERROR
            }

            else -> Unit
        }
    }

    private fun writeReady(ready: List<ByteArray>) {
        if (ready.isEmpty()) return
        synchronized(writeLock) {
            for (payload in ready) {
                val written = try {
                    tun.write(payload)
                } catch (_: Throwable) {
                    -1
                }
                if (written > 0) stats.delivered.incrementAndGet()
            }
        }
    }

    // ---------------------------------------------------------------- control plane

    private fun controlLoop() {
        // start() already sent the first HELLO and PING on every link.
        var lastPing = clockMs()
        var lastHello = clockMs()
        while (running) {
            val now = clockMs()
            refreshLinks()
            if (now - lastHello >= helloIntervalMs) {
                lastHello = now
                sendHelloAll()
            }
            if (now - lastPing >= pingIntervalMs) {
                lastPing = now
                sendPingAll()
            }
            synchronized(writeLock) {
                val ready = synchronized(reorder) { reorder.tick(now) }
                writeReady(ready)
            }
            stats.setPending(pendingCount)
            sleepQuiet(tickMs)
        }
    }

    private fun refreshLinks() {
        val current = try {
            pool.endpoints()
        } catch (_: Throwable) {
            emptyList<LinkEndpoint>()
        }
        val seen = HashSet<Int>(current.size)
        for (endpoint in current) {
            seen.add(endpoint.linkId)
            val previous = endpoints.put(endpoint.linkId, endpoint)
            scheduler.register(endpoint.linkId, clockMs())
            stats.link(endpoint.linkId, endpoint.label, endpoint.interfaceName)
            if (previous !== endpoint || receivers[endpoint.linkId] == null) {
                receivers.remove(endpoint.linkId)?.alive = false
                val receiver = Receiver(endpoint)
                receivers[endpoint.linkId] = receiver
                spawn("tl-rx-${endpoint.linkId}") { receiver.run() }
            }
        }
        for (id in endpoints.keys.toList()) {
            if (id !in seen) {
                endpoints.remove(id)
                receivers.remove(id)?.alive = false
                scheduler.unregister(id)
                stats.dropLink(id)
            }
        }
    }

    private fun sendHelloAll() {
        val ts = clockMs() / 1000L
        val proof = Proof.hmacProof(secret, session, ts)
        val payload = ControlJson.hello(ts, proof, Proto.MTU, name)
        for (endpoint in endpoints.values) {
            sendControl(endpoint, Proto.TYPE_HELLO, payload, Proto.FLAG_AUTH)
        }
    }

    private fun sendPingAll() {
        val us = clockUs()
        val payload = writeU64(us)
        for (endpoint in endpoints.values) {
            sendControl(endpoint, Proto.TYPE_PING, payload)
        }
    }

    private fun sendControl(endpoint: LinkEndpoint, type: Int, payload: ByteArray, flags: Int = 0) {
        if (payload.size > Proto.MAX_PAYLOAD) return
        val datagram = ByteArray(Proto.HEADER_LEN + payload.size)
        Header(type, flags, session, 0, payload.size, endpoint.linkId).encodeInto(datagram, 0)
        System.arraycopy(payload, 0, datagram, Proto.HEADER_LEN, payload.size)
        if (endpoint.send(datagram, datagram.size)) {
            stats.onTx(endpoint.linkId, datagram.size, clockMs())
        }
    }

    // ---------------------------------------------------------------- helpers

    private fun spawn(name: String, body: () -> Unit): Thread =
        Thread(body, name).apply {
            isDaemon = true
            start()
        }

    private fun sleepQuiet(ms: Long) {
        try {
            Thread.sleep(ms.coerceAtLeast(0L))
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
        }
    }

    private fun readU64(buf: ByteArray, offset: Int): Long {
        var value = 0L
        for (i in 0 until 8) value = (value shl 8) or (buf[offset + i].toLong() and 0xFF)
        return value
    }

    private fun writeU64(value: Long): ByteArray {
        val out = ByteArray(8)
        for (i in 0 until 8) out[7 - i] = ((value ushr (8 * i)) and 0xFF).toByte()
        return out
    }

    companion object {
        /**
         * Sends one HELLO on [link] and returns the client address the relay assigned to this
         * session, or null when nothing came back.
         *
         * The tunnel interface needs that address *before* it is built, and the relay hands the
         * same session a different address when another session already holds the default one.
         * Skipping this makes a second device silently unusable: its packets carry somebody
         * else's source address and the relay drops them.
         */
        fun preflightAddress(
            link: LinkEndpoint,
            session: Int,
            secret: String,
            name: String?,
            timeoutMs: Int = 2500
        ): String? {
            val ts = System.currentTimeMillis() / 1000L
            val payload = ControlJson.hello(ts, Proof.hmacProof(secret, session, ts), Proto.MTU, name)
            val datagram = ByteArray(Proto.HEADER_LEN + payload.size)
            Header(Proto.TYPE_HELLO, Proto.FLAG_AUTH, session, 0, payload.size, link.linkId)
                .encodeInto(datagram, 0)
            System.arraycopy(payload, 0, datagram, Proto.HEADER_LEN, payload.size)
            if (!link.send(datagram, datagram.size)) return null

            val buf = ByteArray(2048)
            val deadline = System.currentTimeMillis() + timeoutMs
            while (System.currentTimeMillis() < deadline) {
                val n = link.receive(buf, 250)
                if (n < Proto.HEADER_LEN) continue
                val header = Header.decode(buf, 0, n) ?: continue
                if (header.type != Proto.TYPE_HELLO_ACK) continue
                return ControlJson.parseHelloAck(buf, Proto.HEADER_LEN, n - Proto.HEADER_LEN)?.clientIp
            }
            return null
        }

        fun newSession(): Int {
            val session = kotlin.random.Random.nextInt(1, 0x10000)
            return session
        }
    }
}
