# Phase 10 — GitHub Actions CI

Create the release workflow for building and publishing connector binaries.

---

## File to Create

```
.github/workflows/connector-release.yml
```

---

## Workflow Configuration

**Trigger:** push tag matching `connector-v*`

**Steps:**

1. Checkout
2. Install Rust stable + musl tools
3. `rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl`
4. Build release binaries for both targets
5. Rename to `connector-linux-amd64`, `connector-linux-arm64`
6. Generate `checksums.txt` with SHA-256
7. Create GitHub release from tag
8. Upload: binaries + checksums + `connector-install.sh`

---

## Important Rules

1. **Independent phase** — do this last, after all other phases are complete.
2. **Tag pattern must match** `connector-v*` to avoid triggering on non-connector tags.

---

## Phase 10 Checklist

```
✓ connector-release.yml created
✓ Trigger pattern matches connector-v*
✓ Rust stable + musl tools installed in workflow
✓ Both x86_64 and aarch64 targets built
✓ Binaries renamed correctly
✓ checksums.txt generated with SHA-256
✓ GitHub release created from tag
✓ All artifacts uploaded (binaries + checksums + install script)
✓ Committed and pushed
```

---

## After This Phase

All Member 4 phases are complete. 🎉
