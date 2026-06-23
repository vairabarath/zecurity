---
type: decision
status: proposed
date: 2026-06-22
related:
  - "[[CodeStudy/07-Connector-Audit]]"
  - "[[Decisions/ADR-010-Controller-Code-Quality-Improvements]]"
  - "[[Decisions/ADR-011-Connector-Enrollment-Lifecycle-Hardening]]"
tags:
  - adr
  - controller
  - jwt
  - enrollment
  - tokens
  - observability
  - hygiene
---

# ADR-012 — Enrollment Token Hardening (Stage 7 Audit Follow-ups)

## Context

Stage 7 of the connector-enrollment flow audit (the JWT signing path in
[token.go](controller/internal/connector/token.go) and its shield twin
[shield/token.go](controller/internal/shield/token.go)) surfaced **four
medium-severity hardening items**. The cryptographic primitives are
sound — HMAC-SHA256, alg-none rejection, atomic JTI burn — but the
surrounding defensive boundary has gaps.

None are exploitable today. All are real correctness/operational gaps
that get harder to fix the longer the codebase grows around the current
shape.

Bundling four items here because they touch the same file
(`internal/connector/token.go`) and the same security-sensitive code
path. One PR. One review.

## Items

### 1. CQ-T1 — Add clock-skew leeway on JWT verification (🟡 Medium)

[token.go:65](controller/internal/connector/token.go#L65):

```go
}, jwt.WithIssuer(appmeta.ControllerIssuer), jwt.WithExpirationRequired())
```

No `jwt.WithLeeway(...)` configured. Default leeway is **0 seconds**.

**Symptom**: connector machines on bare metal / IoT often drift 1-5
seconds from NTP-synced time. Enrollments at the `exp` boundary fail
with a cryptic `token expired` error. The user sees no obvious cause —
the token is "fresh" by their wall clock but rejected.

**Fix** (1 LOC):

```go
}, jwt.WithIssuer(appmeta.ControllerIssuer),
   jwt.WithExpirationRequired(),
   jwt.WithLeeway(30*time.Second))
```

30 seconds is industry-standard tolerance. No security impact —
enrollment JTI is still single-use and burned in Redis.

Apply same change to [shield/token.go:142](controller/internal/shield/token.go#L142).

### 2. CQ-T2 — Add `aud` (audience) claim for JWT-type separation (🟡 Medium)

Three JWT types currently share `JWT_SECRET` + `Issuer`:

| JWT type | Claims struct | Separation mechanism today |
|----------|---------------|----------------------------|
| User session | `middleware.Claims` | Empty `sub/tenant_id/role` check |
| Connector enrollment | `connector.EnrollmentClaims` | JTI prefix `enrollment:jti:` |
| Shield enrollment | `shield.EnrollmentClaims` | JTI prefix `shield:enrollment:jti:` |

Cross-replay is blocked **only** by JTI-prefix lookup + downstream claim
emptiness checks. Defense-by-coincidence — the day someone:
- Unifies JTI handling into a shared module
- Adds a 4th JWT type (renewal, ACL push, anything)
- Refactors Redis key prefixes

…the cross-type attack opens.

Standard fix per RFC 7519 — explicit `aud` claim, verified at parse time.

**Fix**:

```go
// In appmeta/identity.go
const (
    AudienceConnectorEnroll = "connector-enroll"
    AudienceShieldEnroll    = "shield-enroll"
    AudienceUserSession     = "user-session"
)

// In connector/token.go GenerateEnrollmentToken:
claims := EnrollmentClaims{
    RegisteredClaims: jwt.RegisteredClaims{
        ...
        Audience: jwt.ClaimStrings{appmeta.AudienceConnectorEnroll},
    },
    ...
}

// In connector/token.go VerifyEnrollmentToken:
}, jwt.WithIssuer(appmeta.ControllerIssuer),
   jwt.WithExpirationRequired(),
   jwt.WithAudience(appmeta.AudienceConnectorEnroll))
```

~5 LOC per JWT type × 3 types = ~15 LOC total. Forward-compatible —
old tokens (without `aud`) would fail verification, so ship as part of
a coordinated controller release.

### 3. CQ-T3 — Structured logging for token operations (🟠 Medium-High)

`GenerateEnrollmentToken`, `VerifyEnrollmentToken`, `StoreEnrollmentJTI`,
`BurnEnrollmentJTI` — all four functions have **zero log statements**.

For a security-sensitive credential lifecycle, this means:
- **Forensics**: post-incident, no record of "which token was issued
  when, by which admin, for which connector"
- **Ops**: token-related errors (Redis failures, signing failures) are
  invisible unless callers log them
- **Audit/Compliance**: SOC 2 CC6.1, CC6.2 evidence requires this
  trail. ISO 27001 Annex A.9. Customer security questionnaires ask
  explicitly: *"How are credential issuance and revocation events
  logged?"*

This overlaps with [[STAGE4/5-F8]] (audit log gap) but is narrower:
F8 is about admin-action audit; CQ-T3 is about token-lifecycle
ops/forensics logging. Both needed.

**Fix** (~20 LOC):

```go
import "log"

func GenerateEnrollmentToken(...) (string, string, error) {
    ...
    if err != nil {
        log.Printf("token: sign failed connector=%s workspace=%s: %v",
            connectorID, workspaceID, err)
        return "", "", fmt.Errorf("sign enrollment token: %w", err)
    }
    log.Printf("token: issued connector=%s workspace=%s jti=%s ttl=%s",
        connectorID, workspaceID, jti, cfg.EnrollmentTokenTTL)
    return tokenString, jti, nil
}

func BurnEnrollmentJTI(...) (string, bool, error) {
    val, err := rdb.GetDel(...).Result()
    if errors.Is(err, valkeycompat.Nil) {
        log.Printf("token: burn miss jti=%s (expired or replayed)", jti)
        return "", false, nil
    }
    if err != nil { ... }
    log.Printf("token: burned jti=%s connector=%s", jti, val)
    return val, true, nil
}
```

Sensitive values: never log the token string itself (only the JTI).
The JTI is a UUID — safe to log; not a credential.

Apply same to shield twin.

### 4. CQ-T5 — Unit tests for `token.go` (🟡 Medium)

No `token_test.go` exists next to
[internal/connector/token.go](controller/internal/connector/token.go).
JWT signing/verification logic is currently covered only by integration
tests in `enrollment_test.go` — high-cost, slow, and they don't isolate
the JWT layer.

**The critical defenses that have NO direct test today**:
- `alg=none` rejection (the most famous JWT attack)
- `alg` confusion (RS256 → HS256) rejection
- Expiration enforcement
- Issuer enforcement
- Wrong-secret rejection
- JTI burn idempotency (second burn returns `found=false`)

A future refactor could silently break any of these. Without tests
asserting them, the alg-none defense is one careless commit away from
removal.

**Fix** (~80 LOC test file):

```go
// internal/connector/token_test.go
func TestGenerateVerify_RoundTrip(t *testing.T) { ... }
func TestVerify_RejectAlgNone(t *testing.T) { ... }
func TestVerify_RejectAlgConfusion(t *testing.T) { ... }
func TestVerify_RejectExpired(t *testing.T) { ... }
func TestVerify_RejectWrongIssuer(t *testing.T) { ... }
func TestVerify_RejectWrongSecret(t *testing.T) { ... }
func TestBurnJTI_AtomicSingleUse(t *testing.T) { ... }
func TestBurnJTI_MissReturnsFoundFalse(t *testing.T) { ... }
```

Use `miniredis` for the JTI tests (no external dep, already common in
Go test suites). After the F1 fix from ADR-011 lands (empty-claim
validation), add `TestVerify_RejectEmptyClaims` too.

Apply same test pattern to shield twin.

## Decision

Adopt all 4 items as a single "Stage 7 hardening PR" — same file
neighborhood, same security-sensitive concerns, single review burden.

Suggested order:

1. **CQ-T5** (tests) — write tests FIRST against current behavior,
   establishes the safety net for the changes below
2. **CQ-T1** (leeway) — 1 LOC, mechanical, immediate UX win
3. **CQ-T3** (logging) — mechanical, audit/forensics win
4. **CQ-T2** (audience) — most invasive, requires coordinated rollout
   (existing tokens lack `aud` — handle migration carefully)

CQ-T2 needs a 2-phase rollout:
- Phase 1: ship verifier with `aud` check **optional** (accept tokens
  missing `aud`); ship generator with `aud` always set
- Phase 2: after all in-flight tokens expire (24h TTL + safety margin),
  flip verifier to require `aud`

## Alternatives Considered

### Alt A — Skip CQ-T4 (connector/shield duplication) and CQ-T6 (signature smell)

**Adopted**: yes. These two items from the audit are bike-shed level —
pure aesthetics with no operational or security impact. Refactoring
~150 LOC of duplication takes more review time than the win justifies.
If a future PR is touching both files for another reason, fold it in
opportunistically. Otherwise leave alone.

### Alt B — Bundle into ADR-010 instead of a new ADR

**Rejected**. ADR-010 is controller-wide hygiene (HTTP server,
middleware, mux patterns). Stage 7 hardening is narrowly scoped to
JWT/token lifecycle code. Mixing them creates a single mega-PR that's
hard to review. Two focused ADRs (010 = HTTP/middleware,
012 = tokens) keep the review surfaces small.

### Alt C — Defer everything to "pre-GA" milestone

**Rejected for CQ-T1, CQ-T3, CQ-T5**:
- CQ-T1 is a 1-line fix that prevents real user-visible failures
- CQ-T3 logs are foundational — adding them after an incident is too late
- CQ-T5 tests get harder to write the more the surrounding code mutates

**Adopted for CQ-T2**: audience separation can wait until a 4th JWT type
is needed or until the first refactor of JTI handling — whichever
comes first.

## Consequences

### Wins

- Operational reliability (CQ-T1: no clock-skew enrollment failures)
- Forensics + compliance evidence (CQ-T3: token lifecycle is logged)
- Refactor safety net (CQ-T5: alg-none and other defenses regression-tested)
- Cross-JWT-type confusion blocked at the parse boundary (CQ-T2)

### Costs

- ~120 LOC across token.go (connector + shield) + new test files
- CQ-T2 requires coordinated rollout — see 2-phase plan
- Review burden on one moderately-sized PR

### Risks

- **CQ-T2 rollout risk**: if Phase 1 ships without backwards
  compatibility, in-flight tokens break. Mitigation: explicit phased
  rollout, opt-in `aud` check first, flip to required after TTL window.
- **CQ-T3 log volume**: 4 new log lines per enrollment. Negligible
  unless enrollment volume is high (it isn't — bursty admin action,
  not request-path).
- **CQ-T5 test brittleness**: tests asserting JWT library behavior
  could break on library upgrades. Mitigation: assert on observable
  behavior (rejection vs acceptance), not on error message strings.

## Plan

### Phase 1 — Tests-first

Land `token_test.go` (and shield twin) covering current behavior.
Establishes the safety net.

### Phase 2 — Mechanical hardening

CQ-T1 (leeway) + CQ-T3 (logging) in one PR. Low risk, no behavioral
change for normal cases.

### Phase 3 — Audience claim rollout (gated)

CQ-T2 in two PRs as described above. Don't combine with Phase 2 —
keep the migration risk isolated.

## Verification

- `go vet ./...` clean
- `go test ./internal/connector/... ./internal/shield/...` passes
- All new unit tests pass (alg-none, expiration, etc.)
- Manual smoke test:
  - Generate a connector enrollment token → use it within `exp - leeway`
    window → enrollment succeeds
  - Set system clock forward by 25 seconds → enrollment still succeeds
    (within leeway)
  - Set system clock forward by 60 seconds → enrollment rejected
    (outside leeway)
  - Tail controller logs during a token issue → confirm
    "token: issued connector=… jti=…" line
  - Tail controller logs during a duplicate Enroll call → confirm
    "token: burn miss jti=…" on second attempt
  - After CQ-T2: send a shield enrollment token to connector Enroll
    endpoint → expect `unexpected audience` error, not `not found`

## Notes

- This ADR explicitly leaves out:
  - **F1-F4** from the Stage 7 audit (4 Low-severity defensive gaps) —
    chat-only; not worth ADR overhead.
  - **CQ-T4** (connector/shield duplication) — bike-shed.
  - **CQ-T6** (return signature smell) — bike-shed.
- The cryptographic core (HMAC, signing method enforcement, atomic JTI)
  was audited and is correct — no changes proposed.
- Closes Stage 7 audit follow-ups for medium-severity hygiene.
