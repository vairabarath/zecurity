#!/usr/bin/env bash
# Run the controller tests that need RESOURCE_TEST_SHIELD_ID against a
# throwaway database.
#
# Those tests (graph/resolvers resource-ACL coherence and posture resolver
# tests, internal/resource snapshot generation and ack/reap) predate the
# throwaway-database rule in docs/database-development.md §5: they use
# RESOURCE_TEST_DATABASE_URL as the database itself and need an existing
# active shield. This script creates a fresh database, applies every schema
# file, seeds one workspace / remote network / connector / active shield (on
# host 10.0.0.95, the host TestProtectResource_DoesNotInvalidate protects),
# runs the tests, and drops the database. It never touches another database.
#
# Usage: scripts/resource-test-fixture.sh [go test args...]
#   POSTGRES_CONTAINER (default ztna_postgres), POSTGRES_USER (default ztna),
#   POSTGRES_PASSWORD (default ztna_dev_secret), POSTGRES_HOST_PORT
#   (default localhost:5432).
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
container="${POSTGRES_CONTAINER:-ztna_postgres}"
user="${POSTGRES_USER:-ztna}"
password="${POSTGRES_PASSWORD:-ztna_dev_secret}"
hostport="${POSTGRES_HOST_PORT:-localhost:5432}"
db="resource_fixture_$$_$(date +%s)"

psql_db() { docker exec -i "$container" psql -U "$user" -d "$1" -v ON_ERROR_STOP=1 -qAt "${@:2}"; }
cleanup() { psql_db postgres -c "DROP DATABASE IF EXISTS $db" >/dev/null 2>&1 || true; }
trap cleanup EXIT

psql_db postgres -c "CREATE DATABASE $db" >/dev/null
for f in $(ls "$repo"/controller/migrations/*.sql | sort); do
  psql_db "$db" <"$f" >/dev/null 2>&1
done
shield_id="$(psql_db "$db" <<'SQL'
WITH ws AS (INSERT INTO workspaces (slug, name, trust_domain, status)
            VALUES ('resource-fixture', 'resource-fixture', 'resource-fixture.zecurity.in', 'active')
            RETURNING id),
     rn AS (INSERT INTO remote_networks (tenant_id, name, location)
            SELECT id, 'rn-fixture', 'office' FROM ws RETURNING id, tenant_id),
     cn AS (INSERT INTO connectors (tenant_id, remote_network_id, name, trust_domain, status)
            SELECT tenant_id, id, 'connector-fixture', 'resource-fixture.zecurity.in', 'active' FROM rn
            RETURNING id, tenant_id, remote_network_id)
INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain, lan_ip)
SELECT tenant_id, remote_network_id, id, 'shield-fixture', 'active', 'resource-fixture.zecurity.in', '10.0.0.95'
FROM cn
RETURNING id;
SQL
)"
shield_id="$(tail -1 <<<"$shield_id")"

cd "$repo/controller"
RESOURCE_TEST_DATABASE_URL="postgres://$user:$password@$hostport/$db?sslmode=disable" \
RESOURCE_TEST_SHIELD_ID="$shield_id" \
  go test -count=1 ./graph/resolvers/ ./internal/resource/ "$@"
