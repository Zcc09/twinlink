package com.zcc09.twinlink.proto

import org.json.JSONObject

/** HELLO_ACK fields we care about (PROTOCOL.md §4). */
data class HelloAck(
    val version: Int,
    val clientIp: String,
    val serverIp: String,
    val mtu: Int,
    val maxPayload: Int
)

/**
 * Control-message JSON, built and parsed only at the edges with org.json.
 * The HMAC itself lives in [Proof] and stays free of org.json.
 */
object ControlJson {

    fun hello(ts: Long, proof: String, mtu: Int = Proto.MTU, name: String? = null): ByteArray {
        val obj = JSONObject()
        obj.put("v", Proto.VERSION)
        obj.put("ts", ts)
        obj.put("proof", proof)
        obj.put("mtu", mtu)
        if (!name.isNullOrEmpty()) obj.put("name", name)
        return obj.toString().toByteArray(Charsets.UTF_8)
    }

    fun parseHelloAck(payload: ByteArray, offset: Int = 0, length: Int = payload.size - offset): HelloAck? {
        val obj = parse(payload, offset, length) ?: return null
        return HelloAck(
            version = obj.optInt("v", Proto.VERSION),
            clientIp = obj.optString("client_ip", Proto.CLIENT_IP),
            serverIp = obj.optString("server_ip", Proto.SERVER_IP),
            mtu = obj.optInt("mtu", Proto.MTU),
            maxPayload = obj.optInt("max_payload", Proto.MAX_PAYLOAD)
        )
    }

    /** BYE carries {"err":"..."}; returns the reason. */
    fun parseError(payload: ByteArray, offset: Int = 0, length: Int = payload.size - offset): String? {
        val obj = parse(payload, offset, length) ?: return null
        return obj.optString("err", "").ifEmpty { null }
    }

    private fun parse(payload: ByteArray, offset: Int, length: Int): JSONObject? {
        if (offset < 0 || length <= 0 || offset + length > payload.size) return null
        return try {
            JSONObject(String(payload, offset, length, Charsets.UTF_8))
        } catch (_: Exception) {
            null
        }
    }
}
