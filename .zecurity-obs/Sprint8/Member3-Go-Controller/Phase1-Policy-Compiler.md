---
type: phase
status: pending
sprint: 8
member: M3
phase: Phase1-Policy-Compiler
depends_on:
  - M2-Phase1-Policy-Schema
tags:
  - go
  - controller
  - policy-engine
  - acl
---

# M3 Phase 1 — Policy CRUD + ACL Compiler

---

## What You're Building

Implement group/member/access-rule operations and compile them into ACL snapshots consumed by Connectors and Clients.

---

## Files to Create / Modify

### `controller/internal/policy/store.go`

Implement DB methods for:

- Create/update/delete groups.
- Add/remove group members.
- Enable/disable resource access rules.
- Query groups for a resource.
- Query users/devices allowed for a resource.

### `controller/internal/policy/compiler.go`

Implement:

```go
func CompileACLSnapshot(ctx context.Context, workspaceID string) (*ACLSnapshot, error)
```

Compiler flow:

```text
for each enabled resource rule:
  resource -> group -> users -> client_devices
  collect client device SPIFFE IDs
  emit ACL entry {resource_id, address, port, protocol, allowed_spiffe_ids[]}
```

### Compiler Output Contract

M4 must read this section before implementing connector-side policy helpers. M3 owns this contract and the compiler must satisfy it.

Snapshot invariants:

- Snapshot is scoped to exactly one `workspace_id`.
- `version` is monotonic per workspace whenever policy changes.
- `generated_at` is the Controller compile time as a Unix timestamp.
- Entries only appear for enabled `access_rules`.
- Disabled rules must not appear.
- Deleted resources/groups/users must not appear.
- Devices with revoked, blocked, or deleted state must not appear in `allowed_spiffe_ids`.
- Each entry represents one reachable resource tuple: `resource_id`, `address`, `port`, `protocol`.
- `protocol` values are lowercase and currently limited to `tcp` or `udp`.
- `allowed_spiffe_ids` contains client device SPIFFE IDs, not user IDs and not emails.
- Empty `allowed_spiffe_ids` is allowed and means deny for everyone for that resource entry.
- Duplicate `allowed_spiffe_ids` should be removed by the compiler.
- Missing resource entry means deny.
- Unknown client SPIFFE ID means deny.
- Missing or invalid snapshot means deny.

Connector enforcement contract:

```text
resource tuple exists AND client_spiffe_id is in allowed_spiffe_ids
  -> allow
otherwise
  -> deny
```

Do not rely on the Controller at tunnel time for access decisions. The local snapshot is the enforcement source for Connector hot-path checks.

### `controller/internal/policy/notifier.go`

Implement:

```go
func NotifyPolicyChange(ctx context.Context, workspaceID string) error
```

This should bump/invalidate the workspace policy version so Connectors receive the latest snapshot on heartbeat.

### `controller/internal/policy/cache.go`

Implement process-local snapshot caching. See [[Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching]].

```go
type SnapshotCache struct {
    mu      sync.RWMutex
    entries map[string]*ACLSnapshot
}

func (c *SnapshotCache) Get(workspaceID string) (*ACLSnapshot, bool)
func (c *SnapshotCache) Set(workspaceID string, snapshot *ACLSnapshot)
func (c *SnapshotCache) Invalidate(workspaceID string)
```

Rules:

- Cache key is `workspace_id`.
- Cache miss compiles from DB and stores the result.
- `NotifyPolicyChange(workspace_id)` invalidates after successful policy mutations.
- Compile failure returns no snapshot.
- Do not serve stale snapshots when compilation fails.

### ClientService `GetACLSnapshot`

In `controller/internal/client/service.go`:

- Verify access token.
- Verify `device_id` belongs to the user/workspace.
- Return current compiled snapshot.

### GraphQL/HTTP Resolvers

All policy mutations must call `NotifyPolicyChange(workspace_id)` after successful commit.

---

## Security Rules

- Default deny on compiler failures.
- Disabled access rules must not appear in snapshots.
- Devices with revoked/blocked state must not appear in `allowed_spiffe_ids`.
- Never trust client-supplied workspace/user IDs over verified JWT claims.

---

## Build Check

```bash
cd controller && go build ./...
```
