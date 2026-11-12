#!/usr/bin/env bash
# Builds libzecurity_ffi.so (client/ffi: the Rust client core) for the
# Android app into android/app/src/main/jniLibs/<abi>/, or into
# JNI_OUT/<abi>/ when set (keeps the build output out of the repo; then pass
# the same JNI_OUT to build-apk.sh).
#
# The client trusts only the platform root CA compiled into it, so, as with
# client/scripts/dev-build.sh, the build embeds a controller's root:
#   ROOT_PEM=/path/to/root.pem   embed this root
#   (unset)                      export it from the local compose database
#
# Needs: the Android NDK (ANDROID_NDK_HOME, or $ANDROID_HOME/ndk/<version>),
# cargo-ndk, and the Rust targets aarch64-linux-android and
# x86_64-linux-android. PROFILE=debug builds without optimisation.
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
export ZECURITY_PLATFORM_ROOT_PEM_FILE="$(realpath "$root")"
echo "build-native: embedding platform root $ZECURITY_PLATFORM_ROOT_PEM_FILE" >&2

if [[ -z "${ANDROID_NDK_HOME:-}" ]]; then
  ndk="$(ls -d "${ANDROID_HOME:?set ANDROID_HOME or ANDROID_NDK_HOME}"/ndk/* | sort -V | tail -1)"
  export ANDROID_NDK_HOME="$ndk"
fi

profile=()
[[ "${PROFILE:-release}" == release ]] && profile=(--release)

cd "$client/ffi"
out="${JNI_OUT:-$mobile/android/app/src/main/jniLibs}"
cargo ndk -t arm64-v8a -t x86_64 --platform 24 -o "$out" build "${profile[@]}"
ls -l "$out"/*/libzecurity_ffi.so
