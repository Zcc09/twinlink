package com.zcc09.twinlink.vpn

/**
 * Process-wide hand-off between the VpnService (writer) and MainActivity (1 s reader).
 * Both run in the same process, so a plain singleton is enough; there is no Binder surface
 * for anybody else to reach.
 */
object TunnelBus {

    @Volatile
    var stats: TwinlinkStats? = null

    @Volatile
    var serviceRunning: Boolean = false

    @Volatile
    var lastError: String? = null

    fun snapshot(): TwinlinkStats.Snapshot {
        val current = stats
        if (current == null || !serviceRunning) {
            val error = lastError
            return if (error == null) {
                TwinlinkStats.Snapshot.idle()
            } else {
                TwinlinkStats.Snapshot.idle().copy(
                    status = TwinlinkStats.Status.ERROR,
                    error = error
                )
            }
        }
        return current.snapshot()
    }
}
