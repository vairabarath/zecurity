# Phase 7 — Wiring Bootstrap into the Service

Coordinate across Member 2, Member 3, and Member 4 to wire the bootstrap service into the application.

---

## Overview

Member 2's `callback.go` calls `bootstrap.Bootstrap()` as a plain function.
But `Bootstrap` is now a method on `bootstrap.Service` which needs a `Pool` and `PKIService`.
Member 3 must expose a `NewService` constructor and update the function signature.

This phase requires coordination across all three members before merging.

---

## Step 1: Update `controller/internal/auth/config.go`

Member 2 updates `auth.Config` to include the bootstrap service:

```go
type Config struct {
    // ...existing fields...
    BootstrapService *bootstrap.Service  // Member 3 provides this
}
```

---

## Step 2: Update `controller/cmd/server/main.go`

Member 4 updates `main.go` to wire the bootstrap service:

```go
// After pki.Init and db.Init are called:

bootstrapSvc := &bootstrap.Service{
    Pool:       db.Pool,
    PKIService: pkiService,
}

authSvc := auth.NewService(auth.Config{
    BootstrapService: bootstrapSvc,
    // ...other config fields...
})
```

---

## Step 3: Update `controller/internal/auth/callback.go`

Member 2 updates the call site in `callback.go`:

**Before:**
```go
result, err := bootstrap.Bootstrap(ctx, email, "google", sub, name)
```

**After:**
```go
result, err := s.bootstrapSvc.Bootstrap(ctx, email, "google", sub, name)
```

where `s.bootstrapSvc` is the bootstrap service injected into the auth service via `auth.Config`.

---

## Dependency Map

```
Phase 7 requires:
  ✓ Phase 2 — pki.Service interface defined
  ✓ Phase 5 — WorkspaceCA generation working
  ✓ Phase 6 — Bootstrap transaction implemented
  ✓ Member 2 — auth.Config updated with BootstrapService field
  ✓ Member 4 — main.go wires bootstrap.Service
  
Coordination needed:
  - Member 2 updates callback.go to use bootstrapSvc.Bootstrap()
  - Member 4 updates main.go to wire bootstrap.Service
  - All three agree on the updated auth.Config fields
  - Full flow test: login → bootstrap → workspace active → JWT issued
```

---

## Verification Checklist

```
[ ] bootstrap.Service wired in main.go with Pool + PKIService
[ ] auth.Config updated with BootstrapService field
[ ] Member 2's callback.go updated to call bootstrapSvc.Bootstrap()
[ ] Full flow test: login → bootstrap → workspace active → JWT issued
[ ] No compilation errors after wiring changes
[ ] All existing auth tests still pass
```
