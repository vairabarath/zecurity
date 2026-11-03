#!/bin/bash
# osend.sh <orca-terminal-handle> <MARKER> '<command>'
# Sends <command> to an Orca terminal wrapped in ==MARKER== / ==MARKERE== echo
# markers, polls until the end marker appears, and prints only the output in between.
# WAIT=<seconds> overrides the default 40 s poll budget. Prints TIMEOUT + tail on expiry.
# Use a fresh MARKER per call (U1, U2, ...) so old scrollback never matches.
# Exits 1 with the raw JSON if the send is refused (e.g. terminal_handle_stale after an
# Orca restart): re-run `orca-ide terminal list --json` and remap handles by title.
#
# orca-ide finds the running app via $XDG_CONFIG_HOME/orca/orca-runtime.json. A shell whose
# XDG_CONFIG_HOME points elsewhere (e.g. a bot desktop session) gets `runtime_unavailable`
# even though Orca is up, so pin it to the user's config unless ORCA_XDG overrides it.
export XDG_CONFIG_HOME="${ORCA_XDG:-$HOME/.config}"
T=$1; M=$2; C=$3
resp=$(orca-ide terminal send --terminal "$T" --text "echo ==${M}==; ${C}; echo; echo ==${M}E==" --enter --json 2>&1)
if ! echo "$resp" | grep -q '"ok": true'; then echo "SEND FAILED"; echo "$resp"; exit 1; fi
for i in $(seq 1 ${WAIT:-40}); do
  out=$(orca-ide terminal read --terminal "$T" --json | python3 -c "import json,sys;d=json.load(sys.stdin);t=d['result']['terminal']['tail'];t=t if isinstance(t,list) else t.split('\n');print('\n'.join(t))")
  if echo "$out" | grep -q "^==${M}E==\$"; then
    echo "$out" | awk "/^==${M}==\$/{f=1;next}/^==${M}E==\$/{f=0}f"; exit 0
  fi
  sleep 1
done
echo "TIMEOUT"; echo "$out" | tail -15
