# Zecurity iOS app: build on a Mac

The iOS app is the same Flutter UI as Android, plus a **packet tunnel
extension** (`ZecurityTunnel/`) that hosts the unchanged Rust client daemon.
Everything here was written on Linux and has **not yet been built or run**:
the first build on a Mac is also the first check of the Swift code and of the
iOS-only Rust code (`client/ffi` `ios_tunnel`).

How the pieces fit: `docs/client-multi-platform.md` section 13.

## What you need

- A Mac with Xcode 16 or later, Flutter stable, and Rust (`rustup`).
- A paid Apple Developer account (team).
- In the developer portal:
  - App IDs `io.zecurity.client` (app) and `io.zecurity.client.tunnel`
    (extension), both with the **Network Extensions** capability and the
    App Group **`group.io.zecurity.client`**.
  - Apple must grant the Network Extension entitlement to the team first,
    if it hasn't (request it early; it can take a while).
- A real iPhone (iOS 15+). The simulator does not run packet tunnel
  extensions reliably.

## 1. Build the Rust core

```bash
ROOT_PEM=/path/to/controller-root.pem client/mobile/scripts/build-ios-native.sh
# → client/mobile/ios/Frameworks/ZecurityCore.xcframework (git-ignored)
```

The root CA must be the controller's you will sign in to (see
`client/trust/README.md`).

## 2. One-time Xcode setup

```bash
cd client/mobile && flutter pub get && open ios/Runner.xcworkspace
```

**Runner target**

1. *Signing & Capabilities*: choose your team; bundle ID `io.zecurity.client`.
   Add **Network Extensions** (tick *Packet Tunnel*) and **App Groups**
   (`group.io.zecurity.client`). Set *Code Signing Entitlements* to
   `Runner/Runner.entitlements` (already in the repo).
2. *General → Frameworks, Libraries, and Embedded Content*: add
   `ios/Frameworks/ZecurityCore.xcframework`, **Do Not Embed** (it is a static
   library).
3. *Build Settings*: **Strip Style = Non-Global Symbols** (all
   configurations). The Flutter UI finds the `zc_*` functions with `dlsym`;
   `AppDelegate` references them so the linker keeps them, and this setting
   keeps them visible.

**Extension target**

1. *File → New → Target… → Network Extension*. Product name
   **ZecurityTunnel**, provider type **Packet Tunnel**, language Swift, embed
   in Runner. Bundle ID `io.zecurity.client.tunnel`.
2. Delete the files Xcode generated for it, and add the repo's
   `ios/ZecurityTunnel/PacketTunnelProvider.swift` to the ZecurityTunnel
   target. Set *Info.plist File* to `ZecurityTunnel/Info.plist` and *Code
   Signing Entitlements* to `ZecurityTunnel/ZecurityTunnel.entitlements`.
3. *Signing & Capabilities*: same team; **Network Extensions** (*Packet
   Tunnel*) and **App Groups** (`group.io.zecurity.client`).
4. *General → Frameworks and Libraries*: add `ZecurityCore.xcframework`,
   **Do Not Embed**.
5. *Build Settings*: iOS Deployment Target **15.0**, Swift Language Version
   5 (Swift 5.10+ toolchain).

Commit the resulting `project.pbxproj` changes so the next person skips this
section.

## 3. Run

```bash
cd client/mobile && flutter run --release   # with the iPhone connected
```

- First launch: iOS asks to **add a VPN configuration**. Allow it. The daemon
  can only run inside the tunnel extension, and iOS runs the extension only
  while the VPN is on, so the VPN icon shows from setup onwards, even while
  signed out. Resource traffic enters the tunnel only after **Connect**.
- Settings: the workspace and, for a dev controller, an address the phone can
  reach (a LAN IP, not `127.0.0.1`).
- **Sign in** opens the login page inside the app (not Safari: leaving the
  app would suspend it, and the login callback goes to a loopback server in
  the app). **Connect** adds the resource routes.

## Logs and debugging

- Daemon log: `daemon.log` in the App Group container; a start-up failure
  leaves `daemon.error` there too.
- Extension process: Xcode *Debug → Attach to Process → ZecurityTunnel*, or
  Console.app filtered on `ZecurityTunnel`.

## To verify on the first device run

- The app and the extension can talk over the Unix socket in the App Group
  container (`daemon.sock`). If the sandbox refuses it, the fallback is
  `NETunnelProviderSession.sendProviderMessage`.
- The extension's memory stays under iOS's limit (about 50 MB) with the
  tunnel up and traffic flowing (the daemon uses two worker threads on iOS;
  the data-plane buffers are the desktop sizes).
- The utun descriptor lookup (`find_utun_fd`) and the 4-byte header framing
  (`client/src/utun.rs`, unit-tested on Linux) work on the device.
- Sign-in through the in-app browser and the loopback callback.
