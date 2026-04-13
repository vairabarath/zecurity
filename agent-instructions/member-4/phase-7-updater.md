# Phase 7 — Rust Connector: Auto-Updater

Implement the optional auto-update mechanism that fetches new releases from GitHub and safely replaces the binary.

---

## File to Create

```
connector/src/updater.rs
```

---

## Update Flow

1. If `AUTO_UPDATE_ENABLED=false` → return immediately
2. Random startup delay 0–3600s (prevent thundering herd)
3. Every `UPDATE_CHECK_INTERVAL_SECS` (default 86400):
   1. GET GitHub releases API → parse `tag_name`
   2. semver compare: latest > `env!("CARGO_PKG_VERSION")`? No → continue
   3. Download binary + `checksums.txt`
   4. Verify SHA-256 — mismatch → abort, binary unchanged
   5. Backup old binary → replace → `systemctl restart`
   6. Health check after 10s → success: remove backup. Failure: restore backup, restart, log rollback

---

## Important Rules

1. **Independent phase** — can be done anytime, doesn't depend on other phases.
2. **SHA-256 verification is critical** — mismatch must abort without modifying the binary.
3. **Rollback must work** — if the new binary fails health check, restore the backup immediately.

---

## Phase 7 Checklist

```
✓ updater.rs implements full update flow
✓ AUTO_UPDATE_ENABLED check at entry
✓ Random startup delay (0–3600s)
✓ GitHub releases API polled correctly
✓ semver comparison works
✓ Binary + checksums downloaded
✓ SHA-256 verification passes
✓ Backup created before replacement
✓ systemctl restart triggered
✓ Health check + rollback logic implemented
✓ Committed and pushed
```

---

## After This Phase

Then proceed to Phase 8 (main entry point).
