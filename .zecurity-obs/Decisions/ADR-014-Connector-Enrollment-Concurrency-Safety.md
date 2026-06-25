---
type: decision
status: proposed
date: 2026-06-23
related:
  - "[[CodeStudy/07-Connector-Audit]]"
  - "[[Decisions/ADR-011-Connector-Enrollment-Lifecycle-Hardening]]"
tags:
  - adr
  - connector
  - rust
  - enrollment
  - concurrency
  - filesystem
---

# ADR-013 — Connector Enrollment Concurrency Safety

## Context

Stage 9 of the connector-enrollment audit surfaced a **concurrent
install race** in [enrollment.rs](connector/src/enrollment.rs). The
file-level operations (`connector.key`, `connector.crt`,
`workspace_ca.crt`, `state.json`) are not atomic across processes, and
the enrollment flow has no concurrency guard.

This is not exploitable by a remote attacker. It is a **customer
self-DoS edge case** that produces a silent, confusing failure mode:
"install reported success, but the connector doesn't work."

### The race

Customer runs the install command twice in parallel (two terminals,
tmux paste, retry-script-with-no-status-check, accidental double-click
plus repeat):

| T | Process A | Process B | Disk state |
|---|-----------|-----------|------------|
| 0ms | Parses JWT, fetches CA | — | — |
| 10ms | Generates keypair A | Parses JWT, fetches CA | — |
| 20ms | Writes `connector.key` = **key A** | Generates keypair B | `key=A` |
| 30ms | Builds CSR from in-memory keypair A | Writes `connector.key` = **key B** (overwrites!) | `key=B` |
| 100ms | Enroll RPC succeeds → cert C for `pub(A)` | Builds CSR from in-memory keypair B | `key=B` |
| 110ms | Writes `connector.crt` = cert for `pub(A)` | Enroll RPC fails (JTI burnt) | `key=B`, `cert=for pub(A)` |
| 115ms | Writes `state.json` | Exits with error | `key=B`, `cert=for pub(A)`, `state=A` |

**Result on disk**:
- `connector.key` = private key **B**
- `connector.crt` = certificate for public key **A**
- `state.json` = connector_id A

### What breaks

Next time the connector daemon starts (or heartbeat fires):
1. Loads `connector.key` from disk → gets key B
2. Presents `connector.crt` during mTLS to controller
3. TLS handshake fails: private key doesn't match certificate's public key
4. Connector logs cryptic TLS error and retries forever
5. Customer sees: *"I just installed it, why doesn't it work?"*

### Why this matters even though it's edge-case

- **Silent failure mode** — install reports success, daemon fails later. No clear diagnostic.
- **Support burden** — customers blame the product; debugging requires reading TLS handshake errors.
- **Onboarding promise** — for a ZTNA product, "the install just works" is part of the sales story.
- **Related operational case** — single-install-retry after a previously-failed attempt: even without concurrency, a stale `connector.key` on disk causes the same key/cert mismatch. The fix below covers both cases.

## Decision

Add a **lockfile + cleanup** guard at the start of `enroll()` in
[enrollment.rs](connector/src/enrollment.rs).

### Proposed implementation (~25 LOC)

```rust
use std::os::unix::fs::OpenOptionsExt;

pub async fn enroll(cfg: &ConnectorConfig) -> Result<EnrollmentState> {
    fs::create_dir_all(&cfg.state_dir)?;

    // Step 0a — Acquire enrollment lock (atomic O_EXCL at kernel level).
    let lock_path = Path::new(&cfg.state_dir).join(".enrolling");
    let _lock = match OpenOptions::new()
        .write(true)
        .create_new(true)         // O_CREAT | O_EXCL
        .mode(0o600)
        .open(&lock_path)
    {
        Ok(f) => LockGuard { path: lock_path.clone(), _file: f },
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
            bail!(
                "another enrollment is in progress (lockfile {}). \
                 If you're sure nothing else is running, remove it and retry.",
                lock_path.display()
            );
        }
        Err(e) => return Err(e.into()),
    };

    // Step 0b — Already enrolled? Don't re-enroll.
    if Path::new(&cfg.state_dir).join("state.json").exists() {
        bail!("connector already enrolled — remove state.json to force re-enrollment");
    }

    // Step 0c — Cleanup stale artifacts from any previously-failed
    // enrollment. Safe to do this INSIDE the lock — we're the only
    // enrollment running.
    for stale in ["connector.key", "connector.crt", "workspace_ca.crt"] {
        let path = Path::new(&cfg.state_dir).join(stale);
        if path.exists() {
            info!(path = %path.display(),
                  "removing stale file from prior failed enrollment");
            fs::remove_file(&path)
                .with_context(|| format!("failed to remove stale {}", path.display()))?;
        }
    }

    // ... rest of enrollment unchanged (parse JWT, fetch CA, generate
    // keypair, build CSR, Enroll RPC, save artifacts, save state.json) ...

    // LockGuard's Drop removes the lockfile when the function returns
    // (success OR Result::Err — but NOT on SIGKILL).
}

struct LockGuard {
    path: PathBuf,
    _file: fs::File,
}
impl Drop for LockGuard {
    fn drop(&mut self) {
        let _ = fs::remove_file(&self.path);
    }
}
```

### What this guarantees

| Scenario | Behavior |
|---|---|
| Single install, happy path | Works unchanged |
| Single install retried after previous failure | Lockfile is gone (Drop ran); stale `connector.key` cleaned up inside the lock; retry succeeds |
| Already-enrolled connector re-runs install | Early exit at `state.json` check — won't trash a working install |
| Two parallel installs (rare) | Second one fails immediately with a clear, actionable error message |
| Process crashes hard (SIGKILL, OOM-kill) | Stale lockfile remains; next retry needs manual `rm .enrolling`. Error message tells the customer exactly what to do. |
| Race window | Closed — `O_EXCL` is atomic at the kernel level |

## Alternatives Considered

### Alt A — Do nothing (accept the race)

**Rejected**. The race is rare but the failure mode is silent and
confusing. Even rare support tickets cost more than ~25 LOC of fix.

### Alt B — `O_EXCL` on `connector.key` only (5 LOC)

```rust
let file = OpenOptions::new()
    .create_new(true)
    .write(true)
    .open(&key_path)?;
```

**Pros**: minimum-viable fix. 5 LOC.

**Cons**:
- Doesn't self-heal after a previously-failed enrollment — customer
  must manually `rm connector.key` to retry, which is the operational
  case that hurts customers most.
- Doesn't guard the cert / CA / state files (smaller blast radius if
  they get corrupted, but still possible).

**Rejected**: the lockfile pattern (25 LOC) covers all cases at small
incremental cost.

### Alt C — Cleanup-then-create without a lock (the bad pattern)

```rust
if key_path.exists() {
    fs::remove_file(&key_path)?;
}
// then write key
```

**Rejected**: produces the same broken final state as Alt A in the
parallel case (delete + create is not atomic across processes), and
without the `state.json` guard, actively trashes already-enrolled
connectors when the install is accidentally re-run. Discussed in
detail during audit chat (2026-06-23).

### Alt D — Linux `flock()` system call

**Rejected**. Less portable, lock not released on hard kill (same as
lockfile approach), more complex than the lockfile pattern. No
meaningful advantage.

### Alt E — Single-instance daemon (one connector binary per host, registered with systemd)

**Rejected for the enrollment phase**. Systemd's `Type=oneshot` would
serialize enrollment runs but doesn't help when the customer runs the
install command (which invokes the binary directly outside systemd) in
parallel. The lockfile guard runs at the binary level, catching both
direct invocation and systemd activation.

## Consequences

### Wins

- Self-healing retries after a failed enrollment (the common case)
- Parallel-install race closed (the rare case)
- Already-enrolled state protected from accidental re-runs
- Clear, actionable error messages instead of silent corruption

### Costs

- ~25 LOC in `enroll()` + a `LockGuard` struct
- One additional file (`.enrolling`) in `state_dir` during enrollment
- New documentation note: "if a connector enrollment process is killed
  hard, the next install must manually `rm /var/lib/zecurity/.enrolling`"

### Risks

- **Stale lockfile from SIGKILL / OOM** — only affects retry after a
  hard kill. Mitigation: the error message tells the customer exactly
  what to do. Could be auto-cleaned in a future iteration by checking
  the lockfile's mtime or stored PID.
- **NFS / shared `state_dir` across hosts** — `O_EXCL` on NFS has had
  historical reliability issues. Not a normal deployment for a
  per-host connector. Document: `state_dir` must be local storage.

## Plan

1. Add `LockGuard` struct + lockfile acquisition at the top of `enroll()`
2. Add `state.json` existence check (early bail)
3. Add stale-file cleanup loop (under the lock)
4. Update install-script documentation: mention the lockfile path for
   support cases
5. Add unit test for the cleanup path (mock filesystem)

## Verification

- `cargo build` + `cargo clippy` clean
- `cargo test` passes
- Manual smoke:
  - Fresh install on clean directory → succeeds
  - Re-run install on same directory (with state.json present) →
    early-exit error mentioning state.json
  - Stop the connector mid-enrollment (SIGINT) → lockfile removed via
    Drop → retry succeeds
  - Stop with SIGKILL → lockfile remains → retry fails with clear
    "remove the file" message → manual cleanup → retry succeeds
  - Two install commands in parallel (`sh install.sh & sh install.sh`)
    → one succeeds, one fails immediately with lockfile error

## Notes

- Out of scope for this ADR:
  - **F1** (no HTTP timeout on `/ca.crt` fetch) — deferred; will be
    addressed during the connector rate-limiting / hardening pass
  - **F2** (no response body size limit) — deferred; same pass as F1
  - **CQ-9-1..4** (test coverage, parser duplication, magic constants)
    — chat-only hygiene
- This ADR is connector-side (Rust). Controller-side enrollment
  lifecycle concerns live in [[Decisions/ADR-011-Connector-Enrollment-Lifecycle-Hardening]].
- Closes Stage 9 audit follow-up: **F3** in [[CodeStudy/07-Connector-Audit]].
