/**
 * Client-side mirror of the controller's `resource.NormalizeHost`
 * (controller/internal/resource/store.go, PENDING-14 F0/F1): a resource host is
 * a usable IPv4 address or a DNS name. The server is the authority; this only
 * shows the mistake before saving.
 */
export const RESOURCE_HOST_ERROR =
  'Host must be an IPv4 address (such as 10.0.0.5) or a DNS name (such as db.internal.example.com); IPv6 is not supported'

export const NAME_NEEDS_TCP_ERROR = 'A resource with a DNS name host must use protocol TCP'

export type ResourceHostKind = 'ipv4' | 'name'

const OCTET = '(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
const IPV4 = new RegExp(`^${OCTET}\\.${OCTET}\\.${OCTET}\\.${OCTET}$`)
const LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

/** The kind of a host as typed, or null when the server would refuse it. */
export function resourceHostKind(host: string): ResourceHostKind | null {
  const h = host.trim()
  if (IPV4.test(h)) {
    const first = Number(h.split('.')[0])
    if (h === '0.0.0.0' || h === '255.255.255.255') return null
    if (first >= 224 && first <= 239) return null // multicast
    return 'ipv4'
  }
  const name = h.toLowerCase().replace(/\.$/, '')
  if (name.length === 0 || name.length > 253) return null
  const labels = name.split('.')
  if (labels.length < 2 || !labels.every((l) => LABEL.test(l))) return null
  if (!/[a-z]/.test(labels[labels.length - 1])) return null
  return 'name'
}

export function isValidResourceHost(host: string): boolean {
  return resourceHostKind(host) !== null
}

/** Whether a stored (normalised) host is a DNS name; mirrors `resource.IsNameHost`. */
export function isNameHost(host: string): boolean {
  return !/^[0-9.]+$/.test(host)
}
