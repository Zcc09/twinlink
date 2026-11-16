package com.zcc09.twinlink.vpn

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.VpnService
import android.os.Build
import android.os.ParcelFileDescriptor
import android.system.ErrnoException
import android.system.Os
import android.system.OsConstants
import android.util.Log
import androidx.core.app.NotificationCompat
import com.zcc09.twinlink.R
import com.zcc09.twinlink.net.LinkManager
import com.zcc09.twinlink.proto.Proto
import com.zcc09.twinlink.ui.MainActivity
import java.net.DatagramSocket

/**
 * Foreground VpnService that owns the TUN and the bonded link set.
 *
 * VpnService.Builder per PROTOCOL.md §10: 10.99.0.2/24, all IPv4 routed in, DNS 1.1.1.1,
 * MTU 1380, non-blocking, and no IPv6 route (v1 does not tunnel IPv6).
 */
class TwinlinkVpnService : VpnService() {

    private var engine: TwinlinkEngine? = null
    private var linkManager: LinkManager? = null
    private var tunFd: ParcelFileDescriptor? = null
    private var tunIo: FdTun? = null
    private var stats: TwinlinkStats? = null

    override fun onCreate() {
        super.onCreate()
        createChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopTunnel()
                stopSelf()
                return START_NOT_STICKY
            }

            else -> {
                startInForeground()
                val prefs = getSharedPreferences(PREFS, MODE_PRIVATE)
                val host = intent?.getStringExtra(EXTRA_HOST)
                    ?: prefs.getString(KEY_HOST, null)
                val port = intent?.getIntExtra(EXTRA_PORT, 0)?.takeIf { it in 1..65535 }
                    ?: prefs.getInt(KEY_PORT, 51821)
                val secret = intent?.getStringExtra(EXTRA_SECRET)
                    ?: prefs.getString(KEY_SECRET, null)
                if (host.isNullOrBlank()) {
                    fail("Error: no relay host configured")
                    return START_NOT_STICKY
                }
                if (secret == null || secret.length < 16) {
                    fail("Error: secret must be at least 16 characters")
                    return START_NOT_STICKY
                }
                if (engine == null) startTunnel(host, port, secret)
            }
        }
        return START_STICKY
    }

    private fun startTunnel(host: String, port: Int, secret: String) {
        val session = TwinlinkEngine.newSession()
        val currentStats = TwinlinkStats()
        currentStats.reset(session)
        stats = currentStats
        TunnelBus.stats = currentStats
        TunnelBus.lastError = null
        TunnelBus.serviceRunning = true
        notifyForeground()

        val builder = Builder()
            .setSession("Twinlink")
            .addAddress(Proto.CLIENT_IP, 24)
            .addRoute("0.0.0.0", 0)
            .addDnsServer(Proto.DNS)
            .setMtu(Proto.MTU)
            .setBlocking(false)

        // The app's own sockets must never be captured by its own tunnel.
        try {
            builder.addDisallowedApplication(packageName)
        } catch (e: Exception) {
            Log.w(TAG, "addDisallowedApplication failed", e)
        }

        val fd = try {
            builder.establish()
        } catch (e: Exception) {
            Log.e(TAG, "establish failed", e)
            null
        }
        if (fd == null) {
            fail("Error: could not establish the tunnel")
            return
        }
        tunFd = fd

        val linkStats = currentStats
        val manager = LinkManager(
            context = this,
            host = host,
            port = port,
            protectSocket = { socket: DatagramSocket -> protect(socket) },
            onChanged = { links ->
                for (endpoint in links) linkStats.link(endpoint.linkId, endpoint.label, endpoint.interfaceName)
            }
        )
        manager.start()
        linkManager = manager

        val tunnel = FdTun(fd)
        tunIo = tunnel
        val running = TwinlinkEngine(
            tun = tunnel,
            pool = manager,
            session = session,
            secret = secret,
            name = Build.MODEL ?: "android",
            stats = currentStats
        )
        engine = running
        running.start()
        Log.i(TAG, "tunnel up: session=${session.toString(16)} relay=$host:$port links=${manager.endpoints().size}")
    }

    private fun stopTunnel() {
        engine?.stop()
        engine = null
        linkManager?.stop()
        linkManager = null
        tunIo?.close()
        tunIo = null
        try {
            tunFd?.close()
        } catch (_: Exception) {
        }
        tunFd = null
        stats?.markDisconnected()
        stats = null
        TunnelBus.serviceRunning = false
        TunnelBus.stats = null
        stopForegroundCompat()
    }

    private fun fail(message: String) {
        val current = stats ?: TwinlinkStats().also { stats = it }
        current.error = message.removePrefix("Error: ")
        current.status = TwinlinkStats.Status.ERROR
        TunnelBus.stats = current
        TunnelBus.lastError = message.removePrefix("Error: ")
        TunnelBus.serviceRunning = false
        engine?.stop()
        engine = null
        linkManager?.stop()
        linkManager = null
        tunIo?.close()
        tunIo = null
        try {
            tunFd?.close()
        } catch (_: Exception) {
        }
        tunFd = null
        stopForegroundCompat()
        stopSelf()
    }

    override fun onRevoke() {
        Log.i(TAG, "VPN permission revoked by the system or another VPN")
        stopTunnel()
        stopSelf()
        super.onRevoke()
    }

    override fun onDestroy() {
        stopTunnel()
        super.onDestroy()
    }

    // ---------------------------------------------------------------- notification

    private fun createChannel() {
        val manager = getSystemService(NotificationManager::class.java) ?: return
        if (manager.getNotificationChannel(CHANNEL_ID) != null) return
        val channel = NotificationChannel(
            CHANNEL_ID,
            getString(R.string.notification_channel),
            NotificationManager.IMPORTANCE_LOW
        )
        channel.setShowBadge(false)
        channel.lockscreenVisibility = Notification.VISIBILITY_PUBLIC
        manager.createNotificationChannel(channel)
    }

    private fun buildNotification(): Notification {
        val open = PendingIntent.getActivity(
            this,
            1,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val stop = PendingIntent.getService(
            this,
            2,
            Intent(this, TwinlinkVpnService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_link)
            .setContentTitle(getString(R.string.notification_title))
            .setContentText(getString(R.string.notification_text))
            .setContentIntent(open)
            .addAction(0, getString(R.string.notification_stop), stop)
            .setOngoing(true)
            .setShowWhen(false)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .build()
    }

    private fun startInForeground() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(
                NOTIFICATION_ID,
                buildNotification(),
                ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC
            )
        } else {
            startForeground(NOTIFICATION_ID, buildNotification())
        }
    }

    private fun notifyForeground() = startInForeground()

    private fun stopForegroundCompat() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
            stopForeground(STOP_FOREGROUND_REMOVE)
        } else {
            @Suppress("DEPRECATION")
            stopForeground(true)
        }
    }

    /** TUN access through the raw fd: non-blocking reads report EAGAIN as "nothing queued". */
    private class FdTun(private val pfd: ParcelFileDescriptor) : TunIo {
        private val fd = pfd.fileDescriptor

        override fun read(buf: ByteArray): Int = try {
            Os.read(fd, buf, 0, buf.size)
        } catch (e: ErrnoException) {
            // EAGAIN == EWOULDBLOCK on Linux: the TUN is empty.
            if (e.errno == OsConstants.EAGAIN) 0 else -1
        }

        override fun write(pkt: ByteArray): Int = try {
            Os.write(fd, pkt, 0, pkt.size)
        } catch (e: ErrnoException) {
            if (e.errno == OsConstants.EAGAIN) 0 else -1
        }

        override fun close() {
            try {
                pfd.close()
            } catch (_: Exception) {
            }
        }
    }

    companion object {
        private const val TAG = "TwinlinkService"
        const val ACTION_START = "com.zcc09.twinlink.START"
        const val ACTION_STOP = "com.zcc09.twinlink.STOP"
        const val EXTRA_HOST = "host"
        const val EXTRA_PORT = "port"
        const val EXTRA_SECRET = "secret"
        const val CHANNEL_ID = "tunnel"
        const val NOTIFICATION_ID = 41
        const val PREFS = "twinlink"
        const val KEY_HOST = "relay_host"
        const val KEY_PORT = "relay_port"
        const val KEY_SECRET = "relay_secret"
        const val DEFAULT_PORT = 51821
    }
}
