#!/usr/bin/env bash
# Builds the Rust client core for iOS as ios/Frameworks/ZecurityCore.xcframework
# (static library + header + module map), for iPhone (aarch64-apple-ios) and
# the Apple-silicon simulator (aarch64-apple-ios-sim). Runs on macOS with
# Xcode; see ios/README.md for the Xcode setup that links it.
#
# Root CA, as client/scripts/dev-build.sh:
#   ROOT_PEM=/path/to/root.pem   embed this root
#   (unset)                      export it from the local compose database
set -euo pipefail

mobile="$(cd "$(dirname "$0")/.." && pwd)"
client="$(cd "$mobile/.." && pwd)"
repo="$(cd "$client/.." && pwd)"

root="${ROOT_PEM:-}"
if [[ -z "$root" ]]; then
  mkdir -p "$client/target"
  root="$client/target/dev-platform-root.pem"
  "$repo/scripts/export-platform-root.sh" "$root"
fi
export ZECURITY_PLATFORM_ROOT_PEM_FILE="$(cd "$(dirname "$root")" && pwd)/$(basename "$root")"
echo "build-ios-native: embedding platform root $ZECURITY_PLATFORM_ROOT_PEM_FILE" >&2

targets=(aarch64-apple-ios aarch64-apple-ios-sim)
rustup target add "${targets[@]}" >/dev/null

target_dir="${CARGO_TARGET_DIR:-$client/ffi/target}"
cd "$client/ffi"
args=()
for t in "${targets[@]}"; do
  # A static library for iOS only; Cargo.toml's cdylib stays for Android.
  cargo rustc --release --lib --crate-type staticlib --target "$t"
  args+=(-library "$target_dir/$t/release/libzecurity_ffi.a" -headers "$mobile/ios/ZecurityCore/include")
done

out="$mobile/ios/Frameworks/ZecurityCore.xcframework"
rm -rf "$out"
mkdir -p "$(dirname "$out")"
xcodebuild -create-xcframework "${args[@]}" -output "$out"
echo "built $out"
