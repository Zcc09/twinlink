package com.zcc09.twinlink.net

import android.content.Context
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.util.Log
import java.net.DatagramSocket
import java.net.InetAddress
import java.util.concurrent.ConcurrentHashMap

/**
 * Watches ConnectivityManager for usable networks and keeps one bound UDP socket per network
 * (PROTOCOL.md §1). A network qualifies when it has INTERNET + VALIDATED and is a Wi-Fi,
 * cellular or ethernet transport.
 *
 * Sockets are `protect()`ed by the VpnService before they are bound, so tunnel traffic never
 * loops back into our own TUN.
 */
class LinkManager(
    context: Context,
    private val host: String,
    private val port: Int,
    private val protectSocket: ((DatagramSocket) -> Boolean)? = null,
    private val onChanged: (List<LinkEndpoint>) -> Unit = {}
) : LinkPool {

    private val cm = context.getSystemService(ConnectivityManager::class.java)
    private val links = ConcurrentHashMap<Int, Link>()

    private val relayAddress: InetAddress? = try {
        InetAddress.getByName(host)
    } catch (e: Exception) {
        Log.w(TAG, "relay host does not resolve: $host", e)
        null
    }

    @Volatile
    private var defaultNetwork: Network? = null

    private val callback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) = adopt(network, null)

        override fun onCapabilitiesChanged(network: Network, capabilities: NetworkCapabilities) =
            adopt(network, capabilities)

        override fun onLinkPropertiesChanged(network: Network, linkProperties: LinkProperties) =
            adopt(network, null)

        override fun onLost(network: Network) = drop(network)
    }

    private val defaultCallback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            defaultNetwork = network
        }

        override fun onLost(network: Network) {
            if (defaultNetwork == network) defaultNetwork = null
        }
    }

    fun start() {
        val manager = cm ?: return
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
            .build()
        try {
            manager.registerNetworkCallback(request, callback)
        } catch (e: Exception) {
            Log.w(TAG, "registerNetworkCallback failed", e)
        }
        try {
            manager.registerDefaultNetworkCallback(defaultCallback)
        } catch (e: Exception) {
            Log.w(TAG, "registerDefaultNetworkCallback failed", e)
        }
        cm.activeNetwork?.let { active ->
            cm.getNetworkCapabilities(active)?.let { adopt(active, it) }
        }
        onChanged(endpoints())
    }

    fun stop() {
        val manager = cm
        if (manager != null) {
            try {
                manager.unregisterNetworkCallback(callback)
            } catch (_: Exception) {
            }
            try {
                manager.unregisterNetworkCallback(defaultCallback)
            } catch (_: Exception) {
            }
        }
        synchronized(this) {
            for (link in links.values) link.close()
            links.clear()
        }
        onChanged(endpoints())
    }

    override fun endpoints(): List<LinkEndpoint> =
        links.values.sortedBy { it.linkId }.toList()

    fun currentLinks(): List<Link> = links.values.sortedBy { it.linkId }

    private fun adopt(network: Network, capabilities: NetworkCapabilities?) {
        val caps = capabilities ?: cm?.getNetworkCapabilities(network) ?: return
        val label = labelFor(caps)
        val usable = label != null &&
            caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
            caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        val address = relayAddress

        var changed = false
        synchronized(this) {
            val existing = links.entries.firstOrNull { it.value.networkHandle() == network }
            if (existing != null) {
                // A link that stops being usable (validated lost, transport gone) is retired.
                if (!usable || address == null) {
                    links.remove(existing.key)?.close()
                    Log.i(TAG, "link ${existing.key} retired: capability lost")
                    changed = true
                }
                // Otherwise keep the socket: rebinding it would break the NAT mapping.
            } else {
                if (!usable || address == null || label == null) return
                val socket = try {
                    DatagramSocket()
                } catch (e: Exception) {
                    Log.w(TAG, "socket open failed", e)
                    return
                }
                try {
                    protectSocket?.invoke(socket)
                    network.bindSocket(socket)
                } catch (e: Exception) {
                    Log.w(TAG, "bindSocket failed on $network", e)
                    socket.close()
                    return
                }
                val id = freeLinkId()
                if (id == null) {
                    socket.close()
                    return
                }
                val iface = cm?.getLinkProperties(network)?.interfaceName
                links[id] = Link(
                    linkId = id,
                    socket = socket,
                    relay = address,
                    port = port,
                    network = network,
                    label = label,
                    interfaceName = iface
                )
                Log.i(TAG, "link $id up: $label / $iface -> $host:$port")
                changed = true
            }
        }
        if (changed) onChanged(endpoints())
    }

    private fun drop(network: Network) {
        var removed = false
        synchronized(this) {
            val entry = links.entries.firstOrNull { it.value.networkHandle() == network }
            if (entry != null) {
                links.remove(entry.key)?.close()
                Log.i(TAG, "link ${entry.key} down")
                removed = true
            }
        }
        if (removed) onChanged(endpoints())
    }

    private fun freeLinkId(): Int? {
        for (id in 0 until 8) if (!links.containsKey(id)) return id
        return null
    }

    private fun labelFor(caps: NetworkCapabilities): String? = when {
        caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "Wi-Fi"
        caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "Mobile"
        caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "Ethernet"
        else -> null
    }

    companion object {
        private const val TAG = "TwinlinkLinks"
    }
}
