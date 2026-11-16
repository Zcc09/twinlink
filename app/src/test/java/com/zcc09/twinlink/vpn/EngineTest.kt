package com.zcc09.twinlink.vpn

import com.zcc09.twinlink.net.LinkEndpoint
import com.zcc09.twinlink.net.LinkPool
import com.zcc09.twinlink.proto.Header
import com.zcc09.twinlink.proto.Proto
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.concurrent.Executors
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/**
 * Engine-level wiring test: real TwinlinkEngine threads, real header encode/decode, real
 * scheduler and reorder buffer. Only the TUN and the link sockets are fakes.
 */
class EngineTest {

    private val schedulerPool: ScheduledExecutorService = Executors.newScheduledThreadPool(4)

    @After
    fun tearDown() {
        schedulerPool.shutdownNow()
    }

    @Test(timeout = 120_000)
    fun fiveHundredDataPacketsArriveInOrderAcrossTwoLinks() {
        val tun = FakeTun()
        val fast = FakeLink(0, rttMs = 20, relayEchoMs = 30, executor = schedulerPool)
        val slow = FakeLink(1, rttMs = 120, relayEchoMs = 30, executor = schedulerPool)
        val engine = TwinlinkEngine(
            tun = tun,
            pool = FixedPool(listOf(fast, slow)),
            session = 0x1a2b,
            secret = "twinlink-test-secret"
        )
        engine.start()
        try {
            for (index in 0 until 500) tun.offer(ipPacket(index))

            val deadline = System.currentTimeMillis() + 30_000
            while (tun.written.size < 500 && System.currentTimeMillis() < deadline) {
                Thread.sleep(20)
            }

            assertEquals("delivered count", 500, tun.written.size)
            tun.written.forEachIndexed { expected, packet ->
                assertEquals("packet at position $expected", expected, indexOf(packet))
                assertEquals("payload size at $expected", IP_SIZE, packet.size)
            }
            assertEquals(500L, engine.deliveredCount)
            assertEquals(0, engine.pendingCount)

            // Both links really carried DATA: the bond was used, not just one socket.
            assertTrue("fast link carried nothing", fast.dataSent.get() > 0)
            assertTrue("slow link carried nothing", slow.dataSent.get() > 0)
            assertEquals(500, fast.dataSent.get() + slow.dataSent.get())
            // Every packet was transmitted exactly once.
            assertEquals(500, engine.txCount)
        } finally {
            engine.stop()
        }
    }

    @Test(timeout = 30_000)
    fun pingIsAnsweredWithAnIdenticalPongOnTheSameLink() {
        val tun = FakeTun()
        val link = FakeLink(0, rttMs = 10, relayEchoMs = 10, executor = schedulerPool)
        val engine = TwinlinkEngine(
            tun = tun,
            pool = FixedPool(listOf(link)),
            session = 0x1a2b,
            secret = "twinlink-test-secret"
        )
        engine.start()
        try {
            val payload = byteArrayOf(0, 0, 1, 2, 3, 4, 5, 6)
            link.deliver(datagram(Proto.TYPE_PING, payload, session = 0x1a2b))

            val pong = link.awaitPong(payload, 5_000)
            requireNotNull(pong) { "no PONG came back" }
            assertEquals(0x1a2b, Header.decode(pong, 0, pong.size)!!.session)
            assertEquals(Proto.HEADER_LEN + payload.size, pong.size)
            for (i in payload.indices) {
                assertEquals(payload[i], pong[Proto.HEADER_LEN + i])
            }
        } finally {
            engine.stop()
        }
    }

    @Test(timeout = 30_000)
    fun malformedAndForeignSessionDatagramsAreCountedNotFatal() {
        val tun = FakeTun()
        val link = FakeLink(0, rttMs = 10, relayEchoMs = 10, executor = schedulerPool)
        val engine = TwinlinkEngine(
            tun = tun,
            pool = FixedPool(listOf(link)),
            session = 0x1a2b,
            secret = "twinlink-test-secret"
        )
        engine.start()
        try {
            link.deliver(byteArrayOf(1, 2, 3)) // shorter than a header
            link.deliver(datagram(Proto.TYPE_DATA, ByteArray(8), session = 0x0099))
            Thread.sleep(300)
            assertTrue(engine.stats.malformed.get() >= 2)
            assertTrue(engine.isRunning)

            // The engine still carries traffic afterwards.
            tun.offer(ipPacket(7))
            val deadline = System.currentTimeMillis() + 5_000
            while (tun.written.isEmpty() && System.currentTimeMillis() < deadline) Thread.sleep(20)
            assertEquals(1, tun.written.size)
            assertEquals(7, indexOf(tun.written[0]))
        } finally {
            engine.stop()
        }
    }

    // ---------------------------------------------------------------- fakes

    private class FixedPool(private val links: List<LinkEndpoint>) : LinkPool {
        override fun endpoints(): List<LinkEndpoint> = links
    }

    private class FakeTun : TunIo {
        private val inbound = LinkedBlockingQueue<ByteArray>()
        val written = java.util.Collections.synchronizedList(ArrayList<ByteArray>())

        fun offer(packet: ByteArray) {
            inbound.offer(packet)
        }

        override fun read(buf: ByteArray): Int {
            val packet = inbound.poll(20, TimeUnit.MILLISECONDS) ?: return 0
            if (packet.size > buf.size) return 0
            System.arraycopy(packet, 0, buf, 0, packet.size)
            return packet.size
        }

        override fun write(pkt: ByteArray): Int {
            written.add(pkt.copyOf())
            return pkt.size
        }

        override fun close() {
            inbound.clear()
        }
    }

    /**
     * A link plus a stand-in relay. DATA comes back after a shared [relayEchoMs] (the relay's own
     * internet RTT is identical for both paths; the per-link difference is what PING measures and
     * what the scheduler's rtt tie-break sees). A spread wider than the 40 ms reorder hold is real
     * loss by design — ReorderBufferTest covers that path.
     */
    private class FakeLink(
        override val linkId: Int,
        private val rttMs: Long,
        private val relayEchoMs: Long,
        private val executor: ScheduledExecutorService
    ) : LinkEndpoint {

        override val label: String = "Fake $linkId"
        override val interfaceName: String = "fake$linkId"

        @Volatile
        override var rttUs: Long = rttMs * 1000

        val dataSent = AtomicInteger()
        private val inbox = LinkedBlockingQueue<ByteArray>()
        private val controlSent = LinkedBlockingQueue<ByteArray>()

        fun deliver(raw: ByteArray) {
            inbox.offer(raw)
        }

        fun awaitPong(expected: ByteArray, timeoutMs: Long): ByteArray? {
            val deadline = System.currentTimeMillis() + timeoutMs
            while (System.currentTimeMillis() < deadline) {
                val candidate = controlSent.poll(100, TimeUnit.MILLISECONDS) ?: continue
                val header = Header.decode(candidate, 0, candidate.size) ?: continue
                if (header.type != Proto.TYPE_PONG || header.length != expected.size) continue
                var match = true
                for (i in expected.indices) {
                    if (candidate[Proto.HEADER_LEN + i] != expected[i]) {
                        match = false
                        break
                    }
                }
                if (match) return candidate
            }
            return null
        }

        override fun send(bytes: ByteArray, len: Int): Boolean {
            val header = Header.decode(bytes, 0, len) ?: return false
            val copy = bytes.copyOf(len)
            when (header.type) {
                Proto.TYPE_DATA -> {
                    dataSent.incrementAndGet()
                    executor.schedule(
                        { inbox.offer(copy) },
                        relayEchoMs,
                        TimeUnit.MILLISECONDS
                    )
                }

                Proto.TYPE_PING -> {
                    // The stand-in relay answers PINGs after this link's own latency.
                    val pong = copy.copyOf()
                    pong[0] = ((Proto.VERSION shl 4) or Proto.TYPE_PONG).toByte()
                    executor.schedule(
                        { inbox.offer(pong) },
                        rttMs,
                        TimeUnit.MILLISECONDS
                    )
                }

                // Everything else the engine sent (HELLO, PONG) is recorded for assertions.
                else -> controlSent.offer(copy)
            }
            return true
        }

        override fun receive(buf: ByteArray, timeoutMs: Int): Int {
            val packet = inbox.poll(timeoutMs.toLong(), TimeUnit.MILLISECONDS) ?: return -1
            if (packet.size > buf.size) return -1
            System.arraycopy(packet, 0, buf, 0, packet.size)
            return packet.size
        }

        override fun close() {
            inbox.clear()
        }
    }

    private companion object {
        const val IP_SIZE = 60

        /** A 60-byte IPv4/UDP-shaped packet whose 4-byte index rides at offset 20. */
        fun ipPacket(index: Int): ByteArray {
            val packet = ByteArray(IP_SIZE) { 0x5A }
            packet[0] = 0x45
            packet[1] = 0x00
            packet[2] = ((IP_SIZE ushr 8) and 0xFF).toByte()
            packet[3] = (IP_SIZE and 0xFF).toByte()
            packet[8] = 64
            packet[9] = 17 // UDP
            packet[20] = ((index ushr 24) and 0xFF).toByte()
            packet[21] = ((index ushr 16) and 0xFF).toByte()
            packet[22] = ((index ushr 8) and 0xFF).toByte()
            packet[23] = (index and 0xFF).toByte()
            return packet
        }

        fun indexOf(packet: ByteArray): Int =
            ((packet[20].toInt() and 0xFF) shl 24) or
                ((packet[21].toInt() and 0xFF) shl 16) or
                ((packet[22].toInt() and 0xFF) shl 8) or
                (packet[23].toInt() and 0xFF)

        fun datagram(type: Int, payload: ByteArray, session: Int): ByteArray {
            val out = ByteArray(Proto.HEADER_LEN + payload.size)
            Header(type, 0, session, 0, payload.size, 0).encodeInto(out, 0)
            System.arraycopy(payload, 0, out, Proto.HEADER_LEN, payload.size)
            return out
        }
    }
}
