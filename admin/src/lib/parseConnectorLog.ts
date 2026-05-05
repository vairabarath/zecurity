export type ConnectorLogPath = 'direct' | 'shield_relay'
export type ConnectorLogDecision = 'allowed' | 'denied'

export type ParsedConnectorLog = {
  destination?: string
  port?: number
  protocol?: string
  path?: ConnectorLogPath
  decision?: ConnectorLogDecision
  raw: string
}

const DEST_RE = /\b(?:dst|destination|to)[=:]?\s*([0-9a-zA-Z._:-]+)(?::(\d+))?/i
const PORT_RE = /\bport[=:]?\s*(\d+)/i
const PROTO_RE = /\b(tcp|udp|http|https|quic)\b/i
const HOST_PORT_RE = /\b(\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-f:]+\]|[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}):(\d+)\b/

export function parseConnectorLog(message: string): ParsedConnectorLog {
  const out: ParsedConnectorLog = { raw: message }
  const lower = message.toLowerCase()

  if (/\b(allow|allowed|granted|ok)\b/.test(lower)) out.decision = 'allowed'
  if (/\b(deny|denied|reject|forbidden|access\s*denied|revoked)\b/.test(lower)) out.decision = 'denied'

  if (/shield[_-]?relay|via\s*shield|relay/.test(lower)) out.path = 'shield_relay'
  else if (/\bdirect\b/.test(lower)) out.path = 'direct'

  const hp = message.match(HOST_PORT_RE)
  if (hp) {
    out.destination = hp[1]
    out.port = Number(hp[2])
  } else {
    const dest = message.match(DEST_RE)
    if (dest) {
      out.destination = dest[1]
      if (dest[2]) out.port = Number(dest[2])
    }
    const port = message.match(PORT_RE)
    if (port && out.port === undefined) out.port = Number(port[1])
  }

  const proto = message.match(PROTO_RE)
  if (proto) out.protocol = proto[1].toLowerCase()

  return out
}
