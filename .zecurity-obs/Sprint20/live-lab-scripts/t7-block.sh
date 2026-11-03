#!/bin/bash
# t7-block.sh <start HH:MM:SS> <block-secs> — TEST 7 (sudo, on inkyank). Self-reverting.
# From <start> for <block-secs>: allow ONE new outbound SYN to controller :9090
# (the renewal's RenewCert connection), then reset every further new :9090 connection
# (the replacement Control stream opens + shield-proxy rebuild). Established streams
# are untouched. Table is always deleted at the end (trap too). Log: /tmp/t7.log
START=$1; BLOCK_SECS=${2:-45}
cleanup() { nft delete table inet s20t7 2>/dev/null; echo "BLOCK_OFF $(date +%T.%3N)" >> /tmp/t7.log; }
trap cleanup EXIT
now=$(date +%s); at=$(date -d "$START" +%s); [ $at -gt $now ] && sleep $((at-now))
nft add table inet s20t7
nft add chain inet s20t7 out '{ type filter hook output priority 0; policy accept; }'
nft add rule inet s20t7 out ip daddr 192.168.1.164 tcp dport 9090 tcp flags syn ct state new limit rate over 1/minute burst 1 packets counter reject with tcp reset
echo "BLOCK_ON $(date +%T.%3N) secs=$BLOCK_SECS" >> /tmp/t7.log
sleep "$BLOCK_SECS"
nft list table inet s20t7 | grep counter >> /tmp/t7.log
