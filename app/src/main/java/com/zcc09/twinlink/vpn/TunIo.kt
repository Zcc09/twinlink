package com.zcc09.twinlink.vpn

/**
 * The TUN side of the tunnel as the engine sees it. Production wraps the ParcelFileDescriptor,
 * the unit tests wrap two byte queues.
 */
interface TunIo {
    /** Reads one IP packet. Returns the length, 0 when nothing is queued, -1 when closed. */
    fun read(buf: ByteArray): Int

    /** Writes one IP packet. Returns bytes written, <= 0 on failure. */
    fun write(pkt: ByteArray): Int

    fun close()
}
