import Flutter
import NetworkExtension
import SafariServices
import UIKit
import ZecurityCore

/// The Flutter UI's iOS host: the same platform channel as Android's
/// MainActivity (files directory, starting the daemon, the VPN prompt, the
/// login page). The daemon runs in the packet tunnel extension
/// (ZecurityTunnel), which iOS runs only while the VPN is on, so starting
/// the "service" means starting the tunnel (with no resource routes until
/// the user connects).
@main
@objc class AppDelegate: FlutterAppDelegate, FlutterImplicitEngineDelegate {
  private static let channelName = "io.zecurity.client/platform"
  private static let appGroup = "group.io.zecurity.client"
  private static let tunnelBundleId = "io.zecurity.client.tunnel"

  private var browser: SFSafariViewController?

  override func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
  ) -> Bool {
    ZecurityCoreLink.keep()
    return super.application(application, didFinishLaunchingWithOptions: launchOptions)
  }

  func didInitializeImplicitFlutterEngine(_ engineBridge: FlutterImplicitEngineBridge) {
    GeneratedPluginRegistrant.register(with: engineBridge.pluginRegistry)
    guard let registrar = engineBridge.pluginRegistry.registrar(forPlugin: "ZecurityHost") else {
      return
    }
    let channel = FlutterMethodChannel(
      name: Self.channelName, binaryMessenger: registrar.messenger())
    channel.setMethodCallHandler { [weak self] call, result in
      self?.handle(call, result)
    }
  }

  private func handle(_ call: FlutterMethodCall, _ result: @escaping FlutterResult) {
    switch call.method {
    case "filesDir":
      guard let url = FileManager.default.containerURL(
        forSecurityApplicationGroupIdentifier: Self.appGroup)
      else {
        result(FlutterError(
          code: "no_app_group", message: "App Group \(Self.appGroup) is not configured",
          details: nil))
        return
      }
      result(url.path)
    case "startService":
      Tunnel.start { error in result(Self.reply(error)) }
    case "restartService":
      Tunnel.restart { error in result(Self.reply(error)) }
    case "prepareVpn":
      // Saving the configuration shows iOS's "Add VPN Configurations" prompt
      // the first time; granted once it is saved.
      Tunnel.manager { manager, _ in result(manager != nil) }
    case "openUrl":
      guard let text = (call.arguments as? [String: Any])?["url"] as? String,
        let url = URL(string: text)
      else {
        result(false)
        return
      }
      // In the app, not Safari: leaving the app would suspend it, and the
      // login callback goes to a loopback server in this process.
      let browser = SFSafariViewController(url: url)
      self.browser = browser
      Self.topViewController()?.present(browser, animated: true)
      result(true)
    case "closeBrowser":
      browser?.dismiss(animated: true)
      browser = nil
      result(true)
    default:
      result(FlutterMethodNotImplemented)
    }
  }

  private static func reply(_ error: Error?) -> Any {
    guard let error else { return true }
    return FlutterError(code: "tunnel", message: error.localizedDescription, details: nil)
  }

  private static func topViewController() -> UIViewController? {
    let scene = UIApplication.shared.connectedScenes
      .compactMap { $0 as? UIWindowScene }
      .first { $0.activationState == .foregroundActive }
    var top = scene?.windows.first { $0.isKeyWindow }?.rootViewController
    while let presented = top?.presentedViewController {
      top = presented
    }
    return top
  }

  /// The VPN configuration for the tunnel extension.
  private enum Tunnel {
    static func manager(_ done: @escaping (NETunnelProviderManager?, Error?) -> Void) {
      NETunnelProviderManager.loadAllFromPreferences { managers, error in
        if let error {
          done(nil, error)
          return
        }
        let manager = managers?.first ?? NETunnelProviderManager()
        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = AppDelegate.tunnelBundleId
        proto.serverAddress = "Zecurity"
        manager.protocolConfiguration = proto
        manager.localizedDescription = "Zecurity"
        manager.isEnabled = true
        manager.saveToPreferences { error in
          if let error {
            done(nil, error)
            return
          }
          manager.loadFromPreferences { error in done(error == nil ? manager : nil, error) }
        }
      }
    }

    static func start(_ done: @escaping (Error?) -> Void) {
      manager { manager, error in
        guard let manager else {
          done(error)
          return
        }
        switch manager.connection.status {
        case .connected, .connecting, .reasserting:
          done(nil)
        default:
          do {
            try manager.connection.startVPNTunnel()
            done(nil)
          } catch {
            done(error)
          }
        }
      }
    }

    /// Stops the tunnel (which ends the daemon) and starts it again, e.g.
    /// so the daemon reads a changed config.
    static func restart(_ done: @escaping (Error?) -> Void) {
      manager { manager, error in
        guard let manager else {
          done(error)
          return
        }
        manager.connection.stopVPNTunnel()
        waitUntilDisconnected(manager, attempts: 40) {
          do {
            try manager.connection.startVPNTunnel()
            done(nil)
          } catch {
            done(error)
          }
        }
      }
    }

    private static func waitUntilDisconnected(
      _ manager: NETunnelProviderManager, attempts: Int, then: @escaping () -> Void
    ) {
      let status = manager.connection.status
      if status == .disconnected || status == .invalid || attempts == 0 {
        then()
        return
      }
      DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) {
        waitUntilDisconnected(manager, attempts: attempts - 1, then: then)
      }
    }
  }
}

/// Dart reaches the core's zc_* functions with dlsym
/// (DynamicLibrary.process()); nothing else references them, so the static
/// library's objects would be dropped at link time. Referencing them here
/// keeps them (with Strip Style "Non-Global Symbols", see ios/README.md).
enum ZecurityCoreLink {
  static func keep() {
    let symbols: [Any] = [
      zc_init as Any, zc_config as Any, zc_setup as Any, zc_request as Any,
      zc_login_start as Any, zc_login_poll as Any, zc_logout as Any, zc_free as Any,
    ]
    precondition(symbols.count == 8)
  }
}
