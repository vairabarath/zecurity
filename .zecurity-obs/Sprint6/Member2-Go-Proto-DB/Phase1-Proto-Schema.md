---
type: phase
status: done
sprint: 6
member: M2
phase: Phase1-Proto-Schema
depends_on: []
tags:
  - proto
  - db
  - graphql
  - day1
---

# M2 Phase 1 — Proto, Migration, GraphQL Schema (Day 1)

> **This is Day 1 work. Everyone else is blocked until you commit.**

---

## What You're Building

Three proto additions, one DB migration, one GraphQL schema file. No Go implementation yet — that's Phase 2.

---

## Files to Touch

### 1. `proto/shield/v1/shield.proto`

Add two new messages and one new oneof variant.

**New messages** (add after existing `Pong` message):

```proto
message DiscoveredService {
  string protocol     = 1;  // "tcp"
  uint32 port         = 2;
  string bound_ip     = 3;
  string service_name = 4;  // empty string if unknown port
}

message DiscoveryReport {
  string shield_id                    = 1;
  uint64 seq                          = 2;  // monotonically increasing per shield session
  repeated DiscoveredService added    = 3;  // services that appeared since last report
  repeated DiscoveredService removed  = 4;  // only port + protocol are meaningful here
  uint64 fingerprint                  = 5;  // hash over current full port set
  bool   full_sync                    = 6;  // true = replace-all, false = diff
}
```

**Modify `ShieldControlMessage.oneof body`** — add after `pong = 6`:

```proto
// Shield → Connector
DiscoveryReport discovery_report = 7;
```

> **Rule:** field 7 is assigned to this sprint. Fields 8–11 are reserved for Sprint 7 RDE tunnel messages — do NOT define them here. Never reuse 1–6.

---

### 2. `proto/connector/v1/connector.proto`

Add three new messages and three new oneof variants.

**New messages** (add after existing `ResourceAckBatch` message):

```proto
message ShieldDiscoveryReport {
  string shield_id                 = 1;
  shield.v1.DiscoveryReport report = 2;
}

message ShieldDiscoveryBatch {
  repeated ShieldDiscoveryReport reports = 1;
}

message ScanTarget {
  string ip   = 1;
  uint32 port = 2;
}

message ScanCommand {
  string          request_id  = 1;  // UUID — client uses this to poll results
  repeated string targets     = 2;  // IPs or CIDRs (e.g. "192.168.1.0/24")
  repeated uint32 ports       = 3;  // ports to probe (max 16)
  uint32          max_targets = 4;  // hard cap, default 512
  uint32          timeout_sec = 5;  // per-target timeout, default 5, max 60
}

message ScanResult {
  string ip             = 1;
  uint32 port           = 2;
  string protocol       = 3;  // "tcp"
  string service_name   = 4;
  uint64 first_seen     = 5;  // unix timestamp
  string reachable_from = 6;  // connector_id that ran the scan
}

message ScanReport {
  string              request_id   = 1;
  repeated ScanResult results      = 2;
  string              error        = 3;  // set only on scan-level errors
}
```

**Modify `ConnectorControlMessage.oneof body`** — add after `pong = 7`:

```proto
// Connector → Controller
ShieldDiscoveryBatch shield_discovery = 8;
ScanReport           scan_report      = 9;
// Controller → Connector
ScanCommand          scan_command     = 10;
```

> **Rule:** fields 8, 9, 10 are the next available. Never reuse 1–7.

---

### 3. `controller/migrations/008_discovery.sql`

```sql
-- Shield-local service discovery
CREATE TABLE shield_discovered_services (
  shield_id    UUID    NOT NULL REFERENCES shields(id) ON DELETE CASCADE,
  protocol     TEXT    NOT NULL DEFAULT 'tcp',
  port         INTEGER NOT NULL CHECK (port > 0 AND port < 65536),
  bound_ip     TEXT    NOT NULL,
  service_name TEXT    NOT NULL DEFAULT '',
  first_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (shield_id, protocol, port)
);

CREATE INDEX idx_sds_shield_id ON shield_discovered_services (shield_id);

-- Connector network scan results
CREATE TABLE connector_scan_results (
  request_id   TEXT    NOT NULL,
  connector_id UUID    NOT NULL,
  ip           TEXT    NOT NULL,
  port         INTEGER NOT NULL,
  protocol     TEXT    NOT NULL DEFAULT 'tcp',
  service_name TEXT    NOT NULL DEFAULT '',
  first_seen   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (request_id, ip, port, protocol)
);

CREATE INDEX idx_csr_request_id   ON connector_scan_results (request_id);
CREATE INDEX idx_csr_first_seen   ON connector_scan_results (first_seen);
CREATE INDEX idx_csr_connector_id ON connector_scan_results (connector_id);
```

---

### 4. `controller/graph/discovery.graphqls`

```graphql
type DiscoveredService {
  shieldId:    ID!
  protocol:    String!
  port:        Int!
  boundIp:     String!
  serviceName: String!
  firstSeen:   String!
  lastSeen:    String!
}

type ScanResult {
  requestId:     String!
  ip:            String!
  port:          Int!
  protocol:      String!
  serviceName:   String!
  reachableFrom: String!
  firstSeen:     String!
}

extend type Query {
  getDiscoveredServices(shieldId: ID!): [DiscoveredService!]!
  getScanResults(requestId: String!): [ScanResult!]!
}

extend type Mutation {
  promoteDiscoveredService(shieldId: ID!, protocol: String!, port: Int!): Resource!
  triggerScan(connectorId: ID!, targets: [String!]!, ports: [Int!]!): String!
}
```

> `triggerScan` returns the `requestId` (UUID) the client uses to poll `getScanResults`.

---

## After Your Commit

Notify the team. They run:

```bash
buf generate                          # from repo root
cd controller && go generate ./graph/...
cd admin && npm run codegen
```

## Build Check

```bash
buf generate    # must be clean
cd controller && go build ./...
```
