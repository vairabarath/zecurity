package io.zecurity.client

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.net.VpnService
import android.os.Build
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel

/**
 * The Flutter UI, plus the platform calls it needs: the files directory
 * (shared with the daemon), starting and restarting the daemon's service,
 * the VPN permission prompt, and opening the login page.
 */
class MainActivity : FlutterActivity() {
    companion object {
        private const val CHANNEL = "io.zecurity.client/platform"
        private const val VPN_REQUEST = 41
    }

    private var pendingVpnResult: MethodChannel.Result? = null

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        MethodChannel(flutterEngine.dartExecutor.binaryMessenger, CHANNEL).setMethodCallHandler { call, result ->
            when (call.method) {
                "filesDir" -> result.success(filesDir.absolutePath)
                "startService" -> {
                    askForNotifications()
                    startDaemonService()
                    result.success(true)
                }
                "restartService" -> {
                    stopService(Intent(this, ZecurityVpnService::class.java))
                    // The service process ends in onDestroy; give it a moment.
                    window.decorView.postDelayed({ startDaemonService() }, 800)
                    result.success(true)
                }
                "prepareVpn" -> {
                    val consent = VpnService.prepare(this)
                    if (consent == null) {
                        result.success(true)
                    } else {
                        pendingVpnResult = result
                        @Suppress("DEPRECATION")
                        startActivityForResult(consent, VPN_REQUEST)
                    }
                }
                "openUrl" -> {
                    val url = call.argument<String>("url")
                    if (url == null) {
                        result.success(false)
                    } else {
                        startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)))
                        result.success(true)
                    }
                }
                // The login page opens in the browser app on Android; nothing to close.
                "closeBrowser" -> result.success(true)
                else -> result.notImplemented()
            }
        }
    }

    private fun startDaemonService() {
        val intent = Intent(this, ZecurityVpnService::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) startForegroundService(intent) else startService(intent)
    }

    private fun askForNotifications() {
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 42)
        }
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == VPN_REQUEST) {
            pendingVpnResult?.success(resultCode == RESULT_OK)
            pendingVpnResult = null
        }
    }
}
