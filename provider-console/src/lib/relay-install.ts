// Relay enrolment helpers (Sprint 22 Phase E, D-30): SAN checks that mirror
// the controller, and the install commands shown once after "Add relay".
// The controller validates every request again; these only catch mistakes
// before the call.

/** Splits a free-text list on commas and whitespace, dropping empty entries. */
export function parseList(text: string): string[] {
  return text.split(/[\s,]+/).filter((v) => v !== '')
}

/** Mirrors validateDNSSANs (controller/internal/relay/provision.go). Null = valid. */
export function checkDnsSan(value: string): string | null {
  if (value !== value.toLowerCase()) return 'DNS names must be lowercase.'
  if (value.includes('*')) return 'Wildcard DNS names aren’t allowed.'
  if (/[ /]/.test(value)) return 'DNS names can’t contain a space or slash.'
  return null
}

function parseIPv4(s: string): number[] | null {
  const parts = s.split('.')
  if (parts.length !== 4) return null
  const out: number[] = []
  for (const p of parts) {
    // Go's ParseIP rejects leading zeros, so "01" is not an address.
    if (!/^(0|[1-9][0-9]{0,2})$/.test(p)) return null
    const n = Number(p)
    if (n > 255) return null
    out.push(n)
  }
  return out
}

function parseIPv6Groups(s: string): number[] | null {
  if (s.includes('.')) return null // embedded IPv4 never prints back the same way
  const halves = s.split('::')
  if (halves.length > 2) return null
  const side = (h: string) => (h === '' ? [] : h.split(':'))
  const head = side(halves[0])
  const tail = halves.length === 2 ? side(halves[1]) : []
  const groups = [...head, ...tail]
  if (!groups.every((g) => /^[0-9a-fA-F]{1,4}$/.test(g))) return null
  const missing = 8 - groups.length
  if (halves.length === 1 ? missing !== 0 : missing < 1) return null
  const nums = groups.map((g) => Number.parseInt(g, 16))
  return [...nums.slice(0, head.length), ...Array<number>(halves.length === 2 ? missing : 0).fill(0), ...nums.slice(head.length)]
}

// net.IP.String() for a 16-byte address: lowercase hex without leading
// zeros, the longest run (2+) of zero groups replaced by "::" (first wins).
function formatIPv6(g: number[]): string {
  let bestStart = -1
  let bestLen = 1
  for (let i = 0; i < 8; ) {
    if (g[i] !== 0) {
      i++
      continue
    }
    let j = i
    while (j < 8 && g[j] === 0) j++
    if (j - i > bestLen) {
      bestStart = i
      bestLen = j - i
    }
    i = j
  }
  const hex = (xs: number[]) => xs.map((x) => x.toString(16)).join(':')
  if (bestStart < 0) return hex(g)
  return `${hex(g.slice(0, bestStart))}::${hex(g.slice(bestStart + bestLen))}`
}

/** Mirrors validateIPSANs: a valid address in canonical form. Null = valid. */
export function checkIpSan(value: string): string | null {
  const invalid = 'Enter an IP address in canonical form, e.g. 203.0.113.5 or 2001:db8::1.'
  if (value.includes(':')) {
    const g = parseIPv6Groups(value)
    if (!g) return invalid
    // IPv4-mapped addresses print as IPv4, so their IPv6 text isn't canonical.
    if (g.slice(0, 5).every((x) => x === 0) && g[5] === 0xffff) return invalid
    return formatIPv6(g) === value ? null : invalid
  }
  return parseIPv4(value) ? null : invalid
}

/** POSIX single-quoting: safe for any value. */
export function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

export interface InstallParams {
  relayId: string
  provisioningToken: string
  caFingerprint: string
  controllerGrpcAddr: string
  controllerHttpAddr: string
  dnsSans: string[]
  ipSans: string[]
}

export interface InstallCommands {
  /** systemd install from a repository checkout (scripts/relay-local-install.sh). */
  installer: string
  /** Running the relay binary directly, e.g. for a quick test. */
  direct: string
  /** True when a controller address isn't configured and a placeholder is shown. */
  placeholders: boolean
}

const GRPC_PLACEHOLDER = '<controller-host>:9090'
const HTTP_PLACEHOLDER = '<controller-host>:8080'

export function buildInstallCommands(p: InstallParams): InstallCommands {
  const vars: [string, string][] = [
    ['CONTROLLER_ADDR', p.controllerGrpcAddr || GRPC_PLACEHOLDER],
    ['CONTROLLER_HTTP_ADDR', p.controllerHttpAddr || HTTP_PLACEHOLDER],
    ['RELAY_ID', p.relayId],
    ['RELAY_CA_FINGERPRINT', p.caFingerprint],
    ['RELAY_PROVISIONING_TOKEN', p.provisioningToken],
  ]
  if (p.dnsSans.length > 0) vars.push(['RELAY_DNS_SANS', p.dnsSans.join(',')])
  if (p.ipSans.length > 0) vars.push(['RELAY_IP_SANS', p.ipSans.join(',')])

  const assignments = vars.map(([k, v]) => `${k}=${shellQuote(v)}`)
  const join = (first: string, last: string) =>
    [first, ...assignments.map((a) => `  ${a}`), `  ${last}`].join(' \\\n')

  return {
    installer: join('sudo', './scripts/relay-local-install.sh ./zecurity-relay'),
    direct: join('env', './zecurity-relay'),
    placeholders: !p.controllerGrpcAddr || !p.controllerHttpAddr,
  }
}

/** The first problem in a SAN list (invalid or repeated entry), or null. */
export function checkList(entries: string[], check: (v: string) => string | null): string | null {
  const seen = new Set<string>()
  for (const v of entries) {
    const problem = check(v)
    if (problem) return `${v}: ${problem}`
    if (seen.has(v)) return `${v} is listed twice.`
    seen.add(v)
  }
  return null
}
