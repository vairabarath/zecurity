#!/bin/bash
# fc-probe9092.sh <seconds> — change-only :9092 serial probe (100 ms) for both connectors.
end=$(( $(date +%s) + ${1:-2400} )); prev=""
s() { timeout 2 openssl s_client -connect $1:9092 -alpn ztna-tunnel-v1 </dev/null 2>/dev/null | openssl x509 -noout -serial 2>/dev/null | cut -c8-15; }
while [ $(date +%s) -lt $end ]; do
  a=$(s 192.168.1.75); b=$(s 192.168.1.42)
  cur="ink9092=${a:-FAIL} udaya9092=${b:-FAIL}"
  if [ "$cur" != "$prev" ]; then echo "$(date +%T.%3N) $cur"; prev="$cur"; fi
  sleep 0.1
done
