package io.zecurity.client

import android.app.KeyguardManager
import android.app.admin.DevicePolicyManager
import android.content.Context
import android.hardware.biometrics.BiometricManager
import android.os.Build
import org.json.JSONArray
import org.json.JSONObject

/**
 * The Android posture checks (ADR-031 D-4), read through public APIs with
 * no extra permission. No biometric data is ever read: only whether a
 * strong biometric is enrolled. IDs match controller/internal/posture.
 */
object AndroidPosture {
    const val ENCRYPTION = "android.disk_encryption.enabled"
    const val SCREEN_LOCK = "android.screen_lock.enabled"
    const val BIOMETRIC = "android.biometric.enrolled"

    fun collect(context: Context): String {
        val checks = JSONArray()
        fun add(id: String, status: String, detail: String) {
            checks.put(JSONObject().put("id", id).put("status", status).put("detail", detail))
        }

        try {
            val dpm = context.getSystemService(DevicePolicyManager::class.java)
            when (val s = dpm.storageEncryptionStatus) {
                DevicePolicyManager.ENCRYPTION_STATUS_ACTIVE ->
                    add(ENCRYPTION, "PASS", "storage encrypted")
                DevicePolicyManager.ENCRYPTION_STATUS_ACTIVE_PER_USER ->
                    add(ENCRYPTION, "PASS", "file-based encryption active")
                DevicePolicyManager.ENCRYPTION_STATUS_ACTIVE_DEFAULT_KEY ->
                    add(ENCRYPTION, "FAIL", "encrypted with the default key (no screen lock credential)")
                DevicePolicyManager.ENCRYPTION_STATUS_UNSUPPORTED ->
                    add(ENCRYPTION, "UNSUPPORTED", "this device does not support storage encryption")
                else -> add(ENCRYPTION, "FAIL", "storage not encrypted (status $s)")
            }
        } catch (e: Exception) {
            add(ENCRYPTION, "ERROR", e.javaClass.simpleName)
        }

        try {
            val km = context.getSystemService(KeyguardManager::class.java)
            if (km.isDeviceSecure) add(SCREEN_LOCK, "PASS", "PIN, pattern or password set")
            else add(SCREEN_LOCK, "FAIL", "no secure screen lock")
        } catch (e: Exception) {
            add(SCREEN_LOCK, "ERROR", e.javaClass.simpleName)
        }

        try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                val bm = context.getSystemService(BiometricManager::class.java)
                when (val r = bm.canAuthenticate(BiometricManager.Authenticators.BIOMETRIC_STRONG)) {
                    BiometricManager.BIOMETRIC_SUCCESS ->
                        add(BIOMETRIC, "PASS", "strong biometric enrolled")
                    BiometricManager.BIOMETRIC_ERROR_NONE_ENROLLED ->
                        add(BIOMETRIC, "FAIL", "no strong biometric enrolled")
                    BiometricManager.BIOMETRIC_ERROR_NO_HARDWARE ->
                        add(BIOMETRIC, "UNSUPPORTED", "no biometric hardware")
                    else -> add(BIOMETRIC, "FAIL", "strong biometric not available (code $r)")
                }
            } else {
                add(BIOMETRIC, "UNSUPPORTED", "needs Android 11 or later")
            }
        } catch (e: Exception) {
            add(BIOMETRIC, "ERROR", e.javaClass.simpleName)
        }

        return checks.toString()
    }
}
