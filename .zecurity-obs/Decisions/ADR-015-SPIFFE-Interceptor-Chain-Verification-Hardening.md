---
type: decision
status: proposed
date: 2026-06-25
related:
  - "[[CodeStudy/07-Connector-Audit]]"
  - "[[Decisions/ADR-011-Connector-Enrollment-Lifecycle-Hardening]]"
tags:
  - adr
  - controller
  - mtls
  - spiffe
  - authentication
  - defense-in-depth
---

# ADR-014 — SPIFFE Interceptor Chain Verification Hardening

## Context

Stage 12 of the connector flow audit (gRPC TLS handshake + SPIFFE
interceptor) surfaced an **architectural defense gap** in the gRPC
unary and stream SPIFFE interceptors. The gap is **not exploitable
today**, but it is the canonical "silent regression on next feature
PR" shape — the kind of bug that takes a thorough cross-file read to
discover and a single careless commit to weaponize.

### The gap

The controller's gRPC server presents its server certificate and
**requests** (but does not require) client certificates via
`ClientAuth: tls.RequestClientCert`. There are no `ClientCAs` set, so
Go's TLS stack performs **no validation** of client certs. All
validation is delegated to the SPIFFE interceptor.

In [`controller/internal/connector/spiffe.go`](controller/internal/connector/spiffe.go),
both the unary interceptor (line ~202) and the stream interceptor
(line ~529, in `control_stream.go`) gate the cert-chain verification
on **role equality with `connector`**:

```go
if role == appmeta.SPIFFERoleConnector {
    if err := verifyConnectorCertificate(ctx, store, trustDomain, leaf); err != nil {
        return nil, status.Errorf(codes.Unauthenticated,
            "connector certificate verification failed: %v", err)
    }
}
```

For any role other than `connector` (`shield`, `agent`, future
roles), the interceptor:

1. Extracts the leaf cert from the TLS peer info
2. Parses the SPIFFE URI from the cert
3. Validates the trust domain exists in the DB (workspace lookup)
4. **Skips** the cert chain verification against the workspace CA
5. Injects identity into context and calls the handler

The TLS layer also didn't verify the cert (RequestClientCert), so the
leaf cert is **never chain-validated** in this path.

### Concrete (non-)exploit chain — today

If a future RPC handler ever uses `spiffe.Role(ctx)` for
authorization with a value other than `connector`, an attacker can:

1. Generate a self-signed cert with URI SAN
   `spiffe://ws-victim.zecurity.in/<role>/<entity-id>` for any role
   they choose
2. Connect to the controller's gRPC server
3. Pass the TLS handshake (server doesn't validate client certs)
4. Pass `parseSPIFFEID` (URI is well-formed)
5. Pass `validator(trustDomain)` (victim's workspace exists in DB)
6. **Skip** `verifyConnectorCertificate` (role != "connector")
7. Reach the handler with attacker-controlled SPIFFE identity

This works **today** at the interceptor level — the only thing
saving the system is that no handler currently grants authorization
based on a non-connector role:

| RPC | Today's behavior |
|----|----|
| `ConnectorService.Enroll` | SPIFFE interceptor skipped (JWT auth) |
| `ShieldService.Enroll` | SPIFFE interceptor skipped (JWT auth) |
| `ShieldService.RenewCert` | **Explicitly rejects non-connector role** ([renewal.go:33](controller/internal/shield/renewal.go#L33)) |
| `ShieldService.Control` | Not implemented — returns Unimplemented |
| `ShieldService.Goodbye` | Not implemented — returns Unimplemented |
| `ClientService.*` | Interceptor entirely skipped, handlers do JWT auth |
| All other connector RPCs | Use connector role, which IS verified |

So today: **not exploitable**. The defense-in-depth at each handler
holds the line where the interceptor doesn't.

### Why it still needs fixing now, before any handler addition

This is a textbook latent vulnerability:

- **Discovery requires cross-file analysis.** Reading the interceptor
  alone doesn't reveal the risk; one must also enumerate every handler
  and verify that none uses a non-connector role for authorization.
- **A single innocent PR weaponizes it.** Implementing
  `ShieldService.Control` and authorizing via `spiffe.Role(ctx) == "shield"`
  — a perfectly natural choice — opens a tenant-impersonation hole.
- **No test would catch it.** Tests for the new handler would use
  legitimately-issued shield certs; no test exercises self-signed
  attacker certs.
- **The interceptor's own comment is wrong.** It lists context roles
  as "connector, agent, or controller" — `shield` is missing, which
  suggests shield support was added without revisiting the trust model.

Fixing this before the next handler PR makes the architecture
**deny-by-default** rather than **delegate-to-handler**, removing the
foot-gun without forcing every future handler author to remember the
rule.

## Decision

Hoist cert chain verification out of the `role == connector`
conditional. Verify the chain for **every** authenticated SPIFFE
client cert. Role-specific authorization remains in handlers; trust
establishment becomes universal.

### Proposed implementation (~15 LOC, both interceptors)

Rename `verifyConnectorCertificate` → `verifyClientCertificate` (or
similar role-neutral name), and call it unconditionally:

```go
// In spiffe.go (UnarySPIFFEInterceptor) AND
// control_stream.go (StreamSPIFFEInterceptor):

trustDomain, role, entityID, err := parseSPIFFEID(leaf)
if err != nil {
    return nil, status.Errorf(codes.Unauthenticated, "invalid SPIFFE ID: %v", err)
}

if !validator(ctx, trustDomain) {
    return nil, status.Errorf(codes.PermissionDenied, "trust domain %q not accepted", trustDomain)
}

// Universal chain verification — every client cert must chain to
// the workspace CA for the trust domain claimed in its SPIFFE URI.
// Role-specific authorization decisions live in handlers; trust
// establishment is universal.
if err := verifyClientCertificate(ctx, store, trustDomain, leaf); err != nil {
    return nil, status.Errorf(codes.Unauthenticated,
        "certificate verification failed: %v", err)
}

spiffeID := "spiffe://" + trustDomain + "/" + role + "/" + entityID
ctx = spiffe.WithIdentity(ctx, spiffeID, role, entityID, trustDomain)
return handler(ctx, req)
```

And update the function itself ([spiffe.go:218-244](controller/internal/connector/spiffe.go#L218-L244)):

```go
// verifyClientCertificate confirms the leaf cert chains to the
// workspace CA owning the SPIFFE trust domain. Role-agnostic.
// Callers that require a specific role check spiffe.Role(ctx)
// after this function returns successfully.
func verifyClientCertificate(ctx context.Context, store WorkspaceStore,
    trustDomain string, leaf *x509.Certificate) error {

    workspaceCA, err := store.GetWorkspaceCAByTrustDomain(ctx, trustDomain)
    if err != nil {
        return fmt.Errorf("load workspace CA: %w", err)
    }
    if workspaceCA == nil {
        return fmt.Errorf("workspace CA not found for trust domain %q", trustDomain)
    }

    roots := x509.NewCertPool()
    roots.AddCert(workspaceCA)

    if _, err := leaf.Verify(x509.VerifyOptions{
        Roots:     roots,
        KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
    }); err != nil {
        return fmt.Errorf("verify leaf against workspace CA: %w", err)
    }

    return nil
}
```

### What changes in trust semantics

Before: "trust the role; if role==connector, also verify the chain."

After: "verify the chain for every client cert; then trust the
SPIFFE identity it asserts." This matches how every other mTLS
deployment with SPIFFE works (SPIRE, Istio, etc.) — chain validation
is the foundation, role is just an attribute carried in the URI.

### Update the stale comment

The interceptor docstring at [spiffe.go:145-148](controller/internal/connector/spiffe.go#L145-L148)
lists context roles as "connector, agent, or controller" — add
`shield` and `client` to match reality.

## Alternatives Considered

### Alt A — Do nothing; rely on per-handler role checks (rejected)

Trust the discipline of every handler author to either:
- Explicitly require `connector` role, or
- Implement their own chain verification

**Rejected**: this is exactly what got us into the gap. The
architecture should make the safe choice by default, not require every
handler PR to remember a non-obvious rule.

### Alt B — Switch to `tls.RequireAndVerifyClientCert` with a static ClientCAs pool (rejected)

Move chain validation from the interceptor to the TLS handshake by
setting `ClientCAs` to a pool of all workspace CAs.

**Rejected**:
- Workspace CAs are created dynamically per tenant; a static pool
  doesn't fit
- Connectors enrolling for the first time present no cert — TLS
  layer would reject before the JWT-auth Enroll handler runs
- The current dynamic per-workspace-CA verification in the
  interceptor is correct; just needs to fire for all roles, not
  just connector

### Alt C — Require shield/agent role checks in every future handler (rejected)

Document a convention: every new RPC handler must explicitly check
the role and ignore identities with mismatched roles.

**Rejected for v1**: same discipline problem as Alt A. Code-level
default beats process-level discipline.

### Alt D — Defer until first non-connector-role handler is added (rejected)

Wait until a real shield/agent handler is needed; fix together.

**Rejected**: the bug becomes a P0 the moment the new handler is
authored, not when it ships. PR review pressure plus "the
interceptor handles auth, right?" thinking will let it through.
Better to close the gap proactively, when there's no schedule
pressure.

## Consequences

### Wins

- Chain verification becomes **deny-by-default** rather than
  **opt-in-per-role**
- Future role additions (`shield`, `agent`, anything Sprint 8+
  invents) inherit chain verification without code changes
- Architectural alignment with industry-standard SPIFFE
  implementations (SPIRE, Istio)
- One less foot-gun for handler authors

### Costs

- ~15 LOC change across two files (`spiffe.go`,
  `control_stream.go`)
- One DB lookup (`GetWorkspaceCAByTrustDomain`) per RPC where one
  was previously skipped for non-connector roles — negligible (the
  same lookup already runs for connector traffic, which is the
  bulk of inbound RPCs)
- Function rename touches one private function; no public API change

### Risks

- **Risk**: an existing non-connector path that we didn't audit
  starts failing because its cert genuinely doesn't chain to a
  workspace CA. Mitigation: today there are no such paths (verified
  by enumerating handlers — see table in Context); ClientService is
  skipped entirely at the interceptor and is unaffected.
- **Risk**: rejecting otherwise-valid client certs that lack
  workspace CA chains. Mitigation: same as above — no current
  legitimate caller would fail this check.

## Plan

### Phase 1 — Mechanical refactor

1. Rename `verifyConnectorCertificate` → `verifyClientCertificate`
   in `controller/internal/connector/spiffe.go`
2. Hoist the call out of the `if role == ...` conditional in both
   `UnarySPIFFEInterceptor` and `StreamSPIFFEInterceptor`
3. Update the docstring at the top of `UnarySPIFFEInterceptor` to
   list all current roles (add `shield`, `client`)

### Phase 2 — Verification

- `go vet ./...` clean
- `go test ./internal/connector/...` passes
- Manual smoke: existing connector enrollment + heartbeat works
  (positive control)
- Add a test that submits a self-signed cert with a shield SPIFFE
  URI to any non-Enroll RPC and confirms it's rejected with
  `Unauthenticated` (negative control)

## Verification

- `go vet ./...` clean
- All existing connector tests pass
- New negative test (self-signed shield-URI cert) returns
  `Unauthenticated` instead of reaching a handler
- Manual smoke test of the connector control stream + heartbeat
  flows confirms no legitimate path broke

## Notes

- This ADR addresses **the controller-side chain verification gap
  only**. The TLS server config staying as
  `tls.RequestClientCert` (no `ClientCAs`) is correct — the dynamic
  per-workspace CA validation in the interceptor is intentional and
  better-suited to a multi-tenant CA hierarchy than a static cert
  pool would be.
- Out of scope:
  - Shield enrollment flow audit (will be its own deliverable)
  - The current `ClientService` JWT-based bypass (Sprint 7
    end-user device flow)
  - Pre-launch hardening items deferred to rate-limiting work
- Closes Stage 12 audit finding **STAGE12-F1** in
  [[CodeStudy/07-Connector-Audit]].
