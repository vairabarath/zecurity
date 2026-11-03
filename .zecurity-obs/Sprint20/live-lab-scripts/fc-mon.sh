#!/bin/bash
# fc-mon.sh [START] — fix-connector live acceptance snapshot (read-only).
START="${1:-10:49:00}"
L=~/s20-run/logs/controller.log
echo "== $(date +%T) hold"; tail -n2 ~/s20-run/logs/fc-hold.log
echo "== controller since $START (s20fc connectors)"
W=$(awk -v s="$START" '$2 >= s' $L)
for id in 9beadebf 1ff5bcaa c3c88536; do
  printf '%s ReEnroll=%s connected=%s superseded=%s disconnected=%s streamErr=%s\n' $id \
    "$(echo "$W" | grep $id | grep -c 'ReEnroll sent')" \
    "$(echo "$W" | grep $id | grep -c ' connected$')" \
    "$(echo "$W" | grep $id | grep -c 'superseded stream closed')" \
    "$(echo "$W" | grep $id | grep -c ' disconnected$')" \
    "$(echo "$W" | grep $id | grep -c 'stream error')"
done
echo "acl push lines: $(echo "$W" | grep -c 'acl push')"; echo "$W" | grep 'acl push' | tail -n3 | cut -c1-130
echo "notify topology lines: $(echo "$W" | grep -ci 'notify topology')"
echo "== client since $START"
J=$(journalctl -u zecurity-client --since "$START" --no-pager -o cat)
printf 'restarting VPN: %s  hot-applied: %s  keeping tunnel: %s  transport stored: %s\n' \
  "$(echo "$J" | grep -cE '(structural configuration change|snapshot changed), restarting VPN')" \
  "$(echo "$J" | grep -c 'transport map hot-applied successfully')" \
  "$(echo "$J" | grep -c 'effective config unchanged, keeping tunnel')" \
  "$(echo "$J" | grep -c 'transport snapshot stored version')"
# Fix 05 Phase 5-C: resource hot-apply lines.
printf 'phase5c: resource start=%s ok=%s pending-capacity=%s zero-K=%s pre-mutation-fail=%s recovery-restart=%s\n' \
  "$(echo "$J" | grep -c 'resource change, hot-applying')" \
  "$(echo "$J" | grep -c 'resources hot-applied')" \
  "$(echo "$J" | grep -c 'resource additions pending: address capacity')" \
  "$(echo "$J" | grep -c 'no routable resources; tunnel kept up, nothing captured')" \
  "$(echo "$J" | grep -c 'resource apply failed before mutation')" \
  "$(echo "$J" | grep -c 'resource apply failed after mutation, recovering with full restart')"
echo "== db sampler (status changes)"; grep -cv "active/.*active/" ~/s20-run/logs/fc-dbsamp.log | sed 's/^/lines-not-both-active: /'; tail -n3 ~/s20-run/logs/fc-dbsamp.log | cut -c1-200
echo "== 9092 probe"; grep -c FAIL ~/s20-run/logs/fc-probe9092.log | sed 's/^/FAIL lines: /'; tail -n2 ~/s20-run/logs/fc-probe9092.log
