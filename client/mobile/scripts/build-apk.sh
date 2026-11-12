#!/usr/bin/env bash
# Builds the Android APK (release, arm64 + x86_64, signed with the debug key
# as the Flutter template does). Run scripts/build-native.sh first.
#
# BUILD_DIR=/path   build in a copy of this project there (keeps Gradle and
#                   Flutter build output off this disk), then copy the APK to
#                   APK_OUT (default: this project's dist/).
set -euo pipefail

mobile="$(cd "$(dirname "$0")/.." && pwd)"
ls "$mobile"/android/app/src/main/jniLibs/*/libzecurity_ffi.so >/dev/null \
  || { echo "build-apk: run scripts/build-native.sh first" >&2; exit 1; }

src="$mobile"
if [[ -n "${BUILD_DIR:-}" ]]; then
  src="$BUILD_DIR/mobile"
  mkdir -p "$src"
  find "$src" -mindepth 1 -maxdepth 1 ! -name build ! -name .dart_tool -exec rm -rf {} +
  tar -C "$mobile" --exclude=./build --exclude=./.dart_tool --exclude=./dist \
      --exclude=./android/.gradle -cf - . | tar -C "$src" -xf -
fi

cd "$src"
flutter build apk --release --target-platform android-arm64,android-x64

out="${APK_OUT:-$mobile/dist}"
mkdir -p "$out"
cp build/app/outputs/flutter-apk/app-release.apk "$out/zecurity-client.apk"
sha256sum "$out/zecurity-client.apk"
