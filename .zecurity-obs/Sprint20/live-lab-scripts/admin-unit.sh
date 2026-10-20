#!/bin/bash
# Run the admin UI dev server as a user systemd unit so Hermes turn interrupts can't kill it.
pkill -f "node.*vite" ; sleep 1
systemctl --user reset-failed s20-admin 2>/dev/null
systemd-run --user --unit=s20-admin --working-directory=/home/inkyank-01/zecurity/admin \
  -p StandardOutput=append:/home/inkyank-01/s20-run/logs/admin.log \
  -p StandardError=append:/home/inkyank-01/s20-run/logs/admin.log \
  /usr/bin/env PATH="$PATH" npm run dev -- --host 127.0.0.1 --port 5173 --strictPort
