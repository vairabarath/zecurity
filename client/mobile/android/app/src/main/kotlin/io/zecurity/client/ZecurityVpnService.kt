package io.zecurity.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.VpnService
import android.os.Build
import android.os.Process
import android.util.Log

/**
 * Hosts the client daemon (in the ":vpn" process, see the manifest) and
 * creates the VPN interface when the daemon brings the tunnel up.
 *
 * Only the allowed resource IPs are routed into the interface, one /32 each
 * (per-IP capture, as on Windows). The app itself is excluded from its VPN,
 * so the daemon's own QUIC and gRPC traffic never loops into the tunnel.
 */
class ZecurityVpnService : VpnService() {
    companion object {
        @Volatile
        var instance: ZecurityVpnService? = null
            private set

        private const val TAG = "ZecurityVpn"
        private const val CHANNEL_ID = "zecurity_client"
        private const val NOTIFICATION_ID = 1
        const val TUNNEL_ADDRESS = "100.64.0.1"
    }

    override fun onCreate() {
        super.onCreate()
        instance = this
        startInForeground()
        System.loadLibrary("zecurity_ffi")
        val rc = ZecurityNative.startDaemon(filesDir.absolutePath)
        if (rc != 0) Log.e(TAG, "startDaemon failed: $rc")
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int =
        // Not restarted automatically: a deliberate daemon exit (revoked
        // device) must not loop. The app starts the service when opened.
        START_NOT_STICKY

    override fun onDestroy() {
        instance = null
        super.onDestroy()
        // The daemon has no in-process stop path; end this (service-only)
        // process, like stopping the Linux systemd unit.
        Process.killProcess(Process.myPid())
    }

    override fun onRevoke() {
        // The user turned the VPN off in system settings, or another VPN app
        // took over: stop, as `systemctl stop` would.
        stopSelf()
    }

    /** Creates the VPN interface; returns its fd (owned by the daemon) or -1. */
    fun establishTunnel(routes: String): Int {
        return try {
            val builder = Builder()
                .setSession("Zecurity")
                .addAddress(TUNNEL_ADDRESS, 32)
                .setMtu(1500)
                .addDisallowedApplication(packageName)
            // Always at least one route, so an interface with no resources
            // is still valid.
            builder.addRoute(TUNNEL_ADDRESS, 32)
            routes.split(',').map { it.trim() }.filter { it.isNotEmpty() }.forEach {
                builder.addRoute(it, 32)
            }
            builder.establish()?.detachFd() ?: -1
        } catch (e: Exception) {
            Log.e(TAG, "establish failed", e)
            -1
        }
    }

    private fun startInForeground() {
        val manager = getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            manager.createNotificationChannel(
                NotificationChannel(CHANNEL_ID, "Zecurity", NotificationManager.IMPORTANCE_LOW)
            )
        }
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val notification = (if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O)
            Notification.Builder(this, CHANNEL_ID) else @Suppress("DEPRECATION") Notification.Builder(this))
            .setSmallIcon(android.R.drawable.ic_lock_lock)
            .setContentTitle("Zecurity")
            .setContentText("Zecurity client is running")
            .setContentIntent(open)
            .setOngoing(true)
            .build()
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }
}
