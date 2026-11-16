package com.zcc09.twinlink.proto

import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

/**
 * HELLO authentication (PROTOCOL.md §4): lowercase hex HMAC-SHA256 over
 * "twinlink-v1|<hex session>|<decimal ts>" keyed with the UTF-8 shared secret.
 *
 * Pure Kotlin (javax.crypto only) so the unit tests can pin the vectors.
 */
object Proof {
    const val PREFIX = "twinlink-v1"

    /** Lowercase, zero-padded 4-digit hex of the u16 session id. */
    fun hexSession(session: Int): String {
        val v = session and 0xFFFF
        val digits = "0123456789abcdef"
        val out = CharArray(4)
        out[0] = digits[(v ushr 12) and 0x0F]
        out[1] = digits[(v ushr 8) and 0x0F]
        out[2] = digits[(v ushr 4) and 0x0F]
        out[3] = digits[v and 0x0F]
        return String(out)
    }

    fun message(session: Int, ts: Long): String = "$PREFIX|${hexSession(session)}|$ts"

    fun hmacHex(secret: String, message: String): String {
        val mac = Mac.getInstance("HmacSHA256")
        mac.init(SecretKeySpec(secret.toByteArray(Charsets.UTF_8), "HmacSHA256"))
        return toHex(mac.doFinal(message.toByteArray(Charsets.UTF_8)))
    }

    fun hmacProof(secret: String, session: Int, ts: Long): String = hmacHex(secret, message(session, ts))

    fun verify(secret: String, session: Int, ts: Long, proof: String): Boolean {
        val expected = hmacProof(secret, session, ts)
        return constantTimeEquals(expected, proof.lowercase())
    }

    private fun constantTimeEquals(a: String, b: String): Boolean {
        if (a.length != b.length) return false
        var diff = 0
        for (i in a.indices) diff = diff or (a[i].code xor b[i].code)
        return diff == 0
    }

    fun toHex(bytes: ByteArray): String {
        val digits = "0123456789abcdef"
        val out = CharArray(bytes.size * 2)
        for (i in bytes.indices) {
            val v = bytes[i].toInt() and 0xFF
            out[i * 2] = digits[v ushr 4]
            out[i * 2 + 1] = digits[v and 0x0F]
        }
        return String(out)
    }
}
