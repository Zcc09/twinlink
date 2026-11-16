package com.zcc09.twinlink.ui

import android.Manifest
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.PackageManager
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.LinearLayout
import android.widget.TextView
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import com.google.android.material.button.MaterialButton
import com.google.android.material.materialswitch.MaterialSwitch
import com.google.android.material.textfield.TextInputEditText
import com.zcc09.twinlink.BuildConfig
import com.zcc09.twinlink.R
import com.zcc09.twinlink.vpn.TunnelBus
import com.zcc09.twinlink.vpn.TwinlinkStats
import com.zcc09.twinlink.vpn.TwinlinkVpnService
import java.util.Locale

/**
 * One-screen control surface: master switch, relay settings, per-link live cards.
 * Plain Views + Material components only (no Compose), dark surface, 8 dp grid.
 */
class MainActivity : AppCompatActivity() {

    private lateinit var prefs: SharedPreferences

    private lateinit var switchConnect: MaterialSwitch
    private lateinit var statusValue: TextView
    private lateinit var statusDetail: TextView
    private lateinit var totalLine: TextView
    private lateinit var versionView: TextView
    private lateinit var inputHost: TextInputEditText
    private lateinit var inputPort: TextInputEditText
    private lateinit var inputSecret: TextInputEditText
    private lateinit var buttonSave: MaterialButton
    private lateinit var linkList: LinearLayout
    private lateinit var linksEmpty: TextView
    private lateinit var footnote: TextView

    private val rows = LinkedHashMap<Int, LinkRow>()
    private val handler = Handler(Looper.getMainLooper())
    private var ticker: Runnable? = null
    private var suppressSwitch = false
    private var pendingConnect = false

    private val consentLauncher =
        registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
            if (result.resultCode == RESULT_OK) {
                startTunnel()
            } else {
                pendingConnect = false
                setSwitchSilently(false)
                showStatusError(getString(R.string.note_consent_denied).removePrefix("Error: "))
            }
        }

    private val notificationPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { /* optional */ }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        prefs = getSharedPreferences(TwinlinkVpnService.PREFS, MODE_PRIVATE)

        switchConnect = findViewById(R.id.switch_connect)
        statusValue = findViewById(R.id.status_value)
        statusDetail = findViewById(R.id.status_detail)
        totalLine = findViewById(R.id.total_line)
        versionView = findViewById(R.id.version)
        inputHost = findViewById(R.id.input_host)
        inputPort = findViewById(R.id.input_port)
        inputSecret = findViewById(R.id.input_secret)
        buttonSave = findViewById(R.id.button_save)
        linkList = findViewById(R.id.link_list)
        linksEmpty = findViewById(R.id.links_empty)
        footnote = findViewById(R.id.footnote)

        // On a tablet the content column would otherwise stretch a single-line
        // text field past 2500 px. Cap it at a comfortable reading width and
        // leave the rest of the window as plain surface.
        val content = findViewById<LinearLayout>(R.id.content)
        content.post {
            val maxPx = (840 * resources.displayMetrics.density).toInt()
            if (content.width > maxPx) {
                content.layoutParams = (content.layoutParams as ViewGroup.LayoutParams).apply {
                    width = maxPx
                }
            }
        }

        versionView.text = getString(R.string.version_line, BuildConfig.VERSION_NAME)
        loadSettings()

        switchConnect.setOnCheckedChangeListener { _, checked ->
            if (suppressSwitch) return@setOnCheckedChangeListener
            if (checked) onConnectRequested() else onDisconnectRequested()
        }
        buttonSave.setOnClickListener {
            if (saveSettings()) {
                buttonSave.text = getString(R.string.saved)
                handler.postDelayed({ buttonSave.text = getString(R.string.action_apply) }, 1200)
            }
        }

        requestNotificationPermissionIfNeeded()
    }

    override fun onResume() {
        super.onResume()
        val runnable = object : Runnable {
            override fun run() {
                refresh()
                handler.postDelayed(this, 1000L)
            }
        }
        ticker = runnable
        handler.post(runnable)
    }

    override fun onPause() {
        super.onPause()
        ticker?.let { handler.removeCallbacks(it) }
        ticker = null
    }

    // ---------------------------------------------------------------- settings

    private fun loadSettings() {
        inputHost.setText(prefs.getString(TwinlinkVpnService.KEY_HOST, ""))
        inputPort.setText(prefs.getInt(TwinlinkVpnService.KEY_PORT, TwinlinkVpnService.DEFAULT_PORT).toString())
        inputSecret.setText(prefs.getString(TwinlinkVpnService.KEY_SECRET, ""))
    }

    private fun saveSettings(): Boolean {
        val host = inputHost.text?.toString()?.trim().orEmpty()
        if (host.isEmpty()) {
            showStatusError(getString(R.string.error_host_empty).removePrefix("Error: "))
            return false
        }
        val port = inputPort.text?.toString()?.trim()?.toIntOrNull() ?: TwinlinkVpnService.DEFAULT_PORT
        val secret = inputSecret.text?.toString().orEmpty()
        if (secret.length < 16) {
            showStatusError(getString(R.string.error_secret_short).removePrefix("Error: "))
            return false
        }
        prefs.edit()
            .putString(TwinlinkVpnService.KEY_HOST, host)
            .putInt(TwinlinkVpnService.KEY_PORT, if (port in 1..65535) port else TwinlinkVpnService.DEFAULT_PORT)
            .putString(TwinlinkVpnService.KEY_SECRET, secret)
            .apply()
        return true
    }

    // ---------------------------------------------------------------- connect / disconnect

    private fun onConnectRequested() {
        if (!saveSettings()) {
            setSwitchSilently(false)
            return
        }
        pendingConnect = true
        val consent = VpnService.prepare(this)
        if (consent != null) {
            consentLauncher.launch(consent)
        } else {
            startTunnel()
        }
    }

    private fun startTunnel() {
        if (!saveSettings()) {
            pendingConnect = false
            setSwitchSilently(false)
            return
        }
        val intent = Intent(this, TwinlinkVpnService::class.java)
            .setAction(TwinlinkVpnService.ACTION_START)
            .putExtra(TwinlinkVpnService.EXTRA_HOST, inputHost.text?.toString()?.trim())
            .putExtra(TwinlinkVpnService.EXTRA_PORT, inputPort.text?.toString()?.trim()?.toIntOrNull() ?: TwinlinkVpnService.DEFAULT_PORT)
            .putExtra(TwinlinkVpnService.EXTRA_SECRET, inputSecret.text?.toString())
        ContextCompat.startForegroundService(this, intent)
        pendingConnect = false
        statusValue.setTextColor(ContextCompat.getColor(this, R.color.text))
        statusValue.text = getString(R.string.status_connecting)
        statusDetail.text = getString(R.string.waiting_relay)
    }

    private fun onDisconnectRequested() {
        startService(Intent(this, TwinlinkVpnService::class.java).setAction(TwinlinkVpnService.ACTION_STOP))
        handler.postDelayed({ refresh() }, 300L)
    }

    private fun requestNotificationPermissionIfNeeded() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        val granted = ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) ==
            PackageManager.PERMISSION_GRANTED
        if (!granted) {
            notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    // ---------------------------------------------------------------- rendering

    private fun refresh() {
        val snapshot = TunnelBus.snapshot()
        val running = TunnelBus.serviceRunning || snapshot.status != TwinlinkStats.Status.DISCONNECTED
        if (!pendingConnect) setSwitchSilently(running)

        when (snapshot.status) {
            TwinlinkStats.Status.DISCONNECTED -> {
                statusValue.setTextColor(ContextCompat.getColor(this, R.color.muted))
                statusValue.text = getString(R.string.status_disconnected)
                statusDetail.text = getString(R.string.tunnel_off)
            }

            TwinlinkStats.Status.CONNECTING -> {
                statusValue.setTextColor(ContextCompat.getColor(this, R.color.accent))
                statusValue.text = getString(R.string.status_connecting)
                statusDetail.text = getString(R.string.waiting_relay)
            }

            TwinlinkStats.Status.CONNECTED -> {
                statusValue.setTextColor(ContextCompat.getColor(this, R.color.teal))
                statusValue.text = getString(R.string.status_connected)
                statusDetail.text = getString(
                    R.string.session_line,
                    String.format(Locale.US, "%04x", snapshot.session and 0xFFFF),
                    snapshot.mtu,
                    snapshot.links.size
                )
            }

            TwinlinkStats.Status.ERROR -> {
                statusValue.setTextColor(ContextCompat.getColor(this, R.color.warn))
                statusValue.text = getString(R.string.status_error, snapshot.error ?: "unknown")
                statusDetail.text = getString(R.string.tunnel_off)
            }
        }

        totalLine.text = getString(
            R.string.rate_pair,
            formatRate(snapshot.totalRxBps),
            formatRate(snapshot.totalTxBps)
        )

        renderLinks(snapshot.links)
        footnote.visibility = View.VISIBLE
    }

    private fun renderLinks(links: List<TwinlinkStats.LinkSnapshot>) {
        val ids = links.map { it.id }
        if (ids != rows.keys.toList()) {
            linkList.removeAllViews()
            rows.clear()
            val inflater = LayoutInflater.from(this)
            for (link in links) {
                val view = inflater.inflate(R.layout.item_link, linkList, false)
                val row = LinkRow(
                    bar = view.findViewById(R.id.link_bar),
                    label = view.findViewById(R.id.link_label),
                    detail = view.findViewById(R.id.link_detail),
                    rx = view.findViewById(R.id.link_rx),
                    tx = view.findViewById(R.id.link_tx)
                )
                linkList.addView(view)
                rows[link.id] = row
            }
        }
        for (link in links) {
            val row = rows[link.id] ?: continue
            row.label.text = link.label
            val iface = link.iface ?: "link ${link.id}"
            row.detail.text = getString(R.string.link_detail, iface, getString(R.string.rtt_format, formatRtt(link.rttUs)))
            row.rx.text = "↓ " + formatRate(link.rxBps)
            row.tx.text = "↑ " + formatRate(link.txBps)
            row.bar.setBackgroundColor(
                ContextCompat.getColor(this, if (link.alive) R.color.teal else R.color.line)
            )
            row.label.setTextColor(
                ContextCompat.getColor(this, if (link.alive) R.color.text else R.color.muted)
            )
        }
        linksEmpty.visibility = if (links.isEmpty()) View.VISIBLE else View.GONE
    }

    private fun showStatusError(message: String) {
        statusValue.setTextColor(ContextCompat.getColor(this, R.color.warn))
        statusValue.text = getString(R.string.status_error, message)
        statusDetail.text = getString(R.string.tunnel_off)
    }

    private fun setSwitchSilently(checked: Boolean) {
        if (switchConnect.isChecked == checked) return
        suppressSwitch = true
        switchConnect.isChecked = checked
        suppressSwitch = false
    }

    private fun formatRate(bps: Long): String = when {
        bps >= 1_000_000L -> String.format(Locale.US, "%.1f Mbps", bps / 1_000_000.0)
        bps >= 1_000L -> String.format(Locale.US, "%d kbps", bps / 1_000L)
        else -> String.format(Locale.US, "%d bps", bps)
    }

    private fun formatRtt(us: Long): String =
        if (us < 0) getString(R.string.unknown) else String.format(Locale.US, "%d ms", us / 1000L)

    private class LinkRow(
        val bar: View,
        val label: TextView,
        val detail: TextView,
        val rx: TextView,
        val tx: TextView
    )
}
