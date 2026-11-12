#!/bin/sh
# PENDING-11 S-5 (D-12): give the runtime role ztna_app (created NOLOGIN by
# 053_audit_chain.sql) a login and password. Runs once, after every *.sql file
# (docker-entrypoint-initdb.d runs files in name order). DB-backed tests apply
# only *.sql and use SET ROLE ztna_app instead.
set -eu
if [ -z "${APP_DB_PASSWORD:-}" ]; then
    # Don't break a stack that mounts migrations/ without the variable; the
    # controller's startup privilege check reports an owner DATABASE_URL.
    echo "zz_app_role_login.sh: WARNING: APP_DB_PASSWORD is not set; ztna_app stays NOLOGIN" >&2
    exit 0
fi
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
    -v app_pw="$APP_DB_PASSWORD" <<'EOSQL'
ALTER ROLE ztna_app LOGIN PASSWORD :'app_pw';
EOSQL
