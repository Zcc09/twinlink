package com.zcc09.twinlink.proto

import java.nio.ByteBuffer
import java.nio.ByteOrder

/**
 * Twinlink protocol v1 constants and the 12-byte datagram header (PROTOCOL.md §2, §3).
 *
 * This file is pure Kotlin (no android.* imports) so it can run in JVM unit tests.
 */
object Proto {
    const val VERSION = 1

    const val HEADER_LEN = 12
    const val MAX_PAYLOAD = 1400
    const val MAX_DATAGRAM = HEADER_LEN + MAX_PAYLOAD

    const val MTU = 1380
    const val DEFAULT_PORT = 51821
    const val CLIENT_IP = "10.99.0.2"
    const val SERVER_IP = "10.99.0.1"
    const val DNS = "1.1.1.1"

    const val TYPE_HELLO = 1
    const val TYPE_HELLO_ACK = 2
    const val TYPE_DATA = 3
    const val TYPE_PING = 4
    const val TYPE_PONG = 5
    const val TYPE_STATS = 6
    const val TYPE_BYE = 7

    const val FLAG_LAST_FRAG = 0x01
    const val FLAG_AUTH = 0x02

    const val MAX_LINKS = 8

    fun isValidType(type: Int): Boolean = type in TYPE_HELLO..TYPE_BYE

    /** True when the payload starts with an IPv4 first nibble of 4 (PROTOCOL.md §3). */
    fun isIPv4(payload: ByteArray, offset: Int = 0): Boolean {
        if (payload.size <= offset) return false
        return ((payload[offset].toInt() ushr 4) and 0x0F) == 4
    }
}

/** Wrap-safe u32 sequence helpers (PROTOCOL.md §7). Never compare seq values with plain `<`. */
fun seqLess(a: Int, b: Int): Boolean = (a - b) < 0

fun seqGreater(a: Int, b: Int): Boolean = (a - b) > 0

/** Signed distance from [from] to [to] in the wrap-safe u32 space. */
fun seqDistance(from: Int, to: Int): Int = to - from

/** Wrap-safe three-way compare: negative when [a] is behind [b], zero when equal. */
fun seqCompare(a: Int, b: Int): Int = a - b

/** Decoded form of the fixed 12-byte header. All fields are unsigned as stored. */
class Header(
    val type: Int,
    val flags: Int,
    val session: Int,
    val seq: Int,
    val length: Int,
    val linkId: Int
) {
    fun encode(): ByteArray {
        val buf = ByteArray(Proto.HEADER_LEN)
        encodeInto(buf, 0)
        return buf
    }

    fun encodeInto(dest: ByteArray, offset: Int = 0) {
        val bb = ByteBuffer.wrap(dest, offset, Proto.HEADER_LEN).order(ByteOrder.BIG_ENDIAN)
        bb.put((((Proto.VERSION and 0x0F) shl 4) or (type and 0x0F)).toByte())
        bb.put((flags and 0xFF).toByte())
        bb.putShort((session and 0xFFFF).toShort())
        bb.putInt(seq)
        bb.putShort((length and 0xFFFF).toShort())
        bb.put((linkId and 0xFF).toByte())
        bb.put(0)
    }

    fun withLinkId(id: Int): Header = Header(type, flags, session, seq, length, id)

    override fun toString(): String =
        "Header(type=$type, flags=$flags, session=${session and 0xFFFF}, seq=${seq.toLong() and 0xFFFFFFFFL}, " +
            "len=$length, link=${linkId and 0xFF})"

    companion object {
        /**
         * Decodes and validates a datagram in one step: short datagrams, a foreign version, an
         * unknown type, an oversized payload and a declared length that does not match the
         * datagram size are all rejected with null (PROTOCOL.md §7).
         */
        fun decode(buf: ByteArray, offset: Int = 0, len: Int = buf.size - offset): Header? {
            if (len < Proto.HEADER_LEN) return null
            if (offset < 0 || offset + len > buf.size) return null

            val b0 = buf[offset].toInt() and 0xFF
            val version = (b0 ushr 4) and 0x0F
            if (version != Proto.VERSION) return null
            val type = b0 and 0x0F
            if (!Proto.isValidType(type)) return null

            val bb = ByteBuffer.wrap(buf, offset, Proto.HEADER_LEN).order(ByteOrder.BIG_ENDIAN)
            bb.position(offset)
            bb.get() // ver_type
            val flags = bb.get().toInt() and 0xFF
            val session = bb.getShort().toInt() and 0xFFFF
            if (session == 0) return null
            val seq = bb.getInt()
            val length = bb.getShort().toInt() and 0xFFFF
            val linkId = bb.get().toInt() and 0xFF
            bb.get() // reserved

            if (length > Proto.MAX_PAYLOAD) return null
            if (length != len - Proto.HEADER_LEN) return null

            return Header(type, flags, session, seq, length, linkId)
        }
    }
}
