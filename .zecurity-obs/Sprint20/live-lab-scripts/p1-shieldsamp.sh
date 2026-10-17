#!/bin/bash
# p1-shieldsamp.sh <seconds> — change-only (500 ms): which connector the shield is on + connector statuses.
end=$(( $(date +%s) + ${1:-900} )); prev=""
Q="select string_agg(c.name||'='||c.status, ' ' order by c.name) || ' | shield_via=' ||
  (select c2.name from shields s join connectors c2 on c2.id=s.connector_id where s.name='s20fc-shield-nika')
  from connectors c where c.name like 's20fc-%'"
while [ $(date +%s) -lt $end ]; do
  cur=$(docker exec ztna_postgres psql -U ztna -d ztna_platform -At -c "$Q" 2>&1)
  if [ "$cur" != "$prev" ]; then echo "$(date +%T.%3N) $cur"; prev="$cur"; fi
  sleep 0.5
done
