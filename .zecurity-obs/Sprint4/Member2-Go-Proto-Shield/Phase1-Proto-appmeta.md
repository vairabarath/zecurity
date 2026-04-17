---
type: task
status: done
sprint: 4
member: M2
phase: 1
priority: DAY1-CRITICAL
depends_on: []
unlocks:
  - Everyone else (buf generate unblocks M3 + M4)
  - M3 resolvers (need appmeta constants)
  - M4 crate scaffold (needs shield.proto)
tags:
  - go
  - proto
  - appmeta
  - day1
---

# M2 · Phase 1 — Proto Files + appmeta (DAY 1 — COMMIT FIRST)

> **This is the most critical commit in Sprint 4.**
> Nothing else can proceed until these three files are committed and `buf generate` runs.
> Commit all three together in a single PR/push.

---

## Files to Create / Modify

| File | Action |
|------|--------|
| `proto/shield/v1/shield.proto` | CREATE |
| `proto/connector/v1/connector.proto` | MODIFY (add Goodbye + ShieldHealth) |
| `controller/internal/appmeta/identity.go` | MODIFY (add Shield constants) |

---

## Checklist

### 1. Create `proto/shield/v1/shield.proto`

- [ ] Create directory `proto/shield/v1/`
- [ ] Write proto with package `shield.v1`
- [ ] `option go_package = "github.com/vairabarath/zecurity/gen/go/proto/shield/v1;shieldv1";`
- [ ] `service ShieldService` with 4 RPCs: `Enroll`, `Heartbeat`, `RenewCert`, `Goodbye`
- [ ] All messages defined: `EnrollRequest`, `EnrollResponse`, `HeartbeatRequest`, `HeartbeatResponse`, `RenewCertRequest`, `RenewCertResponse`, `GoodbyeRequest`, `GoodbyeResponse`
- [ ] `EnrollResponse` includes: `certificate_pem`, `workspace_ca_pem`, `intermediate_ca_pem`, `shield_id`, `interface_addr`, `connector_addr`, `connector_id`
- [ ] `HeartbeatResponse` includes: `ok`, `latest_version`, `re_enroll`

> See sprint4-shield-plan.md for full proto content.

### 2. Modify `proto/connector/v1/connector.proto`

- [ ] Add `Goodbye` RPC to `ConnectorService`:
  ```protobuf
  rpc Goodbye(GoodbyeRequest) returns (GoodbyeResponse);
  ```
- [ ] Add `GoodbyeRequest` message: `{ string connector_id = 1; }`
- [ ] Add `GoodbyeResponse` message: `{ bool ok = 1; }`
- [ ] Add `ShieldHealth` message:
  ```protobuf
  message ShieldHealth {
    string shield_id         = 1;
    string status            = 2;
    string version           = 3;
    int64  last_heartbeat_at = 4;
  }
  ```
- [ ] Add `shields` field to `HeartbeatRequest`: `repeated ShieldHealth shields = 5;`
- [ ] **Do NOT remove or renumber existing fields.** Proto field numbers are permanent.

### 3. Modify `controller/internal/appmeta/identity.go`

- [ ] Add Shield constants block (after existing connector constants):
  ```go
  const (
      SPIFFERoleShield    = "shield"
      PKIShieldCNPrefix   = "shield-"
      ShieldInterfaceName = "zecurity0"
      ShieldInterfaceCIDR = "100.64.0.0/10"
  )
  ```
- [ ] Add `ShieldSPIFFEID()` function:
  ```go
  func ShieldSPIFFEID(trustDomain, shieldID string) string {
      return "spiffe://" + trustDomain + "/" + SPIFFERoleShield + "/" + shieldID
  }
  ```
- [ ] **Do NOT remove existing constants.** Connectors still use them.

### 4. Run buf generate (team step — anyone can do this after commit)

```bash
# From repo root
buf generate
# Verify: controller/gen/go/proto/shield/v1/ directory created
# Verify: controller/gen/go/proto/connector/v1/ updated with Goodbye + ShieldHealth
```

- [ ] Buf generate runs cleanly (no errors)
- [ ] `controller/gen/go/proto/shield/v1/shield.pb.go` exists
- [ ] `controller/gen/go/proto/shield/v1/shield_grpc.pb.go` exists
- [ ] Connector stubs updated with `GoodbyeRequest`, `ShieldHealth`
- [ ] `cd controller && go build ./...` passes (stubs compile, no import errors)

---

## Build Check

```bash
buf generate                          # From repo root
cd controller && go build ./...       # Must pass
```

---

## Notes

- The `buf.yaml` at repo root already has `roots: [proto]`. No change needed to buf.yaml.
- The `buf.gen.yaml` paths are: `out: controller/gen/go` with `paths=source_relative`.
- Field 5 (`shields`) in `HeartbeatRequest` was chosen to not conflict with future additions. Do not reorder.
- The connector proto's existing field numbers (1–4) must not change.

---

## Related

- [[Sprint4/path.md]] — dependency map
- [[Sprint4/Member3-Go-DB-GraphQL/Phase1-DB-GraphQL-Schema]] — parallel Day 1 work
- [[Sprint4/Member4-Rust-Shield-CI/Phase1-Crate-Scaffold]] — unblocked by this phase
