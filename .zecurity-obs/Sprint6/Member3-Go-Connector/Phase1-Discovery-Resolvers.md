---
type: phase
status: done
sprint: 6
member: M3
phase: Phase1-Discovery-Resolvers
depends_on:
  - M2-D1-D
  - M2-A1 (discovery store)
  - buf generate
  - go generate
tags:
  - go
  - graphql
  - resolvers
---

# M3 Phase 1 — Discovery GraphQL Resolvers

---

## What You're Building

Two resolver files handling all discovery-related GraphQL operations.

---

## Files to Touch

### 1. `controller/graph/resolvers/discovery.resolvers.go` (NEW)

Implement all four operations from `discovery.graphqls`:

**`GetDiscoveredServices(shieldId)`**
- Call `discovery.GetDiscoveredServices(ctx, r.DB, shieldId)`
- Map results → `[]*model.DiscoveredService` using `toDiscoveredServiceGQL()`
- Return empty slice (not nil) when no results

**`GetScanResults(requestId)`**
- Call `discovery.GetScanResults(ctx, r.DB, requestId)`
- Map results → `[]*model.ScanResult` using `toScanResultGQL()`

**`PromoteDiscoveredService(shieldId, protocol, port)`**
- Fetch the `shield_discovered_services` row to get `bound_ip` and `service_name`
- Use `bound_ip` as the resource host — same auto-match-by-lan-ip logic as Sprint 5 `CreateResource`
- Create resource row with: `name = "<ServiceName> on <shieldId[:8]>"`, `host = bound_ip`, `protocol = protocol`, `port_from = port`, `port_to = port`, `shield_id = shieldId`, `status = "pending"`
- Return created `Resource`

**`TriggerScan(connectorId, targets, ports)`**
- Validate: targets not empty, len(ports) ≤ 16, targets ≤ 512 IPs when expanded
- Generate a `requestId` UUID
- Look up the connector's active Control stream sender (from the connector registry / control hub)
- Send `ConnectorControlMessage{ScanCommand: &pb.ScanCommand{RequestId: requestId, Targets: targets, Ports: ports32, MaxTargets: 512, TimeoutSec: 5}}`
- Return `requestId` as string

> For `TriggerScan` the resolver needs access to the connector control stream hub. Pass it in via the `Resolver` struct (same pattern as existing resolvers).

---

### 2. `controller/graph/resolvers/helpers.go` (MODIFY)

Add two mapper functions:

```go
func toDiscoveredServiceGQL(s discovery.DiscoveredService) *model.DiscoveredService {
    return &model.DiscoveredService{
        ShieldID:    s.ShieldID,
        Protocol:    s.Protocol,
        Port:        s.Port,
        BoundIp:     s.BoundIP,
        ServiceName: s.ServiceName,
        FirstSeen:   s.FirstSeen.Format(time.RFC3339),
        LastSeen:    s.LastSeen.Format(time.RFC3339),
    }
}

func toScanResultGQL(r discovery.ScanResult) *model.ScanResult {
    return &model.ScanResult{
        RequestID:     r.RequestID,
        IP:            r.IP,
        Port:          r.Port,
        Protocol:      r.Protocol,
        ServiceName:   r.ServiceName,
        ReachableFrom: r.ReachableFrom,
        FirstSeen:     r.FirstSeen.Format(time.RFC3339),
    }
}
```

---

## Build Check

```bash
cd controller && go build ./...
```

---

## Post-Phase Fixes (Applied After Sprint 6)

**Note:** No specific fixes were applied to this phase's files. The resolver implementation was already correct. Fixes were primarily applied to:
- Member4 (Shield): Network setup non-fatal, IPv6 parsing
- Member3 (Connector): Scan loop early exit, logging enhancements (Phase2 and Phase3)
- Member1 (Frontend): New ResourceDiscovery page
