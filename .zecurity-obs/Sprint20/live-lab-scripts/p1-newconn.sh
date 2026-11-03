#!/bin/bash
# p1-newconn.sh <seconds> — every 2 s open a NEW connection to summa (change-only log).
end=$(( $(date +%s) + ${1:-600} )); prev=""
while [ $(date +%s) -lt $end ]; do
  c=$(curl -s -o /dev/null -m 4 -w '%{http_code}' http://192.168.1.38:5173/)
  if [ "$c" != "$prev" ]; then echo "$(date +%T.%3N) newconn=$c"; prev="$c"; fi
  sleep 2
done
