# Client multi-platform conversion note

> Status: planning note, 2026-11-12. Not a decision record. The items under
> "Decisions needed" must go into a decision record before any phase starts.

Goal: take `zecurity-client` (Rust, Linux only today) to **Windows first, then
Android, then iOS**, without forking the protocol or the controller contract.

## 1. Where the client stands today

What the Linux client is (from `client/src`):

| Layer | Modules | Portable? |
|---|---|---|
| Data plane: userspace TCP/IP, flow proxying, DNS answerer | `net_stack.rs` (smoltcp), `dns.rs`, `name_map.rs` | Yes, except the `tun::AsyncDevice` coupling in `net_stack::TunDevice` |
| Outer transport: QUIC + mTLS to connectors and relays | `tunnel_pool.rs`, `relay_pool.rs`, `transport.rs`, `stream_watch.rs` | Yes (quinn, rustls with `ring`) |
| Trust, revocation, cert renewal | `trust.rs`, `crl.rs`, `build.rs` (embedded root), renewal in `daemon.rs` | Yes |
| Control plane: gRPC, ACL/transport snapshots, change watch | `grpc.rs`, `change_watch.rs`, most of `daemon.rs` | Yes |
| Login: OAuth/PKCE, loopback callback, enrolment CSR | `login.rs`, `auth.rs` | Desktop yes; mobile no (loopback HTTP server, `open::that`) |
| Durable state: AES-GCM envelope, per-workspace key file | `state_store.rs` | Yes, except `from_mode(0o600)` and the key-file location/protection |
| Kernel wiring: TUN, routes, split tunnelling | `tun.rs` (tun crate, rtnetlink, `nft`, `ip rule`, `ip route`, table 105, fwmark 0x5a) | **Linux only** (ADR-003, ADR-009) |
| DNS-name resources wiring | `dns_link.rs` (`resolvectl`, polkit rule) | **Linux only** |
| Daemon hosting and IPC | `ipc.rs` (Unix socket `/run/zecurity-client/daemon.sock`, `SO_PEERCRED`, `systemctl start`), `sd_notify`/watchdog in `daemon.rs`, `systemd/` | **Linux only** |
| Posture collectors | `posture.rs` (`linux.*` check IDs, `/etc/os-release`, `/etc/crypttab`, EFI vars, `nft`/`iptables`/`ufw`/`systemctl`) | **Linux only** by design |
| Hardware key | `tpm.rs` (tss-esapi, `/dev/tpmrm0`) | Linux only, already `cfg`-gated |
| Config | `config.rs` (`/etc/zecurity/client.conf` + XDG user config) | Paths Linux-shaped |
| Packaging | `scripts/client-install.sh`, `Cross.toml`, `.github/workflows/client-release.yml` (musl x86_64/aarch64) | Linux only |

Roughly two thirds of the crate is platform-neutral. The platform-bound part
is concentrated in six seams: packet I/O, routing/DNS wiring, IPC, service
hosting, posture, and key storage. That is the whole job: put a trait in front
of each seam, keep Linux byte-for-byte identical behind it, then add Windows,
Android and iOS implementations.

Controller-side facts that matter:

- Posture check IDs are a fixed set in `controller/internal/posture/validate.go`
  (`linux.os.version`, `linux.disk_encryption.luks`, `linux.firewall.active`,
  `linux.secure_boot.enabled`). Unknown IDs are dropped silently. A profile
  requirement whose check the device did not report evaluates as
  `unsupported`, which fails unless `allow_unsupported` is set.
- `EnrollDevice` carries `os` (the client sends the literal `"linux"`) and
  posture reports carry `os_name`/`os_version`.
- Nothing else on the controller, connector, relay or proto is OS-specific.
  Proto changes stay additive.

## 2. Phase C0: split the crate before touching any new OS

Do this once, on Linux, with no behaviour change. It is the prerequisite for
every platform and should ship as its own PR with the existing test suite and
a live run as the gate.

1. **Workspace layout**
   - `client/core/` → crate `zecurity-core` (lib): everything portable from
     the table above plus the platform traits.
   - `client/cli/` → crate `zecurity-client` (bin): Linux and Windows daemon +
     CLI, platform implementations under `src/platform/{linux,windows}/`.
   - `client/mobile/` (Phase M0) → crate `zecurity-mobile` (cdylib/staticlib)
     with UniFFI bindings for Android/iOS.
2. **Traits in `zecurity-core`** (names are suggestions):
   - `PacketIo`: `async fn read(&mut self, buf) -> usize` / `async fn write(&mut self, pkt)`.
     `net_stack::TunDevice` takes `Box<dyn PacketIo>` instead of
     `tun::AsyncDevice`. Linux impl wraps the existing device.
   - `NetConfig`: `bring_up(addr)`, `apply_desired(flows: &BTreeSet<AllowedFlow>)`,
     `add_routes`/`del_routes`, `configure_dns(names, server)`, `tear_down()`.
     The existing `ResourceKernel` trait and `dns_link::DnsLinkState` fold into
     this. Linux impl is the current `tun.rs` + `dns_link.rs` code unchanged.
   - `KeyBackend`: `generate() -> (SigningKey, StoredKeyMaterial)` and
     `load(material) -> SigningKey`. The `rcgen::RemoteKeyPair` and
     `rustls::sign::SigningKey` seam already used by `tpm.rs` is the contract.
     Software keys and the Linux TPM are the two existing impls.
   - `PostureCollector`: `async fn collect() -> PostureReport`. The
     `CommandRunner` and `root: &Path` seams already exist; the Linux
     collectors move behind it as-is.
   - `StateProtection`: how the per-workspace state key is protected at rest
     (Linux: 0600 file, unchanged). Windows/mobile plug in DPAPI/Keystore/Keychain.
   - `Ipc` (desktop only): `listen()`, `connect()`, `peer_is_same_user()`, and
     `ServiceHost`: `notify_ready()`, `watchdog()`, `start_service()`.
3. **Login split**: `login::run` becomes `begin(conf) -> (auth_url, pending)`
   and `complete(pending, code, state) -> LoginResult`. The loopback `axum`
   callback becomes one caller of `complete`; mobile deep links become another.
   The enrolment CSR path is untouched.
4. **Constants**: `"linux"` in `login.rs` and the `os` field become a
   `platform::os_id()` (`linux`, `windows`, `android`, `ios`). Check-ID
   prefixes follow the same string.
5. **CI**: `cargo build -p zecurity-core --target x86_64-pc-windows-msvc` and
   `--target aarch64-linux-android` (lib only, no bin) are added as build-only
   jobs now, so the core cannot regrow Linux dependencies.

Gate: Linux `cargo test`, the existing live journeys, identical `nft`/`ip`
command sequences in the `tun.rs` unit tests.

## 3. Windows (phases W1 to W3)

### W1: service, Wintun, IPC, routes

- **Packet I/O**: Wintun (`wintun.dll`, redistributable, signed by WireGuard
  LLC). Either the `tun` crate's Windows backend (check the pinned version
  supports it) or the `wintun` crate directly. Adapter name `zecurity0`,
  address `100.64.0.1/32` set through IP Helper
  (`CreateUnicastIpAddressEntry`) via the `windows` crate.
- **Daemon hosting**: a Windows Service (`windows-service` crate) running as
  `LocalSystem`. Creating a Wintun adapter and routes needs administrator
  rights, and there is no `CAP_NET_ADMIN` equivalent to delegate them to the
  user. `sd_notify`/watchdog map to SCM `Running` status and a service
  heartbeat; `Restart=on-failure` maps to the service recovery actions.
- **IPC**: named pipe `\\.\pipe\zecurity-client` via
  `tokio::net::windows::named_pipe`. Same newline-delimited JSON protocol
  (ADR-002), so the CLI commands do not change. The same-user check becomes:
  get the client PID (`GetNamedPipeClientProcessId`), open its token, and
  compare its user SID with the interactive session that enrolled, or restrict
  the pipe's security descriptor to the enrolling user's SID plus SYSTEM.
  `ensure_daemon_and_send` calls `StartService` instead of `systemctl`.
- **Routes**: one `/32` route per resource IP on the Wintun adapter via
  `CreateIpForwardEntry2`, with a low metric. This is ADR-003 behaviour, not
  ADR-009: Windows has no cheap equivalent of the nftables fwmark + table 105
  port filter. Port-level split tunnelling on Windows needs a WFP (Windows
  Filtering Platform) callout driver, which is a signed kernel driver. See
  Decisions needed, D-W1.
- **Config and state**: `C:\ProgramData\Zecurity\client.conf` (system) and the
  state directory `C:\ProgramData\Zecurity\state\` owned by SYSTEM, since the
  service, not the user, holds the device identity. `dirs::data_local_dir`
  stays for the CLI-only user config. Replace `from_mode(0o600)` with an ACL
  (SYSTEM + Administrators) and wrap the state key with DPAPI machine scope
  (`CryptProtectData` with `CRYPTPROTECT_LOCAL_MACHINE`).
- **Login**: unchanged. The loopback callback and `open::that` work on
  Windows. The CLI runs the browser flow in the user session and hands
  `PostLoginState` to the service over the pipe, exactly as on Linux.
- **Trust**: the embedded platform root works unchanged. `build.rs` runs on
  the Windows runner as-is.

Gate: `zecurity-client login`, `up`, reach an IP resource, `down`, reboot
restore (FU-10), cert renewal, revocation reaction (D-8). Run on Windows 10
22H2 and Windows 11.

### W2: DNS-name resources and posture

- **DNS**: the Linux equivalent of `resolvectl domain zecurity0 ~name` is an
  NRPT rule (Name Resolution Policy Table): for each resource name, a rule
  sending that exact name to `198.19.255.53`, plus a `/32` route to that
  address on the adapter. Write rules under
  `HKLM\SYSTEM\CurrentControlSet\Services\Dnscache\Parameters\DnsPolicyConfig`
  (or `Add-DnsClientNrptRule`), and remove them on `down` and on service
  start (stale-rule cleanup, same idea as `cleanup_stale_interface`). The
  smoltcp DNS answerer (`dns.rs`) is reused unchanged.
- **Posture** (`windows.*` IDs, added to `recognizedChecks` in the controller):
  - `windows.os.version`: `RtlGetVersion` or the `CurrentVersion` registry key.
  - `windows.disk_encryption.bitlocker`: WMI `Win32_EncryptableVolume`
    (`ProtectionStatus` of the system drive); `manage-bde -status` as the
    command fallback through the existing `CommandRunner` seam.
  - `windows.firewall.active`: `INetFwPolicy2.FirewallEnabled` for the
    current profile via COM, or `netsh advfirewall show currentprofile`.
  - `windows.secure_boot.enabled`: registry
    `HKLM\SYSTEM\CurrentControlSet\Control\SecureBoot\State\UEFISecureBootEnabled`.
- **Admin UI**: the "Trusted Linux profile" page (Sprint 15) becomes
  per-OS. A profile can hold requirements for several OS prefixes; a device
  only reports its own prefix, so the other prefixes need `allow_unsupported`
  or the evaluator needs an "only requirements for the reporting OS" rule.
  See D-W3.

### W3: hardware key and packaging

- **Key backend**: CNG Platform Crypto Provider
  (`NCryptOpenStorageProvider(MS_PLATFORM_CRYPTO_PROVIDER)`), ECDSA P-256,
  non-exportable, `NCryptSignHash` behind the `KeyBackend` seam. The
  `ZECURITY_TPM_KEYS=1` fail-closed rule applies unchanged. Key material
  stored is the CNG key name, not a blob.
- **Packaging**: MSI via WiX (installs binary + `wintun.dll` next to it,
  registers the service, writes `client.conf` from MSI properties
  `CONTROLLER_ADDR`/`CONTROLLER_HTTP_ADDR`). Authenticode signing is
  mandatory: unsigned services and `wintun.dll` loading from an unsigned
  location trip SmartScreen and some EDRs. CI: `windows-latest` runner,
  target `x86_64-pc-windows-msvc` (add `aarch64-pc-windows-msvc` later),
  `protoc` via `arduino/setup-protoc`, asset name `client-windows-amd64.msi`
  in the same release workflow and `checksums.txt`.
- **Install docs**: a Windows section next to `client-install.sh` with the
  `msiexec` one-liner and the `zecurity-client setup --workspace` step.

## 4. Mobile prerequisites (phase M0)

Mobile cannot reuse the daemon/CLI shape at all. There is no daemon, no CLI,
no external commands, and the OS owns the TUN device. What survives is
`zecurity-core` plus the traits from C0. M0 is done once for both platforms:

- `zecurity-mobile` crate: `cdylib` (Android) and `staticlib` (iOS), exposing
  through **UniFFI**:
  - `Client::new(config, callbacks)`; `begin_login() -> url`;
    `complete_login(code, state)`; `status()`; `resources()`; `sync()`;
    `logout()`.
  - `start_tunnel(packet_io: Box<dyn PacketIo>) -> TunnelHandle` and
    `stop_tunnel()`. The host supplies packet I/O; the core supplies the
    desired route/DNS set through a callback (`on_network_config(routes,
    dns_names, dns_server)`), since the host, not the core, programs routes
    on mobile.
  - Callbacks for `KeyBackend` (sign-with-platform-key), `StateProtection`
    (seal/unseal the state key) and posture facts the host must gather.
- **Login completion** uses a deep link (`https://<controller>/client/callback`
  app link or a custom scheme) handled by the host app, which calls
  `complete_login`. The controller's allowed `redirect_uri` set gains that
  value; nothing else changes server-side.
- **Scheduling**: the refresh, ACL sync, posture and renewal schedulers from
  `daemon.rs` run inside the tunnel process while the tunnel is up. Mobile
  OSes stop background work otherwise, so "daemon always up" is not a
  guarantee on mobile; `tunnel_wanted` maps to the OS's always-on VPN setting.
- **Memory budget**: `net_stack.rs` sizes buffers for a desktop (64 KiB TCP
  buffers per socket, 1 MiB per-flow write buffer, SYN defer 1024). These
  need to be constructor parameters so the iOS extension (hard 50 MB limit)
  and Android can run with smaller values.
- **Connector/relay sockets**: QUIC endpoints must never be routed into the
  tunnel. With `/32` resource routes only this cannot loop, but Android's
  `VpnService.protect(fd)` is still applied to every UDP socket quinn opens
  (a socket-creation hook on the endpoint) as defence in depth.

## 5. Android (phases A1 and A2)

### A1: VpnService + tunnel

- Kotlin app with a foreground `VpnService`. `Builder.establish()` returns the
  TUN fd; `ParcelFileDescriptor.detachFd()` is passed to Rust and wrapped in
  `tokio::io::unix::AsyncFd` as the `PacketIo` impl.
- Routes: `Builder.addAddress("100.64.0.1", 32)` and one `addRoute(ip, 32)`
  per resource IP. A route set change requires re-establishing the interface,
  so the hot-apply path (Phase 5-C resource delta) becomes "re-establish with
  the new set, keep flows alive where the kernel allows". Session hand-over
  is the main engineering risk here.
- Port-level split is not available; IP-level, as on Windows (D-W1 covers
  both).
- DNS-name resources: `VpnService` has no per-domain DNS routing. Options are
  in D-A1. Recommended default for A1: IP resources only; names in A2.
- Login: Chrome Custom Tabs + app link; the activity calls `complete_login`.
- State: app-private storage; the state key is wrapped by an Android Keystore
  AES key (`StateProtection`). Device identity is per-app-install, which is
  the mobile norm.
- Build: `cargo-ndk`, targets `aarch64-linux-android` and
  `x86_64-linux-android` (emulator), NDK r26+. `ring`, `quinn`, `tonic`,
  `smoltcp` build for Android; `tun`, `rtnetlink`, `tss-esapi`, `axum` must be
  absent from `zecurity-core`'s default features (C0 guarantees this).
- Distribution: Play Store (VPN apps need the VpnService declaration and a
  policy justification) or MDM/managed Play for enterprise.

### A2: hardware key, posture, DNS names

- Key backend: Android Keystore EC P-256, `setIsStrongBoxBacked(true)` where
  available, signing via callback. Same fail-closed rule as TPM mode.
- Posture (`android.*`): `android.os.version` (`Build.VERSION.RELEASE`,
  security patch level), `android.screen_lock.enabled`
  (`KeyguardManager.isDeviceSecure`), `android.disk_encryption` (file-based
  encryption is mandatory since Android 10; report pass with the API level
  as detail), `android.play_integrity` (optional, needs a server-side
  verifier and is a separate decision).
- DNS names per D-A1.

## 6. iOS (phases I1 and I2)

### I1: Packet Tunnel extension

- Entitlement first: `com.apple.developer.networking.networkextension` with
  `packet-tunnel-provider` must be requested from Apple for the app ID.
  Allow weeks for this; it blocks everything else.
- Structure: host app (Swift) + `NEPacketTunnelProvider` app extension.
  Rust ships as an XCFramework built from `aarch64-apple-ios` and
  `aarch64-apple-ios-sim` with UniFFI Swift bindings.
- Packet I/O: `packetFlow.readPackets` / `writePackets` are callback-based,
  not an fd. The Swift side pumps packets into/out of channels; the Rust
  `PacketIo` impl reads from those channels.
- Routes and DNS: `NEPacketTunnelNetworkSettings` with
  `IPv4Settings.includedRoutes` = one `/32` per resource IP, and
  `NEDNSSettings(servers: ["198.19.255.53"])` with `matchDomains` = the
  resource names. `matchDomains` is the exact equivalent of
  `resolvectl domain ~name`, so DNS-name resources map cleanly on iOS, unlike
  Android. Changing the set means calling `setTunnelNetworkSettings` again,
  which drops the data path briefly; the same hand-over concern as Android.
- Login: `ASWebAuthenticationSession` in the host app with a custom scheme
  or universal link; `complete_login` runs in the host app, and the result is
  handed to the extension through an App Group container plus a shared
  Keychain access group (`StateProtection` = Keychain item wrapping the
  state key).
- Memory: the extension's 50 MB limit is the binding constraint. Set the
  smoltcp buffer parameters from M0 small (for example 16 KiB TCP buffers,
  64 KiB flow buffer) and cap concurrent flows.
- Testing: packet tunnel providers do not run reliably in the simulator;
  plan for physical devices and TestFlight from the first build.

### I2: Secure Enclave and posture

- Key backend: Secure Enclave via `SecKeyCreateRandomKey` with
  `kSecAttrTokenIDSecureEnclave` (P-256 only, which matches the TPM
  decision). Signing via callback from the extension process; the key must
  be created with an access group both targets share.
- Posture (`ios.*`): `ios.os.version`, `ios.passcode.enabled`
  (`LAContext.canEvaluatePolicy(.deviceOwnerAuthentication)`),
  `ios.disk_encryption` (always on; report pass), `ios.jailbreak` (heuristic,
  optional).
- Distribution: App Store review needs the VPN usage justification, or
  Apple Business Manager / MDM for enterprise.

## 7. Controller and admin changes (shared by all platforms)

- `recognizedChecks` gains the `windows.*`, `android.*`, `ios.*` IDs; the
  posture proto is unchanged (IDs are strings).
- Posture profiles: per-OS requirement sets, and an evaluation rule that
  requirements for another OS prefix are not applied to a device. Admin UI
  follows (D-W3).
- Device list shows `os` as a platform (the client already sends it; only
  the literal changes).
- OAuth redirect URIs: add the mobile app-link/scheme values to the allowed
  set for the client flow.
- Release workflow: Windows MSI asset; mobile builds publish to the stores,
  not GitHub releases.
- No connector, relay, shield or proto change is needed.

## 8. Order and rough sizing

| Phase | Content | Depends on | Size |
|---|---|---|---|
| C0 | core/platform split, traits, login split, CI build-only targets | – | 1 to 2 weeks |
| W1 | Windows service, Wintun, named pipe, /32 routes, DPAPI state | C0 | 2 to 3 weeks |
| W2 | NRPT DNS, `windows.*` posture, per-OS profiles (controller + admin) | W1 | 2 weeks |
| W3 | CNG hardware key, MSI, signing, CI | W1 | 1 to 2 weeks |
| M0 | `zecurity-mobile` + UniFFI, deep-link login, buffer parameters, socket protect hook | C0 | 2 weeks |
| A1 | Android VpnService app, IP resources, Keystore-wrapped state | M0 | 3 weeks |
| A2 | Keystore device key, `android.*` posture, DNS names per D-A1 | A1 | 2 weeks |
| I1 | iOS extension, routes + matchDomains, ASWebAuthenticationSession, App Group | M0, entitlement | 3 to 4 weeks |
| I2 | Secure Enclave key, `ios.*` posture | I1 | 1 to 2 weeks |

Request the iOS entitlement during C0; it is on the critical path for I1.
W2's controller/admin work (per-OS profiles) is shared by every later phase
and should be done once, in W2.

## 9. Decisions needed (for the decision record, before C0 ends)

- **D-W1 Split tunnelling granularity off Linux.** Windows, Android and iOS
  get IP-level capture (one `/32` per resource IP, ADR-003 shape), not
  port-level (ADR-009). Non-ACL ports on a resource IP will be reset by
  smoltcp instead of reaching the LAN. Alternative on Windows is a signed WFP
  callout driver. Recommendation: accept IP-level on all three; revisit WFP
  only if a customer needs it.
- **D-W2 Identity scope on Windows.** The service holds the device identity
  as a per-machine identity (SYSTEM-owned state), while Linux is per enrolling
  user. Recommendation: per-machine on Windows and mobile; document that a
  Windows machine is one device regardless of who logs in.
- **D-W3 Per-OS posture profiles.** How a profile expresses requirements for
  several OSes and how the evaluator skips foreign prefixes. Recommendation:
  requirements carry an OS prefix implicitly (from the check ID) and only
  requirements matching the reporting device's `os_name` prefix are
  evaluated; `allow_unsupported` keeps its current meaning within an OS.
- **D-M1 Mobile login completion.** Deep link / app link replaces the
  loopback callback; the controller's redirect URI allow-list gains the
  mobile values. Recommendation: `https` universal/app links, custom scheme
  only as a fallback.
- **D-A1 DNS-name resources on Android.** Options: (a) none in A1; (b) make
  `198.19.255.53` the only DNS server of the VPN and have `dns.rs` forward
  every non-resource query to the device's real resolvers (new forwarder
  code, more moving parts); (c) per-app VPN with
  `addAllowedApplication`. Recommendation: (a) for A1, (b) for A2.
- **D-M2 Hardware keys on mobile.** Secure Enclave and Keystore are P-256
  only, consistent with the TPM choice; software keys stay P-384.
  Recommendation: accept; the fail-closed "requested means required" rule
  applies per platform setting.
- **D-M3 Memory and flow caps on mobile.** Buffer sizes and flow limits
  become configuration; iOS gets the smallest defaults. Needs a measured
  number from I1 before it is fixed.

## 10. Things that stay the same everywhere

- The protocol and the controller contract: enrolment, ACL/transport
  snapshots, change watch, posture report shape, renewal, revocation.
- The trust model: embedded platform root, intermediate pinning, CRL logic.
- The data-plane design: smoltcp in userspace, one QUIC connection per
  connector, relay fallback, 198.19.255.53 as the DNS answerer address.
- The state envelope format and the "no plaintext key on disk" rule; only
  the mechanism that protects the state key changes per platform.
- The security invariants in `CLAUDE.md`: fail closed, never log or return
  secrets, device identity never downgrades from hardware to software.
