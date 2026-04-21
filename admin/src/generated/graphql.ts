/* eslint-disable */
import { TypedDocumentNode as DocumentNode } from '@graphql-typed-document-node/core';
export type Maybe<T> = T | null;
export type InputMaybe<T> = T | null | undefined;
export type Exact<T extends { [key: string]: unknown }> = { [K in keyof T]: T[K] };
export type MakeOptional<T, K extends keyof T> = Omit<T, K> & { [SubKey in K]?: Maybe<T[SubKey]> };
export type MakeMaybe<T, K extends keyof T> = Omit<T, K> & { [SubKey in K]: Maybe<T[SubKey]> };
export type MakeEmpty<T extends { [key: string]: unknown }, K extends keyof T> = { [_ in K]?: never };
export type Incremental<T> = T | { [P in keyof T]?: P extends ' $fragmentName' | '__typename' ? T[P] : never };
/** All built-in and custom scalars, mapped to their actual values */
export type Scalars = {
  ID: { input: string; output: string; }
  String: { input: string; output: string; }
  Boolean: { input: boolean; output: boolean; }
  Int: { input: number; output: number; }
  Float: { input: number; output: number; }
};

export type AuthInitPayload = {
  __typename?: 'AuthInitPayload';
  redirectUrl: Scalars['String']['output'];
  state: Scalars['String']['output'];
};

export type Connector = {
  __typename?: 'Connector';
  certNotAfter?: Maybe<Scalars['String']['output']>;
  createdAt: Scalars['String']['output'];
  hostname?: Maybe<Scalars['String']['output']>;
  id: Scalars['ID']['output'];
  lanAddr?: Maybe<Scalars['String']['output']>;
  lastSeenAt?: Maybe<Scalars['String']['output']>;
  name: Scalars['String']['output'];
  publicIp?: Maybe<Scalars['String']['output']>;
  remoteNetworkId: Scalars['ID']['output'];
  status: ConnectorStatus;
  version?: Maybe<Scalars['String']['output']>;
};

export enum ConnectorStatus {
  Active = 'ACTIVE',
  Disconnected = 'DISCONNECTED',
  Pending = 'PENDING',
  Revoked = 'REVOKED'
}

export type ConnectorToken = {
  __typename?: 'ConnectorToken';
  connectorId: Scalars['ID']['output'];
  installCommand: Scalars['String']['output'];
};

export type Mutation = {
  __typename?: 'Mutation';
  createRemoteNetwork: RemoteNetwork;
  deleteConnector: Scalars['Boolean']['output'];
  deleteRemoteNetwork: Scalars['Boolean']['output'];
  deleteShield: Scalars['Boolean']['output'];
  generateConnectorToken: ConnectorToken;
  generateShieldToken: ShieldToken;
  initiateAuth: AuthInitPayload;
  revokeConnector: Scalars['Boolean']['output'];
  revokeShield: Scalars['Boolean']['output'];
};


export type MutationCreateRemoteNetworkArgs = {
  location: NetworkLocation;
  name: Scalars['String']['input'];
};


export type MutationDeleteConnectorArgs = {
  id: Scalars['ID']['input'];
};


export type MutationDeleteRemoteNetworkArgs = {
  id: Scalars['ID']['input'];
};


export type MutationDeleteShieldArgs = {
  id: Scalars['ID']['input'];
};


export type MutationGenerateConnectorTokenArgs = {
  connectorName: Scalars['String']['input'];
  remoteNetworkId: Scalars['ID']['input'];
};


export type MutationGenerateShieldTokenArgs = {
  remoteNetworkId: Scalars['ID']['input'];
  shieldName: Scalars['String']['input'];
};


export type MutationInitiateAuthArgs = {
  provider: Scalars['String']['input'];
  workspaceName?: InputMaybe<Scalars['String']['input']>;
};


export type MutationRevokeConnectorArgs = {
  id: Scalars['ID']['input'];
};


export type MutationRevokeShieldArgs = {
  id: Scalars['ID']['input'];
};

export enum NetworkHealth {
  Degraded = 'DEGRADED',
  Offline = 'OFFLINE',
  Online = 'ONLINE'
}

export enum NetworkLocation {
  Aws = 'AWS',
  Azure = 'AZURE',
  Gcp = 'GCP',
  Home = 'HOME',
  Office = 'OFFICE',
  Other = 'OTHER'
}

export type Query = {
  __typename?: 'Query';
  connector?: Maybe<Connector>;
  connectors: Array<Connector>;
  lookupWorkspace: WorkspaceLookupResult;
  lookupWorkspacesByEmail: WorkspaceListResult;
  me: User;
  remoteNetwork?: Maybe<RemoteNetwork>;
  remoteNetworks: Array<RemoteNetwork>;
  shield?: Maybe<Shield>;
  shields: Array<Shield>;
  workspace: Workspace;
};


export type QueryConnectorArgs = {
  id: Scalars['ID']['input'];
};


export type QueryConnectorsArgs = {
  remoteNetworkId: Scalars['ID']['input'];
};


export type QueryLookupWorkspaceArgs = {
  slug: Scalars['String']['input'];
};


export type QueryLookupWorkspacesByEmailArgs = {
  email: Scalars['String']['input'];
};


export type QueryRemoteNetworkArgs = {
  id: Scalars['ID']['input'];
};


export type QueryShieldArgs = {
  id: Scalars['ID']['input'];
};


export type QueryShieldsArgs = {
  remoteNetworkId: Scalars['ID']['input'];
};

export type RemoteNetwork = {
  __typename?: 'RemoteNetwork';
  connectors: Array<Connector>;
  createdAt: Scalars['String']['output'];
  id: Scalars['ID']['output'];
  location: NetworkLocation;
  name: Scalars['String']['output'];
  networkHealth: NetworkHealth;
  shields: Array<Shield>;
  status: RemoteNetworkStatus;
};

export enum RemoteNetworkStatus {
  Active = 'ACTIVE',
  Deleted = 'DELETED'
}

export enum Role {
  Admin = 'ADMIN',
  Member = 'MEMBER',
  Viewer = 'VIEWER'
}

export type Shield = {
  __typename?: 'Shield';
  certNotAfter?: Maybe<Scalars['String']['output']>;
  connectorId: Scalars['ID']['output'];
  createdAt: Scalars['String']['output'];
  hostname?: Maybe<Scalars['String']['output']>;
  id: Scalars['ID']['output'];
  interfaceAddr?: Maybe<Scalars['String']['output']>;
  lanIp?: Maybe<Scalars['String']['output']>;
  lastSeenAt?: Maybe<Scalars['String']['output']>;
  name: Scalars['String']['output'];
  remoteNetworkId: Scalars['ID']['output'];
  status: ShieldStatus;
  version?: Maybe<Scalars['String']['output']>;
};

export enum ShieldStatus {
  Active = 'ACTIVE',
  Disconnected = 'DISCONNECTED',
  Pending = 'PENDING',
  Revoked = 'REVOKED'
}

export type ShieldToken = {
  __typename?: 'ShieldToken';
  installCommand: Scalars['String']['output'];
  shieldId: Scalars['ID']['output'];
};

export type User = {
  __typename?: 'User';
  createdAt: Scalars['String']['output'];
  email: Scalars['String']['output'];
  id: Scalars['ID']['output'];
  provider: Scalars['String']['output'];
  role: Role;
};

export type Workspace = {
  __typename?: 'Workspace';
  createdAt: Scalars['String']['output'];
  id: Scalars['ID']['output'];
  name: Scalars['String']['output'];
  slug: Scalars['String']['output'];
  status: WorkspaceStatus;
};

export type WorkspaceListResult = {
  __typename?: 'WorkspaceListResult';
  workspaces: Array<WorkspacePublic>;
};

export type WorkspaceLookupResult = {
  __typename?: 'WorkspaceLookupResult';
  found: Scalars['Boolean']['output'];
  workspace?: Maybe<WorkspacePublic>;
};

export type WorkspacePublic = {
  __typename?: 'WorkspacePublic';
  id: Scalars['ID']['output'];
  name: Scalars['String']['output'];
  slug: Scalars['String']['output'];
};

export enum WorkspaceStatus {
  Active = 'ACTIVE',
  Deleted = 'DELETED',
  Provisioning = 'PROVISIONING',
  Suspended = 'SUSPENDED'
}

export type InitiateAuthMutationVariables = Exact<{
  provider: Scalars['String']['input'];
  workspaceName?: InputMaybe<Scalars['String']['input']>;
}>;


export type InitiateAuthMutation = { __typename?: 'Mutation', initiateAuth: { __typename?: 'AuthInitPayload', redirectUrl: string, state: string } };

export type CreateRemoteNetworkMutationVariables = Exact<{
  name: Scalars['String']['input'];
  location: NetworkLocation;
}>;


export type CreateRemoteNetworkMutation = { __typename?: 'Mutation', createRemoteNetwork: { __typename?: 'RemoteNetwork', id: string, name: string, location: NetworkLocation, status: RemoteNetworkStatus, createdAt: string } };

export type DeleteRemoteNetworkMutationVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type DeleteRemoteNetworkMutation = { __typename?: 'Mutation', deleteRemoteNetwork: boolean };

export type GenerateConnectorTokenMutationVariables = Exact<{
  remoteNetworkId: Scalars['ID']['input'];
  connectorName: Scalars['String']['input'];
}>;


export type GenerateConnectorTokenMutation = { __typename?: 'Mutation', generateConnectorToken: { __typename?: 'ConnectorToken', connectorId: string, installCommand: string } };

export type RevokeConnectorMutationVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type RevokeConnectorMutation = { __typename?: 'Mutation', revokeConnector: boolean };

export type DeleteConnectorMutationVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type DeleteConnectorMutation = { __typename?: 'Mutation', deleteConnector: boolean };

export type GenerateShieldTokenMutationVariables = Exact<{
  remoteNetworkId: Scalars['ID']['input'];
  shieldName: Scalars['String']['input'];
}>;


export type GenerateShieldTokenMutation = { __typename?: 'Mutation', generateShieldToken: { __typename?: 'ShieldToken', shieldId: string, installCommand: string } };

export type RevokeShieldMutationVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type RevokeShieldMutation = { __typename?: 'Mutation', revokeShield: boolean };

export type DeleteShieldMutationVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type DeleteShieldMutation = { __typename?: 'Mutation', deleteShield: boolean };

export type LookupWorkspaceQueryVariables = Exact<{
  slug: Scalars['String']['input'];
}>;


export type LookupWorkspaceQuery = { __typename?: 'Query', lookupWorkspace: { __typename?: 'WorkspaceLookupResult', found: boolean, workspace?: { __typename?: 'WorkspacePublic', id: string, name: string, slug: string } | null } };

export type LookupWorkspacesByEmailQueryVariables = Exact<{
  email: Scalars['String']['input'];
}>;


export type LookupWorkspacesByEmailQuery = { __typename?: 'Query', lookupWorkspacesByEmail: { __typename?: 'WorkspaceListResult', workspaces: Array<{ __typename?: 'WorkspacePublic', id: string, name: string, slug: string }> } };

export type MeQueryVariables = Exact<{ [key: string]: never; }>;


export type MeQuery = { __typename?: 'Query', me: { __typename?: 'User', id: string, email: string, role: Role, provider: string, createdAt: string } };

export type GetWorkspaceQueryVariables = Exact<{ [key: string]: never; }>;


export type GetWorkspaceQuery = { __typename?: 'Query', workspace: { __typename?: 'Workspace', id: string, slug: string, name: string, status: WorkspaceStatus, createdAt: string } };

export type GetRemoteNetworksQueryVariables = Exact<{ [key: string]: never; }>;


export type GetRemoteNetworksQuery = { __typename?: 'Query', remoteNetworks: Array<{ __typename?: 'RemoteNetwork', id: string, name: string, location: NetworkLocation, status: RemoteNetworkStatus, createdAt: string, networkHealth: NetworkHealth, connectors: Array<{ __typename?: 'Connector', id: string, name: string, status: ConnectorStatus, lastSeenAt?: string | null, version?: string | null, hostname?: string | null, publicIp?: string | null, lanAddr?: string | null, certNotAfter?: string | null, createdAt: string }>, shields: Array<{ __typename?: 'Shield', id: string, name: string, status: ShieldStatus, lastSeenAt?: string | null, hostname?: string | null, interfaceAddr?: string | null }> }> };

export type GetConnectorsQueryVariables = Exact<{
  remoteNetworkId: Scalars['ID']['input'];
}>;


export type GetConnectorsQuery = { __typename?: 'Query', connectors: Array<{ __typename?: 'Connector', id: string, name: string, status: ConnectorStatus, lastSeenAt?: string | null, version?: string | null, hostname?: string | null, publicIp?: string | null, lanAddr?: string | null, certNotAfter?: string | null, createdAt: string }> };

export type GetConnectorQueryVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type GetConnectorQuery = { __typename?: 'Query', connector?: { __typename?: 'Connector', id: string, name: string, status: ConnectorStatus, lastSeenAt?: string | null, version?: string | null, hostname?: string | null, publicIp?: string | null, lanAddr?: string | null, certNotAfter?: string | null, createdAt: string, remoteNetworkId: string } | null };

export type GetRemoteNetworkQueryVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type GetRemoteNetworkQuery = { __typename?: 'Query', remoteNetwork?: { __typename?: 'RemoteNetwork', id: string, name: string, location: NetworkLocation, status: RemoteNetworkStatus } | null };

export type GetShieldsQueryVariables = Exact<{
  remoteNetworkId: Scalars['ID']['input'];
}>;


export type GetShieldsQuery = { __typename?: 'Query', shields: Array<{ __typename?: 'Shield', id: string, name: string, status: ShieldStatus, lastSeenAt?: string | null, version?: string | null, hostname?: string | null, interfaceAddr?: string | null, connectorId: string }> };

export type GetShieldQueryVariables = Exact<{
  id: Scalars['ID']['input'];
}>;


export type GetShieldQuery = { __typename?: 'Query', shield?: { __typename?: 'Shield', id: string, name: string, status: ShieldStatus, lastSeenAt?: string | null, version?: string | null, hostname?: string | null, lanIp?: string | null, interfaceAddr?: string | null, certNotAfter?: string | null, createdAt: string, connectorId: string, remoteNetworkId: string } | null };


export const InitiateAuthDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"InitiateAuth"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"provider"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"workspaceName"}},"type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"initiateAuth"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"provider"},"value":{"kind":"Variable","name":{"kind":"Name","value":"provider"}}},{"kind":"Argument","name":{"kind":"Name","value":"workspaceName"},"value":{"kind":"Variable","name":{"kind":"Name","value":"workspaceName"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"redirectUrl"}},{"kind":"Field","name":{"kind":"Name","value":"state"}}]}}]}}]} as unknown as DocumentNode<InitiateAuthMutation, InitiateAuthMutationVariables>;
export const CreateRemoteNetworkDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"CreateRemoteNetwork"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"name"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"location"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"NetworkLocation"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"createRemoteNetwork"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"name"},"value":{"kind":"Variable","name":{"kind":"Name","value":"name"}}},{"kind":"Argument","name":{"kind":"Name","value":"location"},"value":{"kind":"Variable","name":{"kind":"Name","value":"location"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"location"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}}]}}]}}]} as unknown as DocumentNode<CreateRemoteNetworkMutation, CreateRemoteNetworkMutationVariables>;
export const DeleteRemoteNetworkDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"DeleteRemoteNetwork"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"deleteRemoteNetwork"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}]}]}}]} as unknown as DocumentNode<DeleteRemoteNetworkMutation, DeleteRemoteNetworkMutationVariables>;
export const GenerateConnectorTokenDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"GenerateConnectorToken"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"connectorName"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"generateConnectorToken"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"remoteNetworkId"},"value":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}}},{"kind":"Argument","name":{"kind":"Name","value":"connectorName"},"value":{"kind":"Variable","name":{"kind":"Name","value":"connectorName"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"connectorId"}},{"kind":"Field","name":{"kind":"Name","value":"installCommand"}}]}}]}}]} as unknown as DocumentNode<GenerateConnectorTokenMutation, GenerateConnectorTokenMutationVariables>;
export const RevokeConnectorDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"RevokeConnector"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"revokeConnector"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}]}]}}]} as unknown as DocumentNode<RevokeConnectorMutation, RevokeConnectorMutationVariables>;
export const DeleteConnectorDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"DeleteConnector"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"deleteConnector"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}]}]}}]} as unknown as DocumentNode<DeleteConnectorMutation, DeleteConnectorMutationVariables>;
export const GenerateShieldTokenDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"GenerateShieldToken"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}},{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"shieldName"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"generateShieldToken"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"remoteNetworkId"},"value":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}}},{"kind":"Argument","name":{"kind":"Name","value":"shieldName"},"value":{"kind":"Variable","name":{"kind":"Name","value":"shieldName"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"shieldId"}},{"kind":"Field","name":{"kind":"Name","value":"installCommand"}}]}}]}}]} as unknown as DocumentNode<GenerateShieldTokenMutation, GenerateShieldTokenMutationVariables>;
export const RevokeShieldDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"RevokeShield"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"revokeShield"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}]}]}}]} as unknown as DocumentNode<RevokeShieldMutation, RevokeShieldMutationVariables>;
export const DeleteShieldDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"mutation","name":{"kind":"Name","value":"DeleteShield"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"deleteShield"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}]}]}}]} as unknown as DocumentNode<DeleteShieldMutation, DeleteShieldMutationVariables>;
export const LookupWorkspaceDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"LookupWorkspace"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"slug"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"lookupWorkspace"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"slug"},"value":{"kind":"Variable","name":{"kind":"Name","value":"slug"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"found"}},{"kind":"Field","name":{"kind":"Name","value":"workspace"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"slug"}}]}}]}}]}}]} as unknown as DocumentNode<LookupWorkspaceQuery, LookupWorkspaceQueryVariables>;
export const LookupWorkspacesByEmailDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"LookupWorkspacesByEmail"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"email"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"String"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"lookupWorkspacesByEmail"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"email"},"value":{"kind":"Variable","name":{"kind":"Name","value":"email"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"workspaces"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"slug"}}]}}]}}]}}]} as unknown as DocumentNode<LookupWorkspacesByEmailQuery, LookupWorkspacesByEmailQueryVariables>;
export const MeDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"Me"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"me"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"email"}},{"kind":"Field","name":{"kind":"Name","value":"role"}},{"kind":"Field","name":{"kind":"Name","value":"provider"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}}]}}]}}]} as unknown as DocumentNode<MeQuery, MeQueryVariables>;
export const GetWorkspaceDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"GetWorkspace"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"workspace"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"slug"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}}]}}]}}]} as unknown as DocumentNode<GetWorkspaceQuery, GetWorkspaceQueryVariables>;
export const GetRemoteNetworksDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"GetRemoteNetworks"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"remoteNetworks"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"location"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}},{"kind":"Field","name":{"kind":"Name","value":"networkHealth"}},{"kind":"Field","name":{"kind":"Name","value":"connectors"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenAt"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"hostname"}},{"kind":"Field","name":{"kind":"Name","value":"publicIp"}},{"kind":"Field","name":{"kind":"Name","value":"lanAddr"}},{"kind":"Field","name":{"kind":"Name","value":"certNotAfter"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}}]}},{"kind":"Field","name":{"kind":"Name","value":"shields"},"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenAt"}},{"kind":"Field","name":{"kind":"Name","value":"hostname"}},{"kind":"Field","name":{"kind":"Name","value":"interfaceAddr"}}]}}]}}]}}]} as unknown as DocumentNode<GetRemoteNetworksQuery, GetRemoteNetworksQueryVariables>;
export const GetConnectorsDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"GetConnectors"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"connectors"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"remoteNetworkId"},"value":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenAt"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"hostname"}},{"kind":"Field","name":{"kind":"Name","value":"publicIp"}},{"kind":"Field","name":{"kind":"Name","value":"lanAddr"}},{"kind":"Field","name":{"kind":"Name","value":"certNotAfter"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}}]}}]}}]} as unknown as DocumentNode<GetConnectorsQuery, GetConnectorsQueryVariables>;
export const GetConnectorDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"GetConnector"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"connector"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenAt"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"hostname"}},{"kind":"Field","name":{"kind":"Name","value":"publicIp"}},{"kind":"Field","name":{"kind":"Name","value":"lanAddr"}},{"kind":"Field","name":{"kind":"Name","value":"certNotAfter"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}},{"kind":"Field","name":{"kind":"Name","value":"remoteNetworkId"}}]}}]}}]} as unknown as DocumentNode<GetConnectorQuery, GetConnectorQueryVariables>;
export const GetRemoteNetworkDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"GetRemoteNetwork"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"remoteNetwork"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"location"}},{"kind":"Field","name":{"kind":"Name","value":"status"}}]}}]}}]} as unknown as DocumentNode<GetRemoteNetworkQuery, GetRemoteNetworkQueryVariables>;
export const GetShieldsDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"GetShields"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"shields"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"remoteNetworkId"},"value":{"kind":"Variable","name":{"kind":"Name","value":"remoteNetworkId"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenAt"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"hostname"}},{"kind":"Field","name":{"kind":"Name","value":"interfaceAddr"}},{"kind":"Field","name":{"kind":"Name","value":"connectorId"}}]}}]}}]} as unknown as DocumentNode<GetShieldsQuery, GetShieldsQueryVariables>;
export const GetShieldDocument = {"kind":"Document","definitions":[{"kind":"OperationDefinition","operation":"query","name":{"kind":"Name","value":"GetShield"},"variableDefinitions":[{"kind":"VariableDefinition","variable":{"kind":"Variable","name":{"kind":"Name","value":"id"}},"type":{"kind":"NonNullType","type":{"kind":"NamedType","name":{"kind":"Name","value":"ID"}}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"shield"},"arguments":[{"kind":"Argument","name":{"kind":"Name","value":"id"},"value":{"kind":"Variable","name":{"kind":"Name","value":"id"}}}],"selectionSet":{"kind":"SelectionSet","selections":[{"kind":"Field","name":{"kind":"Name","value":"id"}},{"kind":"Field","name":{"kind":"Name","value":"name"}},{"kind":"Field","name":{"kind":"Name","value":"status"}},{"kind":"Field","name":{"kind":"Name","value":"lastSeenAt"}},{"kind":"Field","name":{"kind":"Name","value":"version"}},{"kind":"Field","name":{"kind":"Name","value":"hostname"}},{"kind":"Field","name":{"kind":"Name","value":"lanIp"}},{"kind":"Field","name":{"kind":"Name","value":"interfaceAddr"}},{"kind":"Field","name":{"kind":"Name","value":"certNotAfter"}},{"kind":"Field","name":{"kind":"Name","value":"createdAt"}},{"kind":"Field","name":{"kind":"Name","value":"connectorId"}},{"kind":"Field","name":{"kind":"Name","value":"remoteNetworkId"}}]}}]}}]} as unknown as DocumentNode<GetShieldQuery, GetShieldQueryVariables>;