#!/bin/bash
# p1-mon.sh [START] — Fix 01 Phase 1 closing hold monitor (read-only).
START="${1:-12:30:40}"
echo "== $(date +%T) hold"; tail -n2 ~/s20-run/logs/p1-hold.log
J=$(journalctl -u zecurity-client --since "$START" --no-pager -o cat | sed -E 's/\x1b\[[0-9;]*m//g')
# Fix 01 Phase 2-A (D5): the full-restart line is 'structural configuration change, restarting VPN';
# pre-2-A binaries logged 'snapshot changed, restarting VPN' — count both so either build reads correctly.
printf 'client: keeping tunnel=%s  restarting VPN=%s  hot-apply start=%s ok=%s build-failed=%s fallback=%s  net_stack dead=%s  version changed=%s  sync failed=%s\n' \
  "$(echo "$J" | grep -c 'effective config unchanged, keeping tunnel')" \
  "$(echo "$J" | grep -cE '(structural configuration change|snapshot changed), restarting VPN')" \
  "$(echo "$J" | grep -c 'transport-only change, hot-applying connector map')" \
  "$(echo "$J" | grep -c 'transport map hot-applied successfully')" \
  "$(echo "$J" | grep -c 'transport map build failed, retaining existing map')" \
  "$(echo "$J" | grep -c 'hot-apply not possible')" \
  "$(echo "$J" | grep -c 'net_stack task is not running')" \
  "$(echo "$J" | grep -cE '(version changed, restarting tunnel|change detected, evaluating tunnel configuration)')" \
  "$(echo "$J" | grep -c 'sync failed')"
# F-1: the sync caller line is 'background sync: change detected, evaluating tunnel configuration';
# pre-F-1 binaries logged '… version changed, restarting tunnel' — 'version changed=' counts both.
# Fix 05 Phase 5-C: resource hot-apply lines.
printf 'phase5c: resource start=%s ok=%s pending-capacity=%s zero-K=%s pre-mutation-fail=%s recovery-restart=%s\n' \
  "$(echo "$J" | grep -c 'resource change, hot-applying')" \
  "$(echo "$J" | grep -c 'resources hot-applied')" \
  "$(echo "$J" | grep -c 'resource additions pending: address capacity')" \
  "$(echo "$J" | grep -c 'no routable resources; tunnel kept up, nothing captured')" \
  "$(echo "$J" | grep -c 'resource apply failed before mutation')" \
  "$(echo "$J" | grep -c 'resource apply failed after mutation, recovering with full restart')"
echo "$J" | grep -E 'keeping tunnel|restarting VPN|hot-appl|transport map|stored version|sync failed|version changed|evaluating tunnel' | cut -c1-170 | tail -6
W=$(awk -v s="$START" '$2 >= s' ~/s20-run/logs/controller.log)
for id in 9beadebf 1ff5bcaa c3c88536; do
  printf '%s ReEnroll=%s superseded=%s disconnected=%s\n' $id "$(echo "$W"|grep $id|grep -c 'ReEnroll sent')" "$(echo "$W"|grep $id|grep -c 'superseded')" "$(echo "$W"|grep $id|grep -c ' disconnected$')"
done
echo "acl push since $START:"; echo "$W" | grep 'acl push' | awk '{print "  "$2, $7, $8, $9}'
