---
type: phase
member: M3
sprint: 15
phase: 3
title: Posture-Aware Auth Path + Observability
status: done
depends_on: [Phase2-ACL-Diff-Teardown]
tags: [rust, connector, posture, observability, pending-08]
---

# Phase 3 — Posture-Aware Auth Path + Observability

> Depends on Phase 2, and on M1-E (controller ACL compiler hook) for the ACL to
> actually carry posture information.

## Goal

Confirm posture gating requires no separate connector-side check (it should fall out of
the existing ACL-membership check for free), and add the observability the sprint's
acceptance criteria require.

## Files

| File | Change |
|------|--------|
| `connector/src/device_tunnel.rs` | confirm-only (no new gating logic expected) |
| `connector/src/*` | metrics/logging additions |

## Posture-Aware Auth Path

- In enforce mode, the controller's `CompileACLSnapshot` (M1-E3) already excludes a
  non-compliant device's SPIFFE ID from a resource's `allowed_spiffe_ids`. The
  connector's existing per-resource ACL check (`device_tunnel.rs`, the same check that
  gates on group membership today) therefore **already** rejects a posture-non-compliant
  device at connect time — no new connector-side posture logic is needed.
- Explicitly verify this with an integration test (device with a failing enforce-mode
  profile attempts a new tunnel → rejected by the existing ACL check, not a new one).
- Confirm Phase 2's diff-and-abort also fires when the *cause* of the ACL shrinkage is a
  posture evaluation transition (not just a group/device revoke) — the mechanism is
  cause-agnostic, but this test proves the whole chain end-to-end.

## Observability

**The connector has no existing Rust metrics framework today.** Do not assume one is
already wired in. Pick one of:
- (a) Explicitly add a metrics crate (e.g. `metrics` + an exporter) as a scoped
  sub-task of this phase, with its own small design (what exporter, what's already
  standard for the Go controller side, if anything comparable exists to mirror), or
- (b) Limit this phase to **structured `tracing` logs only** and defer a metrics crate
  to a future sprint.
Default to (b) unless there's a concrete reason to take on the metrics-crate work now —
it's out of scope for what this sprint actually needs to prove (that the mechanism
works), and (a) would expand this phase's surface significantly.

Structured logs (no raw posture values, no PII beyond device/workspace/resource IDs):

- Registry size (active session count) — log periodically or on significant change, as
  a cheap leak detector for Phase 1.
- One structured log line per cancellation fired via ACL-diff teardown:
  `(spiffe_id, resource_id, reason: "acl_diff", transport: "tcp"|"quic"|"relay")` — the
  `transport` label matters because it's the fastest way to notice if the Phase 2
  relay-child-task fix regresses (relay-routed cancellations should track roughly with
  direct ones, not lag or go missing).
- Report accept/reject visibility is primarily M1's concern (the connector doesn't see
  raw reports) — skip duplicating that here.

## Tests

- Integration: enforce-mode-failing device's new tunnel attempt is rejected by the
  existing ACL check with no code changes needed to prove it.
- Integration: posture transition (pass→fail under enforce, driven by M1) results in a
  new snapshot, which results in Phase 2's cancellation firing — full chain, not just
  the connector's half.
- Unit: registry-size log/gauge updates correctly on registry insert/remove.
- Unit: the transport label on a cancellation log line is set correctly for TCP, QUIC, and relay-routed tunnels.

## Build Check
```bash
cd connector && cargo build && cargo test
```

## Implementation Checklist
- [x] **M3-H1** Integration test confirming posture gating requires no new connector-side check — falls out of the existing ACL membership check.
- [x] **M3-H2** Structured logs: registry size, cancellations fired (labeled by transport). Add a metrics crate only if explicitly scoped as a sub-task — default to logs-only.
- [x] **Build gate:** `cd connector && cargo build && cargo test`

## Post-Phase Fixes

### Fix: CRL / relay-CRL URLs fail with "builder error" when controller HTTP addr has no scheme
**File:** `connector/src/enrollment.rs`, `connector/src/main.rs`, `connector/src/enrollment_tests.rs`
**Issue:** On a connector whose config had `CONTROLLER_HTTP_ADDR=192.168.1.164:8080` (bare `host:port` — the documented default for co-located/dev installs), the CRL refresh loop logged `CRL refresh failed: CRL fetch error: builder error` every ~60s and the CRL cache never populated. Because the connector is fail-closed, every device tunnel was denied with `certificate revocation state unavailable` (observed in `device_tunnel.rs`'s `RevocationStatus::Unavailable` branch while validating the Sprint 15 posture-auth path).

**Root Cause:** `enrollment.rs` normalizes a bare `host:port` to `http://host:port` when building the `/ca.crt` URL (`ca_cert_url`), but `main.rs` built the `/ca.crl` and `/relay.crl` URLs directly from the raw `http_base` via `format!("{}/ca.crl?...", http_base.trim_end_matches('/'))`. `reqwest::get` rejects the resulting scheme-less URL (its error displays as "builder error"), so the CRL refresh always failed.

**Fix Applied (enrollment.rs, line ~316):**
```rust
// BEFORE:
fn ca_cert_url(http_addr: &str) -> String {
    if http_addr.starts_with("http://") || http_addr.starts_with("https://") {
        format!("{}/ca.crt", http_addr.trim_end_matches('/'))
    } else {
        format!("http://{}/ca.crt", http_addr)
    }
}

// AFTER:
pub fn http_base_url(http_addr: &str) -> String {
    if http_addr.starts_with("http://") || http_addr.starts_with("https://") {
        http_addr.trim_end_matches('/').to_string()
    } else {
        format!("http://{}", http_addr.trim_end_matches('/'))
    }
}

fn ca_cert_url(http_addr: &str) -> String {
    format!("{}/ca.crt", http_base_url(http_addr))
}
```

**Fix Applied (main.rs, line ~160):**
```rust
// BEFORE:
let crl_url = format!(
    "{}/ca.crl?workspace_id={}",
    http_base.trim_end_matches('/'),
    enrollment_state.workspace_id
);

// AFTER:
let http_base = enrollment::http_base_url(&http_base);
let crl_url = format!(
    "{}/ca.crl?workspace_id={}",
    http_base,
    enrollment_state.workspace_id
);
```

The same normalization now applies to the `/relay.crl` URL built right below it.

**Tests:** added `http_base_bare_host_port_defaults_to_http`, `http_base_explicit_scheme_is_preserved`, `http_base_trailing_slash_is_trimmed` to `enrollment_tests.rs` (`cargo test --lib enrollment`: 6 passed).

**Deployment note:** the running connector was also fixed immediately via config (`CONTROLLER_HTTP_ADDR=http://192.168.1.164:8080` in `/etc/zecurity/connector.conf`) + service restart, then the rebuilt binary replaced `/usr/local/bin/zecurity-connector`. Verified: no `builder error` after restart; `device_tunnel` logs `access allowed ... dest=192.168.1.42 port=5175 route="shield"` and the client logs `tunnel opened`.

