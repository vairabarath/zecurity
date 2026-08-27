// Origin suffix for a group. Admins must never mistake a directory-pushed group
// for a local one, so the display name is always paired with its origin
// (Sprint 17 FE-5, ADR-025 §7). Origin is read-only — the directory owns
// SCIM-origin groups; Zecurity owns manual ones.
//
// Pure presentational: it renders `name · <Origin>` (and an optional
// `(connectionName)` for SCIM-origin groups) from props only. It does NOT query
// Apollo — the consumer resolves connectionId → displayName (via the already
// existing GetIdpConnections) and passes it in, keeping this component testable
// and free of redundant fetches.
//
// Origin values are a closed set on the Group type: manual | scim | system.

const ORIGIN_LABEL: Record<string, string> = {
  manual: 'Local',
  scim: 'SCIM',
  system: 'System',
}

export type GroupOrigin = {
  name: string
  origin: string
  connectionId?: string | null
}

export function GroupOriginLabel({
  group,
  connectionName,
}: {
  group: GroupOrigin
  connectionName?: string | null
}) {
  const originLabel = ORIGIN_LABEL[group.origin] ?? group.origin

  // SCIM-origin groups may additionally show the source connection, e.g.
  // "Engineering · SCIM (Okta)". Only when we actually resolved a name.
  const connectionSuffix =
    group.origin === 'scim' && connectionName
      ? ` (${connectionName})`
      : ''

  return (
    <span>
      {group.name}
      <span className="text-muted-foreground"> · {originLabel}{connectionSuffix}</span>
    </span>
  )
}
