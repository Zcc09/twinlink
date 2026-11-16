package com.zcc09.twinlink.proto

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ProtoTest {

    private val session = 0x1a2b

    private fun datagram(type: Int, payload: ByteArray, flags: Int = 0, seq: Int = 0, link: Int = 0): ByteArray {
        val header = Header(type, flags, session, seq, payload.size, link)
        val out = ByteArray(Proto.HEADER_LEN + payload.size)
        header.encodeInto(out, 0)
        System.arraycopy(payload, 0, out, Proto.HEADER_LEN, payload.size)
        return out
    }

    @Test
    fun everyMessageTypeRoundTrips() {
        for (type in Proto.TYPE_HELLO..Proto.TYPE_BYE) {
            val payload = ByteArray(24) { (it * 3 + type).toByte() }
            if (type == Proto.TYPE_DATA) payload[0] = 0x45
            val flags = if (type == Proto.TYPE_HELLO) Proto.FLAG_AUTH else 0
            val bytes = datagram(type, payload, flags, seq = 0x01020304, link = 5)
            val decoded = Header.decode(bytes, 0, bytes.size)
            requireNotNull(decoded) { "type $type did not decode" }
            assertEquals(type, decoded.type)
            assertEquals(flags, decoded.flags)
            assertEquals(session, decoded.session)
            assertEquals(0x01020304, decoded.seq)
            assertEquals(payload.size, decoded.length)
            assertEquals(5, decoded.linkId)
            assertArrayEquals(
                payload,
                bytes.copyOfRange(Proto.HEADER_LEN, bytes.size)
            )
        }
    }

    @Test
    fun headerLayoutMatchesTheWireFormat() {
        val bytes = Header(Proto.TYPE_DATA, Proto.FLAG_AUTH, 0xABCD, 0x11223344, 1380, 7).encode()
        assertEquals(Proto.HEADER_LEN, bytes.size)
        assertEquals(0x13, bytes[0].toInt() and 0xFF) // version 1, type 3
        assertEquals(0x02, bytes[1].toInt() and 0xFF)
        assertEquals(0xAB, bytes[2].toInt() and 0xFF)
        assertEquals(0xCD, bytes[3].toInt() and 0xFF)
        assertEquals(0x11, bytes[4].toInt() and 0xFF)
        assertEquals(0x22, bytes[5].toInt() and 0xFF)
        assertEquals(0x33, bytes[6].toInt() and 0xFF)
        assertEquals(0x44, bytes[7].toInt() and 0xFF)
        assertEquals(0x05, bytes[8].toInt() and 0xFF) // 1380 = 0x0564
        assertEquals(0x64, bytes[9].toInt() and 0xFF)
        assertEquals(7, bytes[10].toInt() and 0xFF)
        assertEquals(0, bytes[11].toInt() and 0xFF)
    }

    @Test
    fun shortDatagramIsRejected() {
        assertNull(Header.decode(ByteArray(Proto.HEADER_LEN - 1)))
        assertNull(Header.decode(ByteArray(0)))
    }

    @Test
    fun oversizedPayloadIsRejected() {
        val payload = ByteArray(Proto.MAX_PAYLOAD + 1)
        val bytes = datagram(Proto.TYPE_DATA, payload)
        assertNull(Header.decode(bytes, 0, bytes.size))
    }

    @Test
    fun declaredLengthMustMatchDatagram() {
        val bytes = datagram(Proto.TYPE_DATA, ByteArray(10))
        // Claim 12 payload bytes in a datagram that carries 10.
        bytes[8] = 0
        bytes[9] = 12
        assertNull(Header.decode(bytes, 0, bytes.size))

        // Claim 8 in a datagram that carries 10.
        bytes[9] = 8
        assertNull(Header.decode(bytes, 0, bytes.size))
    }

    @Test
    fun foreignVersionIsRejected() {
        val bytes = datagram(Proto.TYPE_DATA, ByteArray(4))
        bytes[0] = (0x20 or Proto.TYPE_DATA).toByte()
        assertNull(Header.decode(bytes, 0, bytes.size))
    }

    @Test
    fun unknownTypeIsRejected() {
        val bytes = datagram(Proto.TYPE_DATA, ByteArray(4))
        bytes[0] = (0x10 or 0x09).toByte()
        assertNull(Header.decode(bytes, 0, bytes.size))
    }

    @Test
    fun zeroSessionIsRejected() {
        val bytes = datagram(Proto.TYPE_DATA, ByteArray(4))
        bytes[2] = 0
        bytes[3] = 0
        assertNull(Header.decode(bytes, 0, bytes.size))
    }

    @Test
    fun decodeHonoursOffsetAndLength() {
        val inner = datagram(Proto.TYPE_PING, ByteArray(8) { 9 })
        val wrapped = ByteArray(3 + inner.size + 5)
        System.arraycopy(inner, 0, wrapped, 3, inner.size)
        val decoded = Header.decode(wrapped, 3, inner.size)
        requireNotNull(decoded)
        assertEquals(Proto.TYPE_PING, decoded.type)
        assertEquals(8, decoded.length)
    }

    @Test
    fun sequenceComparisonIsWrapSafe() {
        assertTrue(seqLess(0, 1))
        assertFalse(seqLess(1, 0))
        assertTrue(seqLess(Int.MAX_VALUE, Int.MIN_VALUE)) // MAX + 1 wraps to MIN
        assertFalse(seqLess(Int.MIN_VALUE, Int.MAX_VALUE))
        assertEquals(1, seqDistance(-1, 0)) // 0xFFFFFFFF -> 0
        assertEquals(1, seqDistance(Int.MAX_VALUE, Int.MIN_VALUE))
        assertFalse(seqLess(5, 5))
    }

    @Test
    fun ipv4NibbleIsDetected() {
        assertTrue(Proto.isIPv4(byteArrayOf(0x45, 0, 0, 0)))
        assertFalse(Proto.isIPv4(byteArrayOf(0x60, 0, 0, 0)))
        assertFalse(Proto.isIPv4(ByteArray(0)))
    }

    @Test
    fun hmacProofMatchesTheProtocolVectors() {
        val secret = "twinlink-test-secret"
        assertEquals("twinlink-v1|1a2b|1737000000", Proof.message(0x1a2b, 1737000000L))
        assertEquals(
            "6ef1b4cbb185afd7ed31c37926e9ffda827911cc178389e564b72afae14fb7fc",
            Proof.hmacProof(secret, 0x1a2b, 1737000000L)
        )
        assertEquals(
            "cbb14259f7bc39a2971758cee8accfeae8960ee0457fa8dc0986cee0d6327a40",
            Proof.hmacProof(secret, 0x1a2b, 1737000060L)
        )
    }

    @Test
    fun proofVerificationRejectsATamperedSecret() {
        val proof = Proof.hmacProof("twinlink-test-secret", 0x1a2b, 1737000000L)
        assertTrue(Proof.verify("twinlink-test-secret", 0x1a2b, 1737000000L, proof))
        assertFalse(Proof.verify("twinlink-test-secreu", 0x1a2b, 1737000000L, proof))
        assertFalse(Proof.verify("twinlink-test-secret", 0x1a2b, 1737000001L, proof))
    }

    @Test
    fun sessionHexIsFourPaddedLowercaseDigits() {
        assertEquals("1a2b", Proof.hexSession(0x1a2b))
        assertEquals("00ff", Proof.hexSession(0xff))
        assertEquals("ffff", Proof.hexSession(0xffff))
    }

    @Test
    fun helloPayloadCarriesTheHandshakeFields() {
        val ts = 1737000000L
        val proof = Proof.hmacProof("twinlink-test-secret", 0x1a2b, ts)
        val json = String(ControlJson.hello(ts, proof, Proto.MTU, "pixel"), Charsets.UTF_8)
        assertTrue(json.contains("\"v\":1"))
        assertTrue(json.contains("\"ts\":$ts"))
        assertTrue(json.contains("\"proof\":\"$proof\""))
        assertTrue(json.contains("\"mtu\":1380"))
        assertTrue(json.contains("\"name\":\"pixel\""))
    }

    private fun assertArrayEquals(expected: ByteArray, actual: ByteArray) {
        assertEquals(expected.size, actual.size)
        for (i in expected.indices) assertEquals("byte $i", expected[i], actual[i])
    }
}
