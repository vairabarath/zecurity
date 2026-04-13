# Phase 1 — GraphQL Operation Files

## Objective

Create the connector-specific GraphQL operation files in the existing frontend GraphQL folder so they are ready for codegen once the backend schema supports them.

This phase is documentation-only for frontend contract setup. It does not require backend code changes and does not modify generated files.

---

## Prerequisites

- None. This phase can start immediately.

---

## Files to Create

```
admin/src/graphql/connector-mutations.graphql
admin/src/graphql/connector-queries.graphql
```

## Files to Modify

```
None
```

---

## Implementation

The repo already stores GraphQL operations under `admin/src/graphql/`, and frontend codegen is configured in `admin/codegen.yml` to scan `src/graphql/**/*.graphql`.

Follow the same pattern as the existing files:

- `admin/src/graphql/mutations.graphql`
- `admin/src/graphql/queries.graphql`

Create `connector-mutations.graphql` with:

```graphql
mutation CreateRemoteNetwork($name: String!, $location: NetworkLocation!) {
  createRemoteNetwork(name: $name, location: $location) {
    id
    name
    location
    status
    createdAt
  }
}

mutation DeleteRemoteNetwork($id: ID!) {
  deleteRemoteNetwork(id: $id)
}

mutation GenerateConnectorToken($remoteNetworkId: ID!, $connectorName: String!) {
  generateConnectorToken(remoteNetworkId: $remoteNetworkId, connectorName: $connectorName) {
    connectorId
    installCommand
  }
}

mutation RevokeConnector($id: ID!) {
  revokeConnector(id: $id)
}

mutation DeleteConnector($id: ID!) {
  deleteConnector(id: $id)
}
```

Create `connector-queries.graphql` with:

```graphql
query GetRemoteNetworks {
  remoteNetworks {
    id
    name
    location
    status
    createdAt
    connectors {
      id
      name
      status
      lastSeenAt
      version
      hostname
    }
  }
}

query GetConnectors($remoteNetworkId: ID!) {
  connectors(remoteNetworkId: $remoteNetworkId) {
    id
    name
    status
    lastSeenAt
    version
    hostname
    publicIp
    certNotAfter
    createdAt
  }
}
```

Keep these operations frontend-only. Do not edit backend schema files in this phase.

---

## Verification

- `admin/src/graphql/connector-mutations.graphql` exists
- `admin/src/graphql/connector-queries.graphql` exists
- Operation files live under `admin/src/graphql/`, matching the current codegen layout
- Existing GraphQL files are not modified unnecessarily

Optional later verification, once backend schema is ready:

```bash
cd admin && npm run codegen
```

---

## Do Not Touch

- Any file under `controller/`
- Any file under `connector/`
- `admin/src/generated/*`
- Apollo client setup in `admin/src/apollo/`

---

## After This Phase

Proceed to Phase 2: page scaffolds and routing.
