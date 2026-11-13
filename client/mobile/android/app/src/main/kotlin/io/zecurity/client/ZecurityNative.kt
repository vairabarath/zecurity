package io.zecurity.client

/**
 * JNI bridge to libzecurity_ffi.so (client/ffi) in the VPN service process.
 */
object ZecurityNative {
    /** Starts the client daemon in this process (once). 0 on success. */
    @JvmStatic
    external fun startDaemon(filesDir: String): Int

    /**
     * Called by the daemon (on its own thread) when it brings the tunnel up:
     * creates the VPN interface routing exactly `routes` ("ip,ip,...", one
     * /32 each) and returns its file descriptor, or -1.
     */
    @JvmStatic
    fun establish(routes: String): Int =
        ZecurityVpnService.instance?.establishTunnel(routes) ?: -1

    /**
     * Called by the daemon for each posture report (ADR-031): the Android
     * checks as a JSON array of {id, status, detail}; "[]" before the
     * service is up.
     */
    @JvmStatic
    fun posture(): String =
        ZecurityVpnService.instance?.let { AndroidPosture.collect(it) } ?: "[]"
}
