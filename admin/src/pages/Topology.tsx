import { useState, useRef, useEffect, useCallback, useMemo } from 'react'
import { useQuery } from '@apollo/client/react'
import {
  GetRemoteNetworksDocument,
  GetWorkspaceDocument,
  ConnectorStatus,
  ShieldStatus,
  NetworkHealth,
} from '@/generated/graphql'

// ── Constants ─────────────────────────────────────────
const PALETTE = {
  ws:     'oklch(0.86 0.095 175)',
  net:    'oklch(0.78 0.10 235)',
  conn:   'oklch(0.83 0.11 55)',
  shield: 'oklch(0.78 0.09 310)',
  warn:   'oklch(0.85 0.13 80)',
  err:    'oklch(0.72 0.17 25)',
  mute:   'oklch(0.58 0.012 250)',
} as const

const WORLD_W = 1800
const WORLD_H = 1400
const MIN_Z = 0.3
const MAX_Z = 3.0

// ── Types ─────────────────────────────────────────────
type TopoStatus = 'ok' | 'warn' | 'err' | 'pend'
type TopoKind = 'ws' | 'net' | 'conn' | 'shield'
type FocusKind = 'home' | 'network' | 'connector'

interface TopoNode {
  id: string
  label: string
  kind: TopoKind
  status: TopoStatus
  sub?: string
  meta?: Record<string, string | number>
  children?: TopoNode[]
  _x: number
  _y: number
  _ang: number
}

interface FocusState {
  kind: FocusKind
  id: string | null
}

// ── Helpers ───────────────────────────────────────────
function statusColor(kind: string, status: TopoStatus): string {
  if (status === 'warn') return PALETTE.warn
  if (status === 'err')  return PALETTE.err
  if (status === 'pend') return PALETTE.mute
  return PALETTE[kind as keyof typeof PALETTE] || PALETTE.net
}

function alphaColor(c: string, a: number): string {
  return c.replace(')', ` / ${a})`)
}

function mapNetStatus(nh: NetworkHealth): TopoStatus {
  if (nh === NetworkHealth.Online)   return 'ok'
  if (nh === NetworkHealth.Degraded) return 'warn'
  return 'pend'
}
function mapConnStatus(s: ConnectorStatus): TopoStatus {
  if (s === ConnectorStatus.Active)       return 'ok'
  if (s === ConnectorStatus.Disconnected) return 'warn'
  return 'pend'
}
function mapShieldStatus(s: ShieldStatus): TopoStatus {
  if (s === ShieldStatus.Active)       return 'ok'
  if (s === ShieldStatus.Disconnected) return 'warn'
  return 'pend'
}

// Computes the set of IDs that should be lit (bright) when hoveredId is set.
// Rules:
//   hover workspace  → '__all__' sentinel (nothing dims)
//   hover network    → workspace + network + all child connectors + all child shields
//   hover connector  → workspace + parent network + connector + all child shields
//   hover shield     → workspace + parent network + parent connector + shield
function computeLitSet(hoveredId: string | null, tree: TopoNode): Set<string> {
  if (!hoveredId) return new Set()
  if (hoveredId === tree.id) return new Set(['__all__'])

  for (const net of tree.children ?? []) {
    if (hoveredId === net.id) {
      const s = new Set([tree.id, net.id])
      for (const c of net.children ?? []) {
        s.add(c.id)
        for (const sh of c.children ?? []) s.add(sh.id)
      }
      return s
    }
    for (const conn of net.children ?? []) {
      if (hoveredId === conn.id) {
        const s = new Set([tree.id, net.id, conn.id])
        for (const sh of conn.children ?? []) s.add(sh.id)
        return s
      }
      for (const sh of conn.children ?? []) {
        if (hoveredId === sh.id) return new Set([tree.id, net.id, conn.id, sh.id])
      }
    }
  }
  return new Set([hoveredId])
}

function findNode(tree: TopoNode, id: string | null): TopoNode | null {
  if (!id) return null
  if (tree.id === id) return tree
  for (const net of tree.children ?? []) {
    if (net.id === id) return net
    for (const conn of net.children ?? []) {
      if (conn.id === id) return conn
      for (const sh of conn.children ?? []) {
        if (sh.id === id) return sh
      }
    }
  }
  return null
}

// Returns typed pair so TypeScript can narrow correctly — avoids closure narrowing bugs
function findConnector(tree: TopoNode, id: string | null): { net: TopoNode; conn: TopoNode } | null {
  if (!id) return null
  for (const n of tree.children ?? []) {
    for (const c of n.children ?? []) {
      if (c.id === id) return { net: n, conn: c }
    }
  }
  return null
}

function topoCounts(tree: TopoNode) {
  let nets = 0, conns = 0, shields = 0, warn = 0, pend = 0
  for (const n of tree.children || []) {
    nets++
    if (n.status === 'warn') warn++
    if (n.status === 'pend') pend++
    for (const c of n.children || []) {
      conns++
      if (c.status === 'warn') warn++
      if (c.status === 'pend') pend++
      for (const s of c.children || []) {
        shields++
        if (s.status === 'warn') warn++
        if (s.status === 'pend') pend++
      }
    }
  }
  return { nets, conns, shields, warn, pend }
}

// ── Glyph ─────────────────────────────────────────────
function Glyph({ kind, x, y, size = 10, color = '#fff' }: {
  kind: TopoKind; x: number; y: number; size?: number; color?: string
}) {
  const s = size
  if (kind === 'ws') {
    const h = s * 1.1
    const pts = [0, 1, 2, 3, 4, 5].map(i => {
      const a = (Math.PI / 3) * i - Math.PI / 2
      return `${x + h * Math.cos(a)},${y + h * Math.sin(a)}`
    }).join(' ')
    return (
      <g>
        <polygon points={pts} fill="none" stroke={color} strokeWidth="1.8" />
        <circle cx={x} cy={y} r={s * 0.34} fill={color} />
      </g>
    )
  }
  if (kind === 'net') {
    return (
      <g stroke={color} strokeWidth="1.5" fill="none" strokeLinecap="round">
        <circle cx={x} cy={y} r={s} />
        <ellipse cx={x} cy={y} rx={s} ry={s * 0.4} />
        <line x1={x} y1={y - s} x2={x} y2={y + s} />
      </g>
    )
  }
  if (kind === 'conn') {
    return (
      <g stroke={color} strokeWidth="1.6" fill="none" strokeLinejoin="round">
        <rect x={x - s * 0.8} y={y - s * 0.8} width={s * 1.6} height={s * 1.6} rx={s * 0.35} />
        <line x1={x} y1={y - s * 0.2} x2={x} y2={y + s * 0.8} />
        <line x1={x - s * 0.35} y1={y - s * 0.2} x2={x + s * 0.35} y2={y - s * 0.2} />
      </g>
    )
  }
  if (kind === 'shield') {
    return (
      <g stroke={color} strokeWidth="1.5" fill="none" strokeLinejoin="round" strokeLinecap="round">
        <path d={`M${x} ${y - s} l${s * 0.82} ${s * 0.28} v${s * 0.55} c0 ${s * 0.62} -${s * 0.52} ${s * 0.92} -${s * 0.82} ${s * 1.00} c-${s * 0.30} -${s * 0.08} -${s * 0.82} -${s * 0.38} -${s * 0.82} -${s * 1.00} v-${s * 0.55} z`} />
        <path d={`M${x - s * 0.22} ${y + s * 0.05} l${s * 0.20} ${s * 0.20} l${s * 0.38} -${s * 0.38}`} />
      </g>
    )
  }
  return null
}

// ── TopoGraph SVG ─────────────────────────────────────
function TopoGraph({
  width = WORLD_W, height = WORLD_H,
  focus, onFocusChange, selectedId, onNodeClick, hoveredId, onHover,
  filterStatus = 'all', searchQuery = '', tree,
}: {
  width?: number; height?: number
  focus: FocusState; onFocusChange: (f: FocusState) => void
  selectedId: string | null; onNodeClick: (n: TopoNode) => void
  hoveredId: string | null; onHover: (id: string | null) => void
  filterStatus?: string; searchQuery?: string; tree: TopoNode
}) {
  const W = width, H = height
  const CX = W / 2, CY = H / 2
  const TAU = Math.PI * 2
  const base = Math.min(W, H)
  const focusKind = focus.kind
  const focusId = focus.id

  const netFocus = focusKind === 'network'
    ? (tree.children?.find(n => n.id === focusId) ?? null)
    : null
  const connFocus = focusKind === 'connector' ? findConnector(tree, focusId) : null

  let R_core: number, R_net: number, R_conn: number, R_shield: number
  if (focusKind === 'home') {
    R_core = base * 0.065; R_net = base * 0.165; R_conn = base * 0.305; R_shield = base * 0.395
  } else if (focusKind === 'network') {
    R_core = base * 0.070; R_net = base * 0.200; R_conn = base * 0.310; R_shield = base * 0.410
  } else {
    R_core = base * 0.070; R_net = base * 0.195; R_conn = base * 0.320; R_shield = base * 0.420
  }

  const nodes: TopoNode[] = []
  const edges: [TopoNode, TopoNode][] = []
  const netWeight = (n: TopoNode) => Math.max((n.children || []).length, 1)
  const shieldR = base * 0.015, shieldGap = shieldR * 2.6
  const connR = base * 0.022          // connector node half-size (matches rect r below)
  const shieldOffset = base * 0.062   // gap from connector edge → shield badge/node center

  const fanShields = (conn: TopoNode, baseAng: number, radialOffset?: number) => {
    const shields = conn.children || []
    if (!shields.length) return
    const ux = Math.cos(baseAng), uy = Math.sin(baseAng)
    const tx = -uy, ty = ux
    const r0 = radialOffset ?? (R_conn + connR + shieldOffset)
    const n = shields.length
    shields.forEach((sh, i) => {
      const t = (i - (n - 1) / 2) * shieldGap
      sh._ang = baseAng; sh._x = CX + ux * r0 + tx * t; sh._y = CY + uy * r0 + ty * t
      nodes.push(sh); edges.push([conn, sh])
    })
  }

  // HOME layout
  if (focusKind === 'home') {
    const totalW = (tree.children || []).reduce((s, n) => s + netWeight(n), 0)
    let cursor = -Math.PI / 2
    ;(tree.children || []).forEach(net => {
      const wedge = (netWeight(net) / totalW) * TAU
      const netAng = cursor + wedge / 2
      net._ang = netAng; net._x = CX + R_net * Math.cos(netAng); net._y = CY + R_net * Math.sin(netAng)
      ;(net.children || []).forEach((conn, i) => {
        const conns = net.children || []
        const t = conns.length === 1 ? 0 : (i / (conns.length - 1)) - 0.5
        const cAng = netAng + t * (wedge * 0.78)
        conn._ang = cAng; conn._x = CX + R_conn * Math.cos(cAng); conn._y = CY + R_conn * Math.sin(cAng)
        nodes.push(conn); edges.push([net, conn])
        if (hoveredId === conn.id || selectedId === conn.id) fanShields(conn, cAng)
      })
      nodes.push(net); edges.push([tree, net]); cursor += wedge
    })
  }
  // NETWORK FOCUS layout
  else if (focusKind === 'network' && netFocus) {
    netFocus._ang = -Math.PI / 2; netFocus._x = CX; netFocus._y = CY - R_net
    nodes.push(netFocus); edges.push([tree, netFocus])
    ;(netFocus.children || []).forEach((conn, i) => {
      const conns = netFocus!.children || []
      const cAng = -Math.PI / 2 + (i / conns.length) * TAU
      conn._ang = cAng; conn._x = CX + R_conn * Math.cos(cAng); conn._y = CY + R_conn * Math.sin(cAng)
      nodes.push(conn); edges.push([netFocus!, conn])
      fanShields(conn, cAng, R_conn + base * 0.055)
    })
  }
  // CONNECTOR FOCUS layout
  else if (focusKind === 'connector' && connFocus) {
    const { net: cfNet, conn: cfConn } = connFocus
    cfNet._ang = -Math.PI / 2; cfNet._x = CX; cfNet._y = CY - R_net
    nodes.push(cfNet); edges.push([tree, cfNet])
    cfConn._ang = -Math.PI / 2; cfConn._x = CX; cfConn._y = CY - R_conn
    nodes.push(cfConn); edges.push([cfNet, cfConn])
    ;(cfConn.children || []).forEach((sh, i) => {
      const total = cfConn.children?.length ?? 1
      const ang = -Math.PI / 2 + (i / Math.max(total, 1)) * TAU
      sh._ang = ang; sh._x = CX + R_shield * Math.cos(ang); sh._y = CY + R_shield * Math.sin(ang)
      nodes.push(sh); edges.push([cfConn, sh])
    })
  }

  tree._x = CX; tree._y = CY; tree._ang = 0

  const sq = searchQuery.trim().toLowerCase()
  const isDim = (n: TopoNode) =>
    (sq && !n.label.toLowerCase().includes(sq)) ||
    (filterStatus !== 'all' && n.status !== filterStatus)

  // litSet: IDs that should be bright when something is hovered.
  // '__all__' sentinel means nothing dims (workspace hover).
  const litSet = computeLitSet(hoveredId, tree)
  const isLit = (id: string) => litSet.size === 0 || litSet.has('__all__') || litSet.has(id)
  const hasHover = litSet.size > 0

  const isShellNode = (n: TopoNode): boolean => {
    if (focusKind === 'network' && n.kind === 'net' && n.id === focusId) return true
    if (focusKind === 'connector' && n.kind === 'net' && connFocus && n.id === connFocus.net.id) return true
    if (focusKind === 'connector' && n.kind === 'conn' && n.id === focusId) return true
    return false
  }

  const backFromNetwork = (e: React.MouseEvent) => { e.stopPropagation(); onFocusChange({ kind: 'home', id: null }) }
  const backFromConnector = (e: React.MouseEvent) => { e.stopPropagation(); onFocusChange({ kind: 'network', id: connFocus!.net.id }) }

  return (
    <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="xMidYMid meet"
      style={{ width: '100%', height: '100%', display: 'block' }}>
      <defs>
        <radialGradient id="tg-core" cx="50%" cy="50%" r="50%">
          <stop offset="0%"   stopColor={PALETTE.ws}     stopOpacity="0.55" />
          <stop offset="55%"  stopColor={PALETTE.ws}     stopOpacity="0.10" />
          <stop offset="100%" stopColor={PALETTE.ws}     stopOpacity="0" />
        </radialGradient>
        <radialGradient id="tg-net" cx="50%" cy="50%" r="50%">
          <stop offset="0%"   stopColor={PALETTE.net}    stopOpacity="0.45" />
          <stop offset="100%" stopColor={PALETTE.net}    stopOpacity="0" />
        </radialGradient>
        <radialGradient id="tg-conn" cx="50%" cy="50%" r="50%">
          <stop offset="0%"   stopColor={PALETTE.conn}   stopOpacity="0.42" />
          <stop offset="100%" stopColor={PALETTE.conn}   stopOpacity="0" />
        </radialGradient>
        <radialGradient id="tg-shield" cx="50%" cy="50%" r="50%">
          <stop offset="0%"   stopColor={PALETTE.shield} stopOpacity="0.42" />
          <stop offset="100%" stopColor={PALETTE.shield} stopOpacity="0" />
        </radialGradient>
        <radialGradient id="tg-warn" cx="50%" cy="50%" r="50%">
          <stop offset="0%"   stopColor={PALETTE.warn}   stopOpacity="0.55" />
          <stop offset="100%" stopColor={PALETTE.warn}   stopOpacity="0" />
        </radialGradient>
        <path id="tg-arc-net"  d={`M ${CX - R_net},${CY} A ${R_net},${R_net} 0 0 1 ${CX + R_net},${CY}`} />
        <path id="tg-arc-conn" d={`M ${CX - R_conn},${CY} A ${R_conn},${R_conn} 0 0 1 ${CX + R_conn},${CY}`} />
      </defs>

      {/* Guide rings */}
      {focusKind === 'home' && (
        <g>
          <circle cx={CX} cy={CY} r={R_net}  fill="none" stroke="oklch(0.36 0.014 255)" strokeDasharray="3 6" strokeWidth="1" />
          <circle cx={CX} cy={CY} r={R_conn} fill="none" stroke="oklch(0.32 0.014 255)" strokeDasharray="3 6" strokeWidth="1" />
        </g>
      )}

      {/* Network shell */}
      {focusKind === 'network' && netFocus && (() => {
        const nf = netFocus
        const c = statusColor('net', nf.status)
        return (
          <g className="shell">
            <circle cx={CX} cy={CY} r={R_net + base * 0.020} fill={alphaColor(c, 0.04)} stroke={c} strokeOpacity="0.22" strokeWidth="1" pointerEvents="none" />
            <circle cx={CX} cy={CY} r={R_net} fill={alphaColor(c, 0.08)} stroke={c} strokeOpacity="0.9" strokeWidth="2.4" pointerEvents="none" />
            <circle cx={CX} cy={CY} r={R_net} fill="none" stroke="transparent" strokeWidth="18" onClick={backFromNetwork} style={{ cursor: 'pointer' }} />
            <text fontSize={base * 0.020} fill={c} fontWeight="800" letterSpacing="0.14em" style={{ textTransform: 'uppercase', pointerEvents: 'none' }}>
              <textPath href="#tg-arc-net" startOffset="50%" textAnchor="middle">{nf.label} · network</textPath>
            </text>
          </g>
        )
      })()}

      {/* Connector + network shells */}
      {focusKind === 'connector' && connFocus && (() => {
        const { net: shellNet, conn: shellConn } = connFocus
        const cc = statusColor('conn', shellConn.status)
        const nc = statusColor('net', shellNet.status)
        const goToNet = (e: React.MouseEvent) => { e.stopPropagation(); onFocusChange({ kind: 'network', id: shellNet.id }) }
        return (
          <g>
            <g className="shell">
              <circle cx={CX} cy={CY} r={R_conn + base * 0.022} fill={alphaColor(cc, 0.04)} stroke={cc} strokeOpacity="0.22" strokeWidth="1" pointerEvents="none" />
              <circle cx={CX} cy={CY} r={R_conn} fill={alphaColor(cc, 0.06)} stroke={cc} strokeOpacity="0.9" strokeWidth="2.6" pointerEvents="none" />
              <circle cx={CX} cy={CY} r={R_conn} fill="none" stroke="transparent" strokeWidth="18" onClick={backFromConnector} style={{ cursor: 'pointer' }} />
              <text fontSize={base * 0.019} fill={cc} fontWeight="800" letterSpacing="0.14em" style={{ textTransform: 'uppercase', pointerEvents: 'none' }}>
                <textPath href="#tg-arc-conn" startOffset="50%" textAnchor="middle">{shellConn.label} · connector</textPath>
              </text>
            </g>
            <g className="shell">
              <circle cx={CX} cy={CY} r={R_net} fill={alphaColor(nc, 0.09)} stroke={nc} strokeOpacity="0.85" strokeWidth="2" pointerEvents="none" />
              <circle cx={CX} cy={CY} r={R_net} fill="none" stroke="transparent" strokeWidth="18" onClick={goToNet} style={{ cursor: 'pointer' }} />
              <text fontSize={base * 0.015} fill={nc} fontWeight="800" letterSpacing="0.16em" style={{ textTransform: 'uppercase', pointerEvents: 'none' }}>
                <textPath href="#tg-arc-net" startOffset="50%" textAnchor="middle">{shellNet.label} · network</textPath>
              </text>
            </g>
          </g>
        )
      })()}

      {/* Edges */}
      {edges.map(([A, B], i) => {
        const shellRadius = (n: TopoNode): number | null => {
          if (focusKind === 'network' && n.kind === 'net' && n.id === focusId) return R_net
          if (focusKind === 'connector' && n.kind === 'net' && connFocus && n.id === connFocus.net.id) return R_net
          if (focusKind === 'connector' && n.kind === 'conn' && n.id === focusId) return R_conn
          return null
        }
        const shA = shellRadius(A), shB = shellRadius(B)
        let ax = A._x, ay = A._y, bx = B._x, by = B._y
        if (shA != null && shB != null) {
          ax = CX; ay = CY - shA; bx = CX; by = CY - shB
        } else if (shA != null) {
          const dxx = B._x - CX, dyy = B._y - CY, dd = Math.hypot(dxx, dyy) || 1
          ax = CX + (dxx / dd) * shA; ay = CY + (dyy / dd) * shA
        } else if (shB != null) {
          const dxx = A._x - CX, dyy = A._y - CY, dd = Math.hypot(dxx, dyy) || 1
          bx = CX + (dxx / dd) * shB; by = CY + (dyy / dd) * shB
        }
        const color = statusColor(B.kind, B.status)
        const live = B.status !== 'pend'
        const dimmed = isDim(B)
        // Edge is "linked" only if BOTH endpoints are in the lit set (whole ancestor→descendant chain)
        const linked = hasHover && isLit(A.id) && isLit(B.id)
        let opacity: number
        if (linked) opacity = 0.95
        else if (hasHover) opacity = 0.06    // something is hovered but this edge is not in the chain
        else if (dimmed) opacity = 0.08
        else if (live) opacity = 0.42
        else opacity = 0.16
        const mx = (ax + bx) / 2, my = (ay + by) / 2
        const pull = B.kind === 'shield' ? 0.0 : B.kind === 'conn' ? 0.06 : 0.02
        const ctrlX = mx + (CX - mx) * pull, ctrlY = my + (CY - my) * pull
        const d = `M${ax},${ay} Q${ctrlX},${ctrlY} ${bx},${by}`
        return (
          <g key={`e${i}`} style={{ pointerEvents: 'none' }}>
            <path d={d} fill="none" stroke={color} strokeOpacity={opacity}
              strokeWidth={linked ? 2.6 : B.kind === 'net' ? 1.6 : B.kind === 'conn' ? 1.3 : 1}
              strokeDasharray={live ? undefined : '3 3'} />
            {live && !dimmed && (!hasHover || linked) && (
              <circle r={B.kind === 'shield' ? 2 : 2.6} fill={color} opacity={0}>
                <animateMotion dur={`${2.2 + (i % 5) * 0.35}s`} repeatCount="indefinite" path={d} />
                <animate attributeName="opacity" values="0;1;1;0" dur={`${2.2 + (i % 5) * 0.35}s`} repeatCount="indefinite" />
              </circle>
            )}
          </g>
        )
      })}

      {/* Workspace core */}
      <g className="topo-node"
        onClick={(e) => { e.stopPropagation(); onNodeClick(tree); onFocusChange({ kind: 'home', id: null }) }}
        onMouseEnter={() => onHover(tree.id)} onMouseLeave={() => onHover(null)}
        style={{ cursor: 'pointer' }}>
        <circle cx={CX} cy={CY} r={R_core + base * 0.010} fill="transparent" pointerEvents="all" />
        <circle cx={CX} cy={CY} r={base * 0.125} fill="url(#tg-core)" pointerEvents="none" />
        <circle cx={CX} cy={CY} r={R_core}
          fill={alphaColor(PALETTE.ws, 0.16)} stroke={PALETTE.ws}
          strokeWidth={selectedId === tree.id ? 2.8 : 1.8} pointerEvents="all" />
        <Glyph kind="ws" x={CX} y={CY - base * 0.014} size={base * 0.028} color={PALETTE.ws} />
        <text x={CX} y={CY + base * 0.020} textAnchor="middle"
          fontSize={base * 0.028} fill="oklch(0.97 0.005 250)" fontWeight="800" letterSpacing="-0.02em">
          {tree.label}
        </text>
        <text x={CX} y={CY + base * 0.040} textAnchor="middle"
          fontSize={base * 0.012} fill="oklch(0.62 0.012 250)" fontWeight="700"
          style={{ textTransform: 'uppercase', letterSpacing: '0.22em' }}>
          {tree.sub}
        </text>
      </g>

      {/* Nodes */}
      {nodes.map((n) => {
        if (isShellNode(n)) return null
        const c = statusColor(n.kind, n.status)
        const dx = Math.cos(n._ang), dy = Math.sin(n._ang)
        const dim = isDim(n)
        const isSelected = selectedId === n.id
        const isFocused = hoveredId === n.id
        // Fade node when hover active and this node is not in the lit chain
        const faded = (hasHover && !isLit(n.id)) || dim
        const handleClick = (e: React.MouseEvent) => {
          e.stopPropagation(); onNodeClick(n)
          if (n.kind === 'net') onFocusChange({ kind: 'network', id: n.id })
          else if (n.kind === 'conn') onFocusChange({ kind: 'connector', id: n.id })
        }

        if (n.kind === 'net') {
          const r = base * 0.036
          return (
            <g key={n.id} className="topo-node"
              onClick={handleClick} onMouseEnter={() => onHover(n.id)} onMouseLeave={() => onHover(null)}
              style={{ cursor: 'pointer', opacity: faded ? 0.24 : 1, transform: `translate(${n._x}px, ${n._y}px)` }}>
              <circle cx={0} cy={0} r={r + base * 0.024} fill={n.status === 'warn' ? 'url(#tg-warn)' : 'url(#tg-net)'} />
              <circle cx={0} cy={0} r={r} fill={alphaColor(c, 0.20)} stroke={c} strokeWidth={isSelected || isFocused ? 2.8 : 1.8} />
              <Glyph kind="net" x={0} y={0} size={r * 0.48} color={c} />
              <text x={dx * (r + 18)} y={dy * (r + 18) + 5}
                textAnchor={dx > 0.3 ? 'start' : dx < -0.3 ? 'end' : 'middle'}
                fontSize={base * 0.014} fill="oklch(0.97 0.005 250)" fontWeight="700"
                style={{ pointerEvents: 'none' }}>
                {n.label}
              </text>
            </g>
          )
        }

        if (n.kind === 'conn') {
          const r = base * 0.022
          const shieldCount = (n.children || []).length
          const showBadge = focusKind === 'home' && shieldCount > 0 && hoveredId !== n.id && selectedId !== n.id
          const bx = dx * (connR + shieldOffset), by = dy * (connR + shieldOffset)
          const bR = base * 0.013
          return (
            <g key={n.id} className="topo-node"
              onClick={handleClick} onMouseEnter={() => onHover(n.id)} onMouseLeave={() => onHover(null)}
              style={{ cursor: 'pointer', opacity: faded ? 0.24 : 1, transform: `translate(${n._x}px, ${n._y}px)` }}>
              <circle cx={0} cy={0} r={r + base * 0.018} fill={n.status === 'warn' ? 'url(#tg-warn)' : 'url(#tg-conn)'} />
              <rect x={-r} y={-r} width={r * 2} height={r * 2} rx={r * 0.4}
                fill={alphaColor(c, 0.18)} stroke={c} strokeWidth={isSelected || isFocused ? 2.6 : 1.6} />
              <Glyph kind="conn" x={0} y={0} size={r * 0.56} color={c} />
              <text x={dx * (r + 18)} y={dy * (r + 18) + 5}
                textAnchor={dx > 0.3 ? 'start' : dx < -0.3 ? 'end' : 'middle'}
                fontSize={base * 0.0115} fill="oklch(0.82 0.010 250)" fontWeight="700"
                style={{ pointerEvents: 'none' }}>
                {n.label}
              </text>
              {showBadge && (
                <g style={{ pointerEvents: 'none' }}>
                  <line x1={dx * r} y1={dy * r} x2={bx - dx * bR} y2={by - dy * bR}
                    stroke={PALETTE.shield} strokeOpacity="0.55" strokeWidth="1.2" strokeDasharray="2 3" />
                  <circle cx={bx} cy={by} r={bR} fill={alphaColor(PALETTE.shield, 0.18)} stroke={PALETTE.shield} strokeOpacity="0.9" strokeWidth="1.4" />
                  <text x={bx} y={by + bR * 0.38} textAnchor="middle"
                    fontSize={bR * 1.1} fill={PALETTE.shield} fontWeight="800"
                    style={{ fontFamily: 'JetBrains Mono, ui-monospace, monospace' }}>
                    {shieldCount}
                  </text>
                </g>
              )}
            </g>
          )
        }

        // Shield
        const r = base * 0.014
        const showLabel = focusKind === 'connector' || isFocused || isSelected
        return (
          <g key={n.id} className="topo-node"
            onClick={handleClick} onMouseEnter={() => onHover(n.id)} onMouseLeave={() => onHover(null)}
            style={{ cursor: 'pointer', opacity: faded ? 0.28 : 1, transform: `translate(${n._x}px, ${n._y}px)` }}>
            <circle cx={0} cy={0} r={r + base * 0.012} fill={n.status === 'warn' ? 'url(#tg-warn)' : 'url(#tg-shield)'} />
            <circle cx={0} cy={0} r={r} fill={alphaColor(c, 0.16)} stroke={c} strokeWidth={isSelected || isFocused ? 2.2 : 1.3} />
            <Glyph kind="shield" x={0} y={0} size={r * 0.85} color={c} />
            {showLabel && (
              <text x={dx * (r + 12)} y={dy * (r + 12) + 4}
                textAnchor={dx > 0.3 ? 'start' : dx < -0.3 ? 'end' : 'middle'}
                fontSize={base * 0.0095} fill="oklch(0.82 0.010 250)" fontWeight="700"
                style={{ pointerEvents: 'none', fontFamily: 'JetBrains Mono, ui-monospace, monospace' }}>
                {n.label.replace(/^shield-/, '')}
              </text>
            )}
          </g>
        )
      })}
    </svg>
  )
}

// ── PanZoom ───────────────────────────────────────────
function PanZoom({ children, fitSignal }: { children: React.ReactNode; fitSignal: number }) {
  const wrapRef = useRef<HTMLDivElement>(null)
  const [view, setView] = useState({ x: 0, y: 0, z: 1 })
  const dragRef = useRef<{ x: number; y: number; vx: number; vy: number } | null>(null)

  const fit = useCallback(() => {
    const el = wrapRef.current; if (!el) return
    const r = el.getBoundingClientRect()
    const zx = (r.width * 0.94) / WORLD_W, zy = (r.height * 0.94) / WORLD_H
    const z = Math.max(MIN_Z, Math.min(MAX_Z, Math.min(zx, zy)))
    setView({ z, x: (r.width - WORLD_W * z) / 2, y: (r.height - WORLD_H * z) / 2 })
  }, [])

  useEffect(() => {
    fit()
    const ro = new ResizeObserver(fit)
    if (wrapRef.current) ro.observe(wrapRef.current)
    return () => ro.disconnect()
  }, [fit])

  useEffect(() => { fit() }, [fitSignal, fit])

  const zoomAt = (clientX: number, clientY: number, factor: number) => {
    setView(prev => {
      const r = wrapRef.current!.getBoundingClientRect()
      const cx = clientX - r.left, cy = clientY - r.top
      const newZ = Math.max(MIN_Z, Math.min(MAX_Z, prev.z * factor))
      const k = newZ / prev.z
      return { z: newZ, x: cx - (cx - prev.x) * k, y: cy - (cy - prev.y) * k }
    })
  }

  useEffect(() => {
    const el = wrapRef.current; if (!el) return
    const onWheel = (e: WheelEvent) => { e.preventDefault(); zoomAt(e.clientX, e.clientY, Math.exp(-e.deltaY * 0.0015)) }
    el.addEventListener('wheel', onWheel, { passive: false })
    return () => el.removeEventListener('wheel', onWheel)
  })

  const onPointerDown = (e: React.PointerEvent) => {
    if (e.button !== 0 && e.button !== 1) return
    const target = e.target as Element
    if (target.closest('.topo-node, .shell, [data-stopdrag]')) return
    dragRef.current = { x: e.clientX, y: e.clientY, vx: view.x, vy: view.y }
    e.currentTarget.setPointerCapture(e.pointerId)
  }
  const onPointerMove = (e: React.PointerEvent) => {
    if (!dragRef.current) return
    const d = dragRef.current
    setView(prev => ({ ...prev, x: d.vx + (e.clientX - d.x), y: d.vy + (e.clientY - d.y) }))
  }
  const onPointerUp = (e: React.PointerEvent) => {
    dragRef.current = null
    try { e.currentTarget.releasePointerCapture(e.pointerId) } catch { /* no-op */ }
  }
  const zoomBtn = (f: number) => () => {
    const r = wrapRef.current!.getBoundingClientRect()
    zoomAt(r.left + r.width / 2, r.top + r.height / 2, f)
  }

  return (
    <div className="topo-pz-wrap" ref={wrapRef}
      onPointerDown={onPointerDown} onPointerMove={onPointerMove}
      onPointerUp={onPointerUp} onPointerLeave={onPointerUp}>
      <div style={{
        position: 'absolute', top: 0, left: 0,
        width: WORLD_W, height: WORLD_H,
        transform: `translate(${view.x}px, ${view.y}px) scale(${view.z})`,
        transformOrigin: '0 0', willChange: 'transform',
      }}>
        {children}
      </div>
      <div className="topo-pz-controls" data-stopdrag>
        <button className="topo-pz-btn" onClick={zoomBtn(1.25)} title="Zoom in">
          <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round"><path d="M12 5v14M5 12h14"/></svg>
        </button>
        <button className="topo-pz-btn" onClick={zoomBtn(0.8)} title="Zoom out">
          <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round"><path d="M5 12h14"/></svg>
        </button>
        <button className="topo-pz-btn" onClick={fit} title="Fit to screen">
          <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round"><path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/></svg>
        </button>
        <div className="topo-pz-zoom">{Math.round(view.z * 100)}%</div>
      </div>
      <div className="topo-pz-hint">drag to pan · scroll to zoom</div>
    </div>
  )
}

// ── Inspector ─────────────────────────────────────────
function Inspector({ node, isPinned, onClose }: { node: TopoNode | null; isPinned: boolean; onClose: () => void }) {
  if (!node) {
    return (
      <div className="topo-inspector topo-inspector-empty">
        <div className="topo-ins-body">
          <div className="topo-ins-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
              <circle cx="12" cy="12" r="9" /><path d="m8 12 3 3 5-6" />
            </svg>
          </div>
          <div className="topo-ins-title">Inspector</div>
          <div className="topo-ins-sub"><b>Hover</b> a node to inspect it. <b>Click</b> to pin. Click a <b>network</b> to drill in.</div>
        </div>
      </div>
    )
  }

  const c = PALETTE[node.kind as keyof typeof PALETTE] || PALETTE.net
  const kindLabel = { ws: 'Workspace', net: 'Remote Network', conn: 'Connector', shield: 'Shield' }[node.kind]
  const statusLabel = { ok: 'Healthy', warn: 'Warning', err: 'Critical', pend: 'Pending' }[node.status]
  const meta = node.meta || {}

  return (
    <div className="topo-inspector" data-stopdrag>
      <div className="topo-ins-strip" style={{ background: c }} />
      <div className="topo-ins-body">
        <div className="topo-ins-head">
          <div className="topo-ins-kind" style={{ color: c }}>
            <span className="topo-ins-dot" style={{ background: c }} />
            {kindLabel}
          </div>
          {isPinned ? (
            <button className="topo-ins-close" onClick={onClose} title="Unpin">
              <svg viewBox="0 0 24 24" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round">
                <path d="M18 6 6 18M6 6l12 12" />
              </svg>
            </button>
          ) : (
            <span className="topo-ins-preview-badge">preview</span>
          )}
        </div>
        <div className="topo-ins-title">{node.label}</div>
        <div className={`topo-ins-status topo-ins-status-${node.status}`}>
          <span className="topo-ins-status-dot" />
          {statusLabel}
        </div>
        <div className="topo-ins-meta">
          {Object.entries(meta).map(([k, v]) => (
            <div key={k} className="topo-ins-row">
              <div className="topo-ins-k">{k}</div>
              <div className="topo-ins-v">{String(v)}</div>
            </div>
          ))}
          {node.children && (
            <div className="topo-ins-row">
              <div className="topo-ins-k">children</div>
              <div className="topo-ins-v">{node.children.length}</div>
            </div>
          )}
        </div>
        <div className="topo-ins-hint">
          {isPinned
            ? <>Pinned · click empty space or <span className="topo-ins-kbd">Esc</span> to unpin</>
            : <>Click to pin · <span className="topo-ins-kbd">scroll</span> to zoom</>
          }
        </div>
      </div>
    </div>
  )
}

// ── Breadcrumbs ───────────────────────────────────────
function Crumbs({ focus, setFocus, tree }: { focus: FocusState; setFocus: (f: FocusState) => void; tree: TopoNode }) {
  const items: Array<{ key: string; label: string; color: string; onClick: () => void; active: boolean }> = [
    { key: 'home', label: tree.label, color: PALETTE.ws, onClick: () => setFocus({ kind: 'home', id: null }), active: focus.kind === 'home' }
  ]
  if (focus.kind === 'network' || focus.kind === 'connector') {
    const fnet = focus.kind === 'network'
      ? (tree.children?.find(n => n.id === focus.id) ?? null)
      : (findConnector(tree, focus.id)?.net ?? null)
    const fconn = focus.kind === 'connector'
      ? (findConnector(tree, focus.id)?.conn ?? null)
      : null
    if (fnet) items.push({ key: 'net', label: fnet.label, color: PALETTE.net, onClick: () => setFocus({ kind: 'network', id: fnet.id }), active: focus.kind === 'network' })
    if (fconn) items.push({ key: 'conn', label: fconn.label, color: PALETTE.conn, onClick: () => setFocus({ kind: 'connector', id: fconn.id }), active: focus.kind === 'connector' })
  }
  return (
    <div className="topo-crumbs" data-stopdrag>
      {items.map((it, i) => (
        <span key={it.key} style={{ display: 'inline-flex', alignItems: 'center', gap: 2 }}>
          {i > 0 && <span className="topo-crumb-sep">›</span>}
          <button className={`topo-crumb${it.active ? ' active' : ''}`} onClick={it.onClick}>
            <span className="topo-crumb-dot" style={{ background: it.color, boxShadow: `0 0 8px ${it.color}` }} />
            {it.label}
          </button>
        </span>
      ))}
    </div>
  )
}

// ── TopologyPage ──────────────────────────────────────
export default function TopologyPage() {
  const [focus, setFocus] = useState<FocusState>({ kind: 'home', id: null })
  const [selected, setSelected] = useState<TopoNode | null>(null)
  const [hovered, setHovered] = useState<string | null>(null)
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState('all')
  const [fitSignal, setFitSignal] = useState(0)

  const { data: networksData } = useQuery(GetRemoteNetworksDocument, { fetchPolicy: 'cache-and-network', pollInterval: 30000 })
  const { data: workspaceData } = useQuery(GetWorkspaceDocument)

  useEffect(() => { setFitSignal(s => s + 1) }, [focus])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        if (focus.kind === 'connector') {
          const cf = findConnector(tree, focus.id)
          setFocus(cf ? { kind: 'network', id: cf.net.id } : { kind: 'home', id: null })
        } else if (focus.kind === 'network') {
          setFocus({ kind: 'home', id: null })
        } else {
          setSelected(null)
        }
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [focus]) // eslint-disable-line react-hooks/exhaustive-deps

  const tree = useMemo<TopoNode>(() => {
    const networks = networksData?.remoteNetworks ?? []
    return {
      id: 'ws-zero',
      label: workspaceData?.workspace?.name ?? 'workspace',
      sub: 'workspace',
      kind: 'ws',
      status: 'ok',
      meta: { plan: 'Enterprise', slug: workspaceData?.workspace?.slug ?? '' },
      _x: 0, _y: 0, _ang: 0,
      children: networks.map(net => {
        return {
          id: net.id,
          label: net.name,
          kind: 'net' as TopoKind,
          status: mapNetStatus(net.networkHealth),
          meta: { location: net.location, status: net.status, created: net.createdAt },
          _x: 0, _y: 0, _ang: 0,
          children: net.connectors.map((conn) => ({
            id: conn.id,
            label: conn.name,
            kind: 'conn' as TopoKind,
            status: mapConnStatus(conn.status),
            meta: {
              host: conn.hostname ?? 'unknown',
              ver: conn.version ?? 'n/a',
              ip: conn.publicIp ?? 'n/a',
              lastSeen: conn.lastSeenAt ?? 'never',
            },
            _x: 0, _y: 0, _ang: 0,
            children: net.shields
              .filter(sh => sh.connectorId === conn.id)
              .map(sh => ({
                id: sh.id,
                label: sh.name,
                kind: 'shield' as TopoKind,
                status: mapShieldStatus(sh.status),
                meta: {
                  host: sh.hostname ?? 'unknown',
                  addr: sh.interfaceAddr ?? 'n/a',
                  lastSeen: sh.lastSeenAt ?? 'never',
                },
                _x: 0, _y: 0, _ang: 0,
              })),
          })),
        }
      }),
    }
  }, [networksData, workspaceData])

  const counts = topoCounts(tree)
  const hoveredNode = useMemo(() => findNode(tree, hovered), [tree, hovered])
  const displayedNode = selected ?? hoveredNode ?? null

  return (
    <div
      className="topo-stage"
      style={{ position: 'absolute', inset: '-1.5rem', overflow: 'hidden' }}
      onClick={() => setSelected(null)}
    >
      {/* HUD top bar */}
      <div className="topo-hud-top">
        {/* Brand */}
        <div className="topo-brand">
          <div className="topo-brand-mark">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
              <path d="M12 2 4 6v6c0 5 3.5 9 8 10 4.5-1 8-5 8-10V6l-8-4z" />
              <path d="m9 12 2 2 4-4" />
            </svg>
          </div>
          <div className="topo-brand-txt">
            <div className="topo-brand-name">Zecurity</div>
            <div className="topo-brand-sub">{workspaceData?.workspace?.slug ?? '…'} · topology</div>
          </div>
        </div>

        {/* Breadcrumbs */}
        <Crumbs focus={focus} setFocus={setFocus} tree={tree} />

        {/* Title */}
        <div className="topo-titlebar">
          <div className="topo-kicker">
            {focus.kind === 'home' ? 'Topology · overview' : focus.kind === 'network' ? 'Drilled · network shell' : 'Drilled · connector shell'}
          </div>
          <div className="topo-title">
            {focus.kind === 'home' ? 'Network Topology' : focus.kind === 'network' ? 'Network focus mode' : 'Connector focus mode'}
          </div>
        </div>

        <div style={{ flex: 1 }} />

        {/* Search */}
        <div className="topo-search-wrap" data-stopdrag>
          <svg className="topo-search-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round">
            <circle cx="11" cy="11" r="8" /><path d="m21 21-4.35-4.35" />
          </svg>
          <input
            className="topo-search"
            placeholder="Search nodes…"
            value={query}
            onChange={e => setQuery(e.target.value)}
            onClick={e => e.stopPropagation()}
          />
        </div>

        {/* Stat strip */}
        <div className="topo-stat-strip">
          <div className="topo-stat"><div className="topo-k">Nets</div><div className="topo-v">{counts.nets}</div></div>
          <div className="topo-stat"><div className="topo-k">Conns</div><div className="topo-v">{counts.conns}</div></div>
          <div className="topo-stat"><div className="topo-k">Shields</div><div className="topo-v">{counts.shields}</div></div>
          <div className="topo-stat">
            <div className="topo-k">Warn</div>
            <div className="topo-v" style={{ color: counts.warn ? PALETTE.warn : 'oklch(0.62 0.012 250)' }}>
              <span className="topo-dot-s" style={{ background: counts.warn ? PALETTE.warn : 'oklch(0.62 0.012 250)', boxShadow: counts.warn ? `0 0 8px ${PALETTE.warn}` : 'none' }} />
              {counts.warn}
            </div>
          </div>
        </div>

        {/* Filter chips */}
        <div className="topo-filter-chips" data-stopdrag>
          {(['all', 'ok', 'warn', 'pend'] as const).map(k => (
            <button key={k} className={`topo-chip${filter === k ? ' active' : ''}`}
              onClick={e => { e.stopPropagation(); setFilter(k) }}>
              {k === 'all' ? 'All' : k === 'ok' ? 'Healthy' : k === 'warn' ? 'Warn' : 'Pending'}
            </button>
          ))}
        </div>
      </div>

      {/* Legend */}
      <div className="topo-legend">
        <div className="topo-legend-head">Legend</div>
        {([['ws', 'workspace'], ['net', 'network'], ['conn', 'connector'], ['shield', 'shield']] as const).map(([kind, label]) => (
          <div key={kind} className="topo-legend-k">
            <span className="topo-legend-sw" style={{ color: PALETTE[kind] }} />
            {label}
          </div>
        ))}
      </div>

      {/* Canvas */}
      <PanZoom fitSignal={fitSignal}>
        <div onClick={e => e.stopPropagation()} style={{ width: WORLD_W, height: WORLD_H }}>
          <TopoGraph
            width={WORLD_W} height={WORLD_H}
            focus={focus} onFocusChange={setFocus}
            onNodeClick={setSelected} selectedId={selected?.id ?? null}
            hoveredId={hovered} onHover={setHovered}
            filterStatus={filter} searchQuery={query}
            tree={tree}
          />
        </div>
      </PanZoom>

      {/* Inspector */}
      <Inspector node={displayedNode} isPinned={!!selected} onClose={() => setSelected(null)} />
    </div>
  )
}
