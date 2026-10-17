#!/bin/bash
# Serve ~/s20-run/serve on the LAN for 30 min (bundle pull), as a user unit.
systemctl --user reset-failed s20-serve 2>/dev/null
PY=$(command -v python3)
systemd-run --user --unit=s20-serve --working-directory=/home/inkyank-01/s20-run/serve \
  -p RuntimeMaxSec=1800 "$PY" -m http.server 8765 --bind 192.168.1.164
