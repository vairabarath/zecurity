#!/bin/bash
# sockdump.sh <seconds> <ss-filter> — change-only (200 ms) socket + agent PID sampler.
end=$(( $(date +%s) + $1 )); prev=""
while [ $(date +%s) -lt $end ]; do
  pid=$(systemctl show zecurity-connector -p MainPID --value 2>/dev/null)
  [ "$pid" = "0" -o -z "$pid" ] && pid=$(systemctl show zecurity-shield -p MainPID --value 2>/dev/null)
  socks=$(ss -Htn state established "$2" 2>/dev/null | awk '{print $3"->"$4}' | sort | tr '\n' ' ')
  cur="pid=$pid $socks"
  if [ "$cur" != "$prev" ]; then echo "$(date +%T.%3N) $cur"; prev="$cur"; fi
  sleep 0.2
done
