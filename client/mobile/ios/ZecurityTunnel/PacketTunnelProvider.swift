import Foundation
import NetworkExtension
import ZecurityCore

/// The Zecurity packet tunnel extension. It hosts the unchanged client daemon
/// (the iOS counterpart of the Android VpnService process): the daemon starts
/// with the tunnel and asks for routes through the callbacks below when it
/// brings the Zecurity tunnel up or down.
///
/// The tunnel starts with no resource routes, only so the daemon runs (iOS
/// runs an extension only while its VPN is on); resource routes, one /32
/// each, are added when the user connects. The extension's own traffic, and
/// so the daemon's QUIC and gRPC connections, never enters the tunnel.
final class PacketTunnelProvider: NEPacketTunnelProvider {
    static let appGroup = "group.io.zecurity.client"
    private static let tunnelAddress = "100.64.0.1"

    /// The running provider, for the C callbacks (they cannot capture).
    nonisolated(unsafe) static weak var current: PacketTunnelProvider?

    override func startTunnel(
        options: [String: NSObject]?,
        completionHandler: @escaping (Error?) -> Void
    ) {
        PacketTunnelProvider.current = self
        guard let dir = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: Self.appGroup
        )?.path else {
            completionHandler(Self.error(1, "App Group \(Self.appGroup) is not configured"))
            return
        }
        setTunnelNetworkSettings(Self.settings(routes: [])) { error in
            if let error {
                completionHandler(error)
                return
            }
            let v = ProcessInfo.processInfo.operatingSystemVersion
            let version = "\(v.majorVersion).\(v.minorVersion).\(v.patchVersion)"
            let rc = zc_start_daemon(dir, version, zecurityEstablish, zecurityRelease)
            completionHandler(rc == 0 ? nil : Self.error(Int(rc), "the client daemon did not start"))
        }
    }

    override func stopTunnel(
        with reason: NEProviderStopReason,
        completionHandler: @escaping () -> Void
    ) {
        completionHandler()
        // The daemon has no in-process stop path; end the extension's
        // process, as stopping the Linux service does.
        exit(0)
    }

    /// Applies settings routing exactly `routes`; 0 once applied. Called on a
    /// daemon thread, so it may block (bounded, so a stuck call fails `Up`).
    func apply(routes: [String]) -> Int32 {
        let done = DispatchSemaphore(value: 0)
        let outcome = Outcome()
        setTunnelNetworkSettings(Self.settings(routes: routes)) { error in
            outcome.failed = error != nil
            done.signal()
        }
        if done.wait(timeout: .now() + 10) == .timedOut {
            return -1
        }
        return outcome.failed ? -1 : 0
    }

    private final class Outcome: @unchecked Sendable {
        var failed = true
    }

    private static func settings(routes: [String]) -> NEPacketTunnelNetworkSettings {
        let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
        let ipv4 = NEIPv4Settings(addresses: [tunnelAddress], subnetMasks: ["255.255.255.255"])
        ipv4.includedRoutes = ([tunnelAddress] + routes).map {
            NEIPv4Route(destinationAddress: $0, subnetMask: "255.255.255.255")
        }
        settings.ipv4Settings = ipv4
        settings.mtu = 1500
        return settings
    }

    private static func error(_ code: Int, _ message: String) -> NSError {
        NSError(domain: "io.zecurity.client.tunnel", code: code,
                userInfo: [NSLocalizedDescriptionKey: message])
    }
}

// The daemon's callbacks (zc_start_daemon). C function pointers cannot
// capture, so they reach the provider through `current`.

private let zecurityEstablish: zc_establish_fn = { routes in
    guard let provider = PacketTunnelProvider.current else { return -1 }
    let list = routes.map { String(cString: $0) } ?? ""
    return provider.apply(routes: list.split(separator: ",").map(String.init))
}

private let zecurityRelease: zc_release_fn = {
    _ = PacketTunnelProvider.current?.apply(routes: [])
}
