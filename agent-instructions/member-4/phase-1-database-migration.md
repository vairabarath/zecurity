# Phase 1 — Database Migration (DAY 1 — COMMIT FIRST)

This migration unblocks **Member 3** (DB queries in enrollment/heartbeat handlers) and **Member 1** (after codegen). Commit this first — before anything else.

---

## Role & Ownership

Member 4 owns the widest surface area: database migrations, GraphQL schema extensions, connector resolvers on the Go side, and the entire Rust connector binary plus its deployment infrastructure.

### Files Member 4 creates

```
controller/migrations/002_connector_schema.sql
```

### Files Member 4 modifies

```
controller/graph/schema.graphqls
```

### DO NOT TOUCH

- `controller/migrations/001_schema.sql` — Sprint 1 schema, immutable
- `controller/internal/appmeta/identity.go` — Member 3 owns SPIFFE constants
- `controller/proto/connector.proto` — Member 2 writes this
- `controller/internal/connector/config.go` — Member 2 owns Config struct
- `controller/internal/connector/token.go` — Member 2 owns token generation
- `controller/internal/connector/spiffe.go` — Member 3 owns SPIFFE parsing
- `controller/internal/connector/enrollment.go` — Member 3 owns Enroll handler
- `controller/internal/connector/heartbeat.go` — Member 3 owns Heartbeat handler
- `controller/internal/pki/*` — Member 3 owns PKI code
- `controller/cmd/server/main.go` — Member 2 wires everything
- `controller/internal/auth/*` — Sprint 1 auth code
- `controller/internal/bootstrap/*` — Sprint 1 bootstrap code
- `controller/docker-compose.yml` — Sprint 1 dev infra
- `admin/` — Member 1 owns all frontend code
- `Makefile` — Shared; do not modify without coordination

---

## Part 1 — Extend workspaces table

```sql
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS trust_domain TEXT UNIQUE;

UPDATE workspaces
   SET trust_domain = 'ws-' || slug || '.zecurity.in'
 WHERE trust_domain IS NULL;

ALTER TABLE workspaces
    ALTER COLUMN trust_domain SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_trust_domain
    ON workspaces (trust_domain);
```

---

## Part 2 — remote_networks table

```sql
CREATE TABLE remote_networks (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    location    TEXT        NOT NULL CHECK (location IN ('home','office','aws','gcp','azure','other')),
    status      TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','deleted')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX idx_remote_networks_tenant ON remote_networks (tenant_id);
```

---

## Part 3 — connectors table

```sql
CREATE TABLE connectors (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    remote_network_id    UUID        NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,
    status               TEXT        NOT NULL DEFAULT 'pending'
                                     CHECK (status IN ('pending','active','disconnected','revoked')),
    enrollment_token_jti TEXT,
    trust_domain         TEXT,
    cert_serial          TEXT,
    cert_not_after       TIMESTAMPTZ,
    last_heartbeat_at    TIMESTAMPTZ,
    version              TEXT,
    hostname             TEXT,
    public_ip            TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_connectors_tenant         ON connectors (tenant_id);
CREATE INDEX idx_connectors_remote_network ON connectors (remote_network_id, tenant_id);
CREATE INDEX idx_connectors_token_jti      ON connectors (enrollment_token_jti);
CREATE INDEX idx_connectors_trust_domain   ON connectors (trust_domain);
```

---

## Phase 1 Checklist

```
✓ 002_connector_schema.sql created with all 3 parts
✓ workspaces table extended with trust_domain
✓ remote_networks table created with proper constraints
✓ connectors table created with proper constraints and indexes
✓ Committed and pushed — unblocks Member 3 + Member 1
```

---

## After This Phase

**Immediately commit and push.** Then notify:
- Member 3: "migration is merged, tables are ready for DB queries"
- Member 1: "migration is merged, you can proceed after schema.graphqls update"

Then proceed to Phase 2 (GraphQL schema update).
