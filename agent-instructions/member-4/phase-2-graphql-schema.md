# Phase 2 — GraphQL Schema Update (DAY 1)

Add connector and remote_network types, queries, and mutations to the existing GraphQL schema. This unblocks **Member 1** (codegen) and **Member 3** (DB queries in handlers).

---

## File to Modify

```
controller/graph/schema.graphqls
```

**Do NOT remove or modify existing `me`, `workspace`, or `initiateAuth` definitions.**

---

## New Types to Add

```graphql
type RemoteNetwork {
  id:         ID!
  name:       String!
  location:   NetworkLocation!
  status:     RemoteNetworkStatus!
  connectors: [Connector!]!
  createdAt:  String!
}

enum NetworkLocation { HOME  OFFICE  AWS  GCP  AZURE  OTHER }
enum RemoteNetworkStatus { ACTIVE  DELETED }

type Connector {
  id:              ID!
  name:            String!
  status:          ConnectorStatus!
  remoteNetworkId: ID!
  lastSeenAt:      String
  version:         String
  hostname:        String
  publicIp:        String
  certNotAfter:    String
  createdAt:       String!
}

enum ConnectorStatus { PENDING  ACTIVE  DISCONNECTED  REVOKED }

type ConnectorToken {
  connectorId:    ID!
  installCommand: String!
}
```

---

## Add to Existing Query Type

```graphql
remoteNetworks: [RemoteNetwork!]!
remoteNetwork(id: ID!): RemoteNetwork
connectors(remoteNetworkId: ID!): [Connector!]!
```

---

## Add to Existing Mutation Type

```graphql
createRemoteNetwork(name: String!, location: NetworkLocation!): RemoteNetwork!
deleteRemoteNetwork(id: ID!): Boolean!
generateConnectorToken(remoteNetworkId: ID!, connectorName: String!): ConnectorToken!
revokeConnector(id: ID!): Boolean!
deleteConnector(id: ID!): Boolean!
```

---

## After Schema Update

After committing, run:

```bash
make gqlgen
```

This regenerates Go code. Tell **Member 1** to run `npm run codegen`.

---

## Phase 2 Checklist

```
✓ New types added to schema.graphqls
✓ Query extensions added
✓ Mutation extensions added
✓ No existing types modified
✓ make gqlgen runs successfully
✓ Committed and pushed — unblocks Member 1 codegen
```

---

## After This Phase

**Immediately commit and push.** Then notify:
- Member 1: "schema.graphqls updated, run npm run codegen"

Then proceed to Phase 3 (connector resolvers).
