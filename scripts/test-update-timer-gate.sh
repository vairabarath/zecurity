#!/usr/bin/env bash
# test-update-timer-gate.sh — Sprint 20 Fix 11: the installers must enable the
# auto-update timer only when AUTO_UPDATE_ENABLED is true, and the local
# installers must never enable it.
#
# Runs without root: configure_update_timer() is extracted from the release
# installers and run against a fake systemctl that records its calls.
#
# Usage: bash scripts/test-update-timer-gate.sh   (exit 0 = all pass)

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
fail=0

# A fake systemctl that appends its arguments to $TMP/calls.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/systemctl" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$TMP/calls"
EOF
chmod +x "$TMP/bin/systemctl"

check() { # name, condition result
    if [[ "$2" == ok ]]; then echo "PASS $1"; else echo "FAIL $1"; fail=1; fi
}

for unit in connector shield; do
    script="$ROOT/$unit/scripts/$unit-install.sh"
    [[ $unit == connector ]] || script="$ROOT/shield/scripts/shield-install.sh"
    fn="$(sed -n '/^configure_update_timer() {/,/^}/p' "$script")"
    [[ -n "$fn" ]] || { echo "FAIL $unit: configure_update_timer not found"; fail=1; continue; }

    for value in true TRUE " " false 0 no garbage "" unset 1 yes; do
        : > "$TMP/calls"
        (
            PATH="$TMP/bin:$PATH"
            log() { :; }
            if [[ "$value" == unset ]]; then unset AUTO_UPDATE_ENABLED; AUTO_UPDATE_ENABLED=""; else AUTO_UPDATE_ENABLED="$value"; fi
            eval "$fn"
            configure_update_timer
        )
        calls="$(cat "$TMP/calls")"
        case "$value" in
            true|TRUE|1|yes)
                [[ "$calls" == "enable --now zecurity-$unit-update.timer" ]] && r=ok || r=bad
                check "$unit AUTO_UPDATE_ENABLED='$value' enables the timer only" "$r" ;;
            *)
                if grep -q "^enable" <<<"$calls"; then r=bad
                elif grep -q "^disable --now zecurity-$unit-update.timer" <<<"$calls" \
                  && grep -q "^stop zecurity-$unit-update.service" <<<"$calls"; then r=ok
                else r=bad; fi
                check "$unit AUTO_UPDATE_ENABLED='$value' disables the timer" "$r" ;;
        esac
    done
done

# Local installers (branch builds): never enable the update timer.
for unit in connector shield; do
    script="$ROOT/scripts/$unit-local-install.sh"
    if grep -qE "^[^#]*systemctl enable[^#]*zecurity-$unit-update\.timer" "$script"; then r=bad
    elif grep -qE "^systemctl disable --now zecurity-$unit-update\.timer" "$script"; then r=ok
    else r=bad; fi
    check "$unit-local-install.sh never enables the update timer" "$r"
done

# Units: no Requires= on the service in the timers; no hard-coded AUTO_UPDATE_ENABLED.
for unit in connector shield; do
    grep -qE '^Requires=' "$ROOT/$unit/systemd/zecurity-$unit-update.timer" && r=bad || r=ok
    check "zecurity-$unit-update.timer has no Requires=" "$r"
    grep -qE '^Environment=AUTO_UPDATE_ENABLED' "$ROOT/$unit/systemd/zecurity-$unit-update.service" && r=bad || r=ok
    check "zecurity-$unit-update.service has no hard-coded AUTO_UPDATE_ENABLED" "$r"
done

exit $fail
