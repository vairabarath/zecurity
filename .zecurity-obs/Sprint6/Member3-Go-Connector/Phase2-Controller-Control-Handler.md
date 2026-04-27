---
type: phase
status: done
sprint: 6
member: M3
phase: Phase2-Controller-Control-Handler
depends_on:
  - M3-B1 (resolvers)
  - M2-A1 (discovery store)
  - buf generate
tags:
  - go
  - controller
  - control-stream
---

# M3 Phase 2 — Controller Control Stream Handler

---

## What You're Building

MODIFY the controller's connector control stream handler to process two new incoming message types from connectors: `ShieldDiscoveryBatch` and `ScanReport`. Also add the background purge goroutine.

---

## File to Touch

### `controller/internal/connector/control.go` (MODIFY)

Locate the main `switch` (or `oneof` handler) where incoming `ConnectorControlMessage` types are processed.

**Add handler for `ShieldDiscoveryBatch` (field 8):**

```go
case *connectorpb.ConnectorControlMessage_ShieldDiscovery:
    batch := msg.ShieldDiscovery
    for _, report := range batch.Reports {
        shieldID := report.ShieldId
        r := report.Report

        if r.FullSync {
            // Replace all discovered services for this shield
            var services []discovery.DiscoveredService
            for _, svc := range r.Added {
                services = append(services, protoToDiscoveredService(shieldID, svc))
            }
            if err := discovery.ReplaceDiscoveredServices(ctx, s.db, shieldID, services); err != nil {
                log.Warnf("discovery: replace failed for shield %s: %v", shieldID, err)
            }
        } else {
            // Differential update
            var added, removed []discovery.DiscoveredService
            for _, svc := range r.Added {
                added = append(added, protoToDiscoveredService(shieldID, svc))
            }
            for _, svc := range r.Removed {
                removed = append(removed, discovery.DiscoveredService{
                    Protocol: svc.Protocol,
                    Port:     int(svc.Port),
                })
            }
            if err := discovery.UpsertDiscoveredServices(ctx, s.db, shieldID, added, removed); err != nil {
                log.Warnf("discovery: upsert failed for shield %s: %v", shieldID, err)
            }
        }
    }
```

**Add handler for `ScanReport` (field 9):**

```go
case *connectorpb.ConnectorControlMessage_ScanReport:
    rep := msg.ScanReport
    var results []discovery.ScanResult
    for _, r := range rep.Results {
        results = append(results, discovery.ScanResult{
            RequestID:   rep.RequestId,
            ConnectorID: connectorID,
            IP:          r.Ip,
            Port:        int(r.Port),
            Protocol:    r.Protocol,
            ServiceName: r.ServiceName,
        })
    }
    if err := discovery.UpsertScanResults(ctx, s.db, connectorID, results); err != nil {
        log.Warnf("discovery: scan upsert failed for request %s: %v", rep.RequestId, err)
    }
```

**Add helper:**

```go
func protoToDiscoveredService(shieldID string, svc *shieldpb.DiscoveredService) discovery.DiscoveredService {
    return discovery.DiscoveredService{
        ShieldID:    shieldID,
        Protocol:    svc.Protocol,
        Port:        int(svc.Port),
        BoundIP:     svc.BoundIp,
        ServiceName: svc.ServiceName,
    }
}
```

**Add background purge goroutine** (start from `cmd/server/main.go` or from the service init):

```go
// Purge scan results older than 24h, runs every hour
go func() {
    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()
    for range ticker.C {
        cutoff := time.Now().UTC().Add(-discoveryCfg.ScanResultTTL)
        if err := discovery.PurgeScanResults(context.Background(), db, cutoff); err != nil {
            log.Warnf("discovery: purge failed: %v", err)
        }
    }
}()
```

---

## Build Check

```bash
cd controller && go build ./...
```
