#!/bin/sh
# PENDING-11 S-5 (D-12): give the runtime role ztna_app (created NOLOGIN by
# 051_audit_chain.sql) a login and password. Runs once, after every *.sql file
# (docker-entrypoint-initdb.d runs files in name order). DB-backed tests apply
# only *.sql and use SET ROLE ztna_app instead.
set -eu
if [ -z "${APP_DB_PASSWORD:-}" ]; then
    echo "zz_app_role_login.sh: APP_DB_PASSWORD is not set; ztna_app stays NOLOGIN" >&2
    exit 1
fi
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
    -v app_pw="$APP_DB_PASSWORD" <<'EOSQL'
ALTER ROLE ztna_app LOGIN PASSWORD :'app_pw';
EOSQL
