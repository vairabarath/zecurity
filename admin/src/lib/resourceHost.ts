/**
 * Client-side mirror of the controller's `resource.NormalizeHost`
 * (controller/internal/resource/store.go, PENDING-14 F0): a resource host must
 * be a usable IPv4 address. Every client drops any other address, so a DNS name
 * or an IPv6 address used to save fine and then never work. The server is the
 * authority; this only shows the mistake before saving.
 */
export const RESOURCE_HOST_ERROR =
  'Host must be an IPv4 address, such as 10.0.0.5 (DNS names and IPv6 are not supported yet)'

const OCTET = '(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
const IPV4 = new RegExp(`^${OCTET}\\.${OCTET}\\.${OCTET}\\.${OCTET}$`)

export function isValidResourceHost(host: string): boolean {
  const h = host.trim()
  if (!IPV4.test(h)) return false
  const first = Number(h.split('.')[0])
  if (h === '0.0.0.0' || h === '255.255.255.255') return false
  if (first >= 224 && first <= 239) return false // multicast
  return true
}
