// resourceAddressing.ts — Sprint 16 Phase 10 (PENDING-14)
//
// A resource is addressed EITHER by a pinned IP (`host`) OR by a name the
// connector resolves at dial time (`hostname` + `resolver`). The server enforces
// exactly-one in validateAddressing; the types below make violating it
// unrepresentable in the UI, so an operator never sees "host and hostname are
// mutually exclusive" after a failed submit.
//
// The wire shape is deliberately looser than this: `resolver` is a raw JSON
// string, and `host` is `""` (never null) for name-addressed resources so
// existing clients keep working. Everything that renders an address must
// therefore fall back — see `addressOf`.

/** Loopback: one of exactly two addresses a shield is ever allowed to dial. */
export const LOCAL_TARGET_LOOPBACK = '127.0.0.1'

/**
 * How the connector finds the endpoint for a name-addressed resource.
 *
 * `shield` is deliberately NOT a member. Delivery (`route_type`, derived
 * server-side from status + shield_id) and resolution (`resolver.type`) are
 * orthogonal axes; a dropdown containing `shield` would encode exactly the
 * conflation the connector's delivery branch warns about, and operators would
 * then expect it to work.
 *
 * `raw` is the escape hatch, not a resolver type — it carries JSON straight
 * through for the cases the structured form cannot express.
 */
export type ResolverDraft =
  | { type: 'dns'; name: string }
  | { type: 'static'; address: string }
  | { type: 'raw'; json: string }

/**
 * The addressing choice. `host` exists only in the `ip` variant and `hostname`
 * only in the `hostname` variant, so a draft carrying both cannot be
 * constructed — the compiler rejects it rather than the server.
 *
 * `localTarget` lives on the `ip` variant alone: a name-addressed resource can
 * never be shield-delivered, because MarkProtecting joins on
 * `shields.lan_ip = resources.host` and `host` is NULL for those rows. Offering
 * the field in hostname mode would offer something that can never take effect.
 */
export type Addressing =
  | { mode: 'ip'; host: string; localTarget: string }
  | { mode: 'hostname'; hostname: string; resolver: ResolverDraft }

export function emptyAddressing(host = ''): Addressing {
  return { mode: 'ip', host, localTarget: LOCAL_TARGET_LOOPBACK }
}

/**
 * The two addresses a shield may dial, per its own `dial_target` check:
 * loopback, or the shield's own LAN IP — which for a pinned resource is the
 * `host` being entered. Returning the legal set (rather than validating free
 * text) is what keeps an illegal value out of the form entirely.
 */
export function allowedLocalTargets(host: string): string[] {
  const h = host.trim()
  return h && h !== LOCAL_TARGET_LOOPBACK ? [LOCAL_TARGET_LOOPBACK, h] : [LOCAL_TARGET_LOOPBACK]
}

/** Serialize a draft to the `{"type":…,"config":{…}}` string the server stores. */
export function toResolverJson(d: ResolverDraft): string | undefined {
  if (d.type === 'raw') {
    const raw = d.json.trim()
    return raw || undefined
  }
  if (d.type === 'dns') {
    const name = d.name.trim()
    // No config at all means "resolve the resource's own hostname", which is the
    // common case — don't emit an empty config object for it.
    return name
      ? JSON.stringify({ type: 'dns', config: { name } })
      : JSON.stringify({ type: 'dns' })
  }
  return JSON.stringify({ type: 'static', config: { address: d.address.trim() } })
}

/**
 * Parse a stored resolver back into a draft for editing.
 *
 * Anything the structured form cannot represent round-trips through `raw`
 * rather than being silently dropped — including a config carrying keys we do
 * not model. Losing an operator's stored value on an unrelated edit would be
 * worse than showing them JSON.
 */
export function parseResolverJson(raw: string | null | undefined): ResolverDraft {
  const text = (raw ?? '').trim()
  if (!text) return { type: 'dns', name: '' }
  try {
    const v = JSON.parse(text)
    const cfg = (v?.config ?? {}) as Record<string, unknown>
    const extraKeys = Object.keys(cfg).filter((k) => k !== 'name' && k !== 'address')
    if (v?.type === 'dns' && extraKeys.length === 0) {
      return { type: 'dns', name: typeof cfg.name === 'string' ? cfg.name : '' }
    }
    if (v?.type === 'static' && extraKeys.length === 0 && typeof cfg.address === 'string') {
      return { type: 'static', address: cfg.address }
    }
  } catch {
    // fall through — malformed JSON is still the operator's data
  }
  return { type: 'raw', json: text }
}

/** True when the draft is complete enough to submit. */
export function isAddressingValid(a: Addressing): boolean {
  if (a.mode === 'ip') return a.host.trim().length > 0
  if (!a.hostname.trim()) return false
  const r = a.resolver
  if (r.type === 'static') return r.address.trim().length > 0
  if (r.type === 'raw') return r.json.trim().length > 0
  return true // dns with no explicit name resolves the hostname itself
}

/** The `CreateResourceInput` addressing fields — exactly one of host/hostname. */
export function toCreateAddressingInput(a: Addressing): {
  host?: string
  hostname?: string
  resolver?: string
  localTarget?: string
} {
  if (a.mode === 'ip') {
    return {
      host: a.host.trim(),
      localTarget: a.localTarget.trim() || undefined,
    }
  }
  return {
    hostname: a.hostname.trim(),
    resolver: toResolverJson(a.resolver),
  }
}

// ── Display helpers ─────────────────────────────────────────────────────────
//
// `host` is "" for name-addressed resources, so rendering it raw produces a
// blank address. Several pages did exactly that before this phase.

export interface AddressableResource {
  host?: string | null
  hostname?: string | null
}

/** The address to show an operator: the pinned IP, or the client-facing name. */
export function addressOf(r: AddressableResource): string {
  const host = (r.host ?? '').trim()
  if (host) return host
  const hostname = (r.hostname ?? '').trim()
  return hostname || '—'
}

/** True when this resource is addressed by name rather than by a pinned IP. */
export function isNameAddressed(r: AddressableResource): boolean {
  return !(r.host ?? '').trim() && !!(r.hostname ?? '').trim()
}

/**
 * Delivery mode, surfaced for the first time in this phase.
 *
 * Derived exactly as the server does it: a bound shield plus a protected-ish
 * status means the Shield delivers the traffic; anything else is dialed by the
 * connector. Never inferred from `resolver` — that answers a different question.
 */
export function deliveryOf(r: {
  status?: string | null
  shield?: { id: string } | null
}): 'protected' | 'connector' {
  const s = (r.status ?? '').toLowerCase()
  const shieldDelivered = !!r.shield && (s === 'protecting' || s === 'protected' || s === 'failed')
  return shieldDelivered ? 'protected' : 'connector'
}
