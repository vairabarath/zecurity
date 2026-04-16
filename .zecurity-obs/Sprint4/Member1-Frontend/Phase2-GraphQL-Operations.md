---
type: task
status: pending
sprint: 4
member: M1
phase: 2
depends_on:
  - "M3 Day 1: graph/shield.graphqls committed"
  - "M3 Day 1: graph/connector.graphqls modified"
  - "go generate ./graph/... run by M3"
unlocks:
  - Phase3-Shields-Page (full wiring)
  - Phase4-RemoteNetworks-Sidebar (NetworkHealth hook)
tags:
  - frontend
  - graphql
  - codegen
---

# M1 · Phase 2 — GraphQL Operations & Codegen

**Depends on: M3 Day 1 schema committed + `npm run codegen` run.**

---

## Goal

Write the GraphQL operation files (mutations + queries) and run codegen to generate TypeScript hooks for the Shields feature.

---

## Files to Create / Modify

| File | Action | Notes |
|------|--------|-------|
| `admin/src/graphql/mutations.graphql` | MODIFY | Add Shield mutations |
| `admin/src/graphql/queries.graphql` | MODIFY | Add Shield queries |
| Run `npm run codegen` | GENERATE | Produces TypeScript hooks |

---

## Checklist

### mutations.graphql — Add:

- [ ] `GenerateShieldToken` mutation:
```graphql
mutation GenerateShieldToken($remoteNetworkId: ID!, $shieldName: String!) {
  generateShieldToken(remoteNetworkId: $remoteNetworkId, shieldName: $shieldName) {
    shieldId
    installCommand
  }
}
```

- [ ] `RevokeShield` mutation:
```graphql
mutation RevokeShield($id: ID!) {
  revokeShield(id: $id)
}
```

- [ ] `DeleteShield` mutation:
```graphql
mutation DeleteShield($id: ID!) {
  deleteShield(id: $id)
}
```

### queries.graphql — Add:

- [ ] `GetShields` query:
```graphql
query GetShields($remoteNetworkId: ID!) {
  shields(remoteNetworkId: $remoteNetworkId) {
    id
    name
    status
    lastSeenAt
    version
    hostname
    publicIp
    interfaceAddr
    certNotAfter
    createdAt
    connectorId
  }
}
```

### Codegen

- [ ] Run `cd admin && npm run codegen`
- [ ] Verify generated hooks exist:
  - `useGenerateShieldTokenMutation`
  - `useRevokeShieldMutation`
  - `useDeleteShieldMutation`
  - `useGetShieldsQuery`
  - `ShieldStatus` enum type generated

---

## Build Check

```bash
cd admin && npm run codegen
# Should complete without errors
# Check admin/src/generated/ (or wherever codegen outputs) for Shield hooks
```

---

## Notes

- Run codegen once after Day 1 schema is committed for an initial pass.
- Re-run after any schema changes (M3 may iterate on `shield.graphqls`).
- The `installCommand` field in `ShieldToken` is shown **once** on generation — never stored in DB. The modal must copy it before dismissal.

---

## Related

- [[Sprint4/Member3-Go-DB-GraphQL/Phase1-DB-GraphQL-Schema]] — writes the schema this depends on
- [[Sprint4/Member1-Frontend/Phase3-Shields-Page]] — consumes the generated hooks
