package com.zcc09.twinlink.net

/**
 * One bonded path (Wi-Fi, cellular, ethernet) as seen by the engine.
 *
 * Deliberately free of android.* imports: the engine and its tests only ever talk to this
 * interface, so the whole encode/schedule/reorder path runs in a plain JVM.
 */
interface LinkEndpoint {
    val linkId: Int
    val label: String
    val interfaceName: String?

    /** Last measured round trip in microseconds, or -1 when unknown. */
    var rttUs: Long

    /** Sends one datagram. Returns false when the socket refused it (link down, etc). */
    fun send(bytes: ByteArray, len: Int): Boolean

    /** Blocking read with timeout. Returns the datagram length, or -1 when nothing arrived. */
    fun receive(buf: ByteArray, timeoutMs: Int): Int

    fun close()
}

/** The engine refreshes its link set from this each tick, so links can come and go live. */
interface LinkPool {
    fun endpoints(): List<LinkEndpoint>
}
