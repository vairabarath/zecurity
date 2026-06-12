---
type: phase
sprint: 10.1
member: M2
phase: 1
status: planned
---

# M2 Phase 1 — PKI Chain Audit & Contract

## What You're Building

Prove that the deployed PKI can validate Connector and Client chains from a
Workspace CA up to the Platform Intermediate CA. Fail closed when legacy stored
certificates have incompatible path constraints.

## Required Chain

```text
leaf → Workspace CA → Platform Intermediate CA
```

Current source generation must remain:

- Root CA: `MaxPathLen=2`
- Intermediate CA: `MaxPathLen=1`
- Workspace CA: `MaxPathLen=0`

## Files to Touch

- `controller/internal/pki/*_integration_test.go`
- `controller/internal/pki/service.go`
- `controller/internal/pki/` new chain-audit helper if needed
- `.zecurity-obs/Services/PKI.md`

## Requirements

1. Fix stale tests that still expect Root `MaxPathLen=1` or Intermediate
   `MaxPathLen=0`.
2. Issue Connector and Client leaves for at least two workspaces.
3. Verify each leaf using Intermediate as root and Workspace CA as intermediate.
4. Verify cross-workspace and unknown Workspace CA chains fail.
5. At PKI startup, inspect stored CA constraints and report a fatal actionable
   error if full-chain validation is impossible.
6. Do not silently regenerate production CA material.

## Build Check

```bash
cd controller
go test ./internal/pki/...
go build ./...
```

## Post-Phase Fixes

*(Empty)*
