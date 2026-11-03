#!/usr/bin/env bash
# Build zecurity-client for a dev or LAN controller, embedding THAT
# controller's platform root CA (certificate audit bug 1). The client trusts
# only the root compiled into it, so a client built here works only with the
# controller whose root it embeds.
#
# Usage:
#   client/scripts/dev-build.sh [cargo build args...]     e.g. --release
#
# Root:
#   ROOT_PEM=/path/to/root.pem   embed this root (e.g. copied from the
#                                controller host with scripts/export-platform-root.sh)
#   (unset)                      export it from the local compose database
#
# After a local DB reset the controller has a NEW root: rebuild, then
# `zecurity-client login` again.
set -euo pipefail

client="$(cd "$(dirname "$0")/.." && pwd)"
repo="$(cd "$client/.." && pwd)"

root="${ROOT_PEM:-}"
if [[ -z "$root" ]]; then
  mkdir -p "$client/target"
  root="$client/target/dev-platform-root.pem"
  "$repo/scripts/export-platform-root.sh" "$root"
fi

ZECURITY_PLATFORM_ROOT_PEM_FILE="$(realpath "$root")"
export ZECURITY_PLATFORM_ROOT_PEM_FILE
echo "dev-build: embedding platform root $ZECURITY_PLATFORM_ROOT_PEM_FILE" >&2
cd "$client"
exec cargo build "$@"
