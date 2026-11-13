# Zecurity mobile client (Android)

Flutter app + the Rust client core. The daemon runs, unchanged, inside the
app's VPN service process (`ZecurityVpnService`, process `:vpn`); this UI
plays the Linux CLI's role and talks to it over a socket in the app's private
files directory. Design, limits and status: `docs/client-multi-platform.md`
section 12.

| Path | What |
|---|---|
| `lib/` | Flutter UI (`main.dart`), the native core over `dart:ffi` (`native.dart`), the Android channel (`host.dart`) |
| `android/app/src/main/kotlin/io/zecurity/client/` | `MainActivity`, `ZecurityVpnService` (daemon host, VPN interface), `ZecurityNative` (JNI) |
| `../ffi/` | `libzecurity_ffi.so`: C ABI for Dart, JNI for the service |
| `scripts/build-native.sh` | builds the `.so` for arm64-v8a and x86_64 (embeds the controller's root CA) |
| `scripts/build-apk.sh` | builds the APK into `dist/` |

## Build

Needs Flutter (stable), the Android SDK with NDK, JDK 17, `cargo-ndk`, and
`rustup target add aarch64-linux-android x86_64-linux-android`.

```bash
ROOT_PEM=/path/to/controller-root.pem scripts/build-native.sh
scripts/build-apk.sh            # → dist/zecurity-client.apk
```

## Use

Install the APK, open it, enter the workspace (and, for a dev controller,
its address and HTTP base) in Settings, sign in, then Connect and accept
Android's VPN prompt.
