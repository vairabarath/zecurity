#!/bin/bash
# fc-dbsamp.sh <seconds> — change-only sampler (100 ms) of the fix-connector lab:
# both s20fc connectors' status+serial and the shield's status/connector.
end=$(( $(date +%s) + ${1:-2400} )); prev=""
Q="select string_agg(name||'='||status||'/'||left(coalesce(cert_serial,''),8), ' ' order by name) from connectors where name in ('s20fc-conn-inkyank','s20fc-conn-udaya','s20fc-conn-manoj')
union all select string_agg(name||'='||status||'/via='||left(connector_id::text,8), ' ') from shields where name='s20fc-shield-nika'"
while [ $(date +%s) -lt $end ]; do
  cur=$(docker exec ztna_postgres psql -U ztna -d ztna_platform -At -c "$Q" 2>&1 | tr '\n' ' ')
  if [ "$cur" != "$prev" ]; then echo "$(date +%T.%3N) $cur"; prev="$cur"; fi
  sleep 0.1
done
