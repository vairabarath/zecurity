#!/usr/bin/env bash
# test-relay-installer-state.sh — the relay installer must notice state left by
# ANOTHER relay before installing anything (V4 observation, 2026-10-30): the
# relay refuses to start when its saved certificate names a different relay
# ("Relay certificate ID … does not match configured Relay ID …").
#
# Runs without root: check_existing_state() is extracted from
# scripts/relay-local-install.sh and run against temp dirs, real openssl
# certificates and a fake systemctl that records its calls.
#
# Usage: bash scripts/test-relay-installer-state.sh   (exit 0 = all pass)

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
fail=0

command -v openssl >/dev/null || { echo "SKIP: openssl not installed"; exit 0; }

fn="$(sed -n '/^check_existing_state() {/,/^}/p' "$ROOT/scripts/relay-local-install.sh")"
[[ -n "$fn" ]] || { echo "FAIL check_existing_state not found in relay-local-install.sh"; exit 1; }

# A fake systemctl: records calls; "is-active" succeeds when $TMP/active exists.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/systemctl" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$TMP/calls"
if [[ "\$1" == is-active ]]; then [[ -e "$TMP/active" ]]; exit; fi
exit 0
EOF
chmod +x "$TMP/bin/systemctl"

OLD=11111111-2222-4333-8444-555555555555
NEW=aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee

# make_state DIR RELAY_ID|garbage — a state dir as the relay leaves it.
make_state() {
    mkdir -p "$1"
    if [[ "$2" == garbage ]]; then
        echo "not a certificate" > "$1/relay.crt"
    else
        openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
            -subj "/CN=relay" -addext "subjectAltName=URI:spiffe://zecurity.in/relay/$2" \
            -keyout "$1/relay.key" -out "$1/relay.crt" 2>/dev/null
    fi
    echo meta > "$1/relay.crt.meta"
    echo ca > "$1/intermediate-ca.crt"
}

# run DIR [VAR=VALUE…] — run check_existing_state in a subshell; prints its
# exit status and leaves its output in $TMP/out.
run() {
    local dir="$1"; shift
    : > "$TMP/calls"
    (
        export PATH="$TMP/bin:$PATH" RELAY_ID="$NEW"
        for kv in "$@"; do export "${kv?}"; done
        log()  { echo "LOG $*"; }
        warn() { echo "WARN $*"; }
        err()  { echo "ERR $*"; exit 1; }
        eval "$fn"
        check_existing_state "$dir"
    ) > "$TMP/out" 2>&1 && echo 0 || echo $?
}

check() { # name, condition
    if eval "$2"; then echo "PASS $1"; else echo "FAIL $1"; echo "  output: $(tr '\n' ' ' < "$TMP/out")"; fail=1; fi
}

# 1. No state yet: nothing to do.
st=$(run "$TMP/none")
check "no state: proceeds" '[[ $st == 0 ]]'

# 2. A reinstall of the SAME relay keeps its state.
make_state "$TMP/same" "$NEW"
st=$(run "$TMP/same")
check "same relay: proceeds" '[[ $st == 0 ]]'
check "same relay: state kept" '[[ -f $TMP/same/relay.key && -f $TMP/same/relay.crt ]]'
check "same relay: says so" 'grep -q "keeping it" "$TMP/out"'

# 3. Another relay's state: refuse, name both relays, touch nothing.
make_state "$TMP/other" "$OLD"
st=$(run "$TMP/other")
check "other relay: refuses" '[[ $st != 0 ]]'
check "other relay: names both IDs" 'grep -q "$OLD" "$TMP/out" && grep -q "$NEW" "$TMP/out"'
check "other relay: tells how to proceed" 'grep -q "RELAY_RESET_STATE=1" "$TMP/out"'
check "other relay: files untouched" '[[ -f $TMP/other/relay.key && -f $TMP/other/relay.crt ]]'
check "other relay: service not stopped" '! grep -q "^stop" "$TMP/calls"'
check "other relay: no backup made" '[[ -z $(ls -d "$TMP"/other.replaced-* 2>/dev/null) ]]'

# 4. RELAY_RESET_STATE=1: stop a running service, move the state aside, keep it.
touch "$TMP/active"
st=$(run "$TMP/other" RELAY_RESET_STATE=1)
rm -f "$TMP/active"
backup=$(ls -d "$TMP"/other.replaced-* 2>/dev/null | head -1)
check "reset: proceeds" '[[ $st == 0 ]]'
check "reset: running service stopped first" 'grep -q "^stop zecurity-relay.service" "$TMP/calls"'
check "reset: old state moved to a backup" '[[ -n $backup && -f $backup/relay.key && -f $backup/relay.crt && -f $backup/relay.crt.meta && -f $backup/intermediate-ca.crt ]]'
check "reset: backup is private" '[[ -n $backup && $(stat -c %a "$backup") == 700 ]]'
check "reset: state dir now empty" '[[ -d $TMP/other && -z $(ls -A "$TMP/other") ]]'

# 5. Only "1" resets: other values still refuse.
make_state "$TMP/yes" "$OLD"
st=$(run "$TMP/yes" RELAY_RESET_STATE=yes)
check "reset=yes: still refuses" '[[ $st != 0 && -f $TMP/yes/relay.key ]]'

# 6. An unreadable certificate is not trusted to be ours.
make_state "$TMP/garbage" garbage
st=$(run "$TMP/garbage")
check "unreadable certificate: refuses" '[[ $st != 0 ]] && grep -q "unreadable" "$TMP/out"'

# 7. Without openssl the certificate can't be read: refuse, don't guess.
make_state "$TMP/noossl" "$NEW"
mkdir -p "$TMP/nobin"
for t in bash env sed grep head date mv install find ls cat; do
    p="$(command -v "$t")"; [[ -n "$p" ]] && ln -sf "$p" "$TMP/nobin/$t"
done
ln -sf "$TMP/bin/systemctl" "$TMP/nobin/systemctl"
: > "$TMP/calls"
st=$( ( export PATH="$TMP/nobin" RELAY_ID="$NEW"
        log() { echo "LOG $*"; }; warn() { echo "WARN $*"; }; err() { echo "ERR $*"; exit 1; }
        eval "$fn"; check_existing_state "$TMP/noossl" ) > "$TMP/out" 2>&1 && echo 0 || echo $? )
check "no openssl: refuses" '[[ $st != 0 ]] && grep -q "openssl" "$TMP/out"'

exit $fail
