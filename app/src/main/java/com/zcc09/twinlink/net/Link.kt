package com.zcc09.twinlink.net

import android.net.Network
import java.io.IOException
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.SocketTimeoutException

/**
 * A UDP socket bound to exactly one [Network] (PROTOCOL.md §1). Every bonded path owns one of
 * these, so a datagram leaves over the interface it was scheduled on and nowhere else.
 */
class Link(
    override val linkId: Int,
    private val socket: DatagramSocket,
    private val relay: InetAddress,
    private val port: Int,
    private val network: Network,
    override val label: String,
    override val interfaceName: String?
) : LinkEndpoint {

    @Volatile
    override var rttUs: Long = -1L

    @Volatile
    var lastSendOk: Boolean = true
        private set

    private val sendLock = Any()

    override fun send(bytes: ByteArray, len: Int): Boolean {
        return try {
            synchronized(sendLock) {
                socket.send(DatagramPacket(bytes, 0, len, relay, port))
            }
            lastSendOk = true
            true
        } catch (_: IOException) {
            lastSendOk = false
            false
        }
    }

    override fun receive(buf: ByteArray, timeoutMs: Int): Int {
        return try {
            socket.soTimeout = timeoutMs
            val packet = DatagramPacket(buf, buf.size)
            socket.receive(packet)
            packet.length
        } catch (_: SocketTimeoutException) {
            -1
        } catch (_: IOException) {
            -1
        }
    }

    override fun close() {
        try {
            socket.close()
        } catch (_: Exception) {
            // already closed
        }
    }

    fun networkHandle(): Network = network

    override fun toString(): String = "Link($linkId, $label/$interfaceName)"
}
