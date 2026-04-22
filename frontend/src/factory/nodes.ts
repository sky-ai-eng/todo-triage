// Routing primitives placed along belts — splitters, mergers, and poles.
//
// Unlike stations (which own event semantics), these are pure routing:
// stations stay 1-in-1-out so every multi-target branch or multi-source
// convergence lives on one of these lightweight nodes. Items transit
// through without dwelling.
//
//   - splitter: 1 input side, up to 3 output sides. Diamond with outward
//     ticks. `orientation` names the input side (L/R/T/B); the other
//     three sides are outputs.
//   - merger: 3 input sides, 1 output side. Diamond with inward ticks.
//     `orientation` names the output side.
//   - pole: small round waypoint with 4 sides. Purely visual/routing,
//     no semantic. Used to bend belts at grid corners.

import { Container, Graphics, Text } from 'pixi.js'
import type { Port } from './station'

const ACCENT = 0xc47a5a
const TEXT_TERTIARY = 0xa09a94
const NODE_R = 24 // half-diagonal of the diamond
const POLE_R = 8
const TUNNEL_R = 10 // half-size of the tunnel endpoint square

export type NodeKind = 'splitter' | 'merger'
export type Side = 'left' | 'right' | 'top' | 'bottom'
export type TunnelRole = 'entrance' | 'exit'

export interface NodeOptions {
  kind: NodeKind
  center: { x: number; y: number }
  label?: string
  /** Input side for splitter, output side for merger. Data-only for now
   * — doesn't rotate the visual (diamond is symmetric and ticks are
   * drawn on all four sides). Used by the routing layer to identify
   * which port is input vs. output. */
  orientation?: Side
}

export interface NodeHandle {
  kind: NodeKind
  center: { x: number; y: number }
  leftPort: Port
  rightPort: Port
  topPort: Port
  bottomPort: Port
  update(dt: number): void
}

export interface PoleHandle {
  kind: 'pole'
  center: { x: number; y: number }
  leftPort: Port
  rightPort: Port
  topPort: Port
  bottomPort: Port
  update(dt: number): void
}

export interface TunnelOptions {
  role: TunnelRole
  /** Which side of the endpoint has the external-belt port. The opposite
   * side is where the tunnel "goes" (invisible). */
  side: Side
  center: { x: number; y: number }
}

export interface TunnelHandle {
  kind: 'tunnel_entrance' | 'tunnel_exit'
  center: { x: number; y: number }
  /** Exactly one of these four will be defined — the side specified by
   * `side` in TunnelOptions. Others are undefined. */
  leftPort?: Port
  rightPort?: Port
  topPort?: Port
  bottomPort?: Port
  update(dt: number): void
}

export function buildNode(parent: Container, opts: NodeOptions): NodeHandle {
  const { kind, center, label } = opts

  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  // Drop shadow — same stacked-fill approach as stations, scaled down.
  const shadow = new Graphics()
  shadow.moveTo(0, -NODE_R + 3)
  shadow.lineTo(NODE_R, 3)
  shadow.lineTo(0, NODE_R + 3)
  shadow.lineTo(-NODE_R, 3)
  shadow.closePath()
  shadow.fill({ color: 0x000000, alpha: 0.07 })
  root.addChild(shadow)

  // Body (diamond glass).
  const body = new Graphics()
  body.moveTo(0, -NODE_R)
  body.lineTo(NODE_R, 0)
  body.lineTo(0, NODE_R)
  body.lineTo(-NODE_R, 0)
  body.closePath()
  body.fill({ color: 0xffffff, alpha: 0.9 })
  root.addChild(body)

  // Warm tint.
  const tint = new Graphics()
  tint.moveTo(0, -NODE_R)
  tint.lineTo(NODE_R, 0)
  tint.lineTo(0, NODE_R)
  tint.lineTo(-NODE_R, 0)
  tint.closePath()
  tint.fill({ color: ACCENT, alpha: 0.1 })
  root.addChild(tint)

  // Inner highlight — a smaller diamond outline inside, catches "light."
  const IR = NODE_R - 6
  const inner = new Graphics()
  inner.moveTo(0, -IR)
  inner.lineTo(IR, 0)
  inner.lineTo(0, IR)
  inner.lineTo(-IR, 0)
  inner.closePath()
  inner.stroke({ width: 0.75, color: 0xffffff, alpha: 0.85 })
  root.addChild(inner)

  // Outer rim — accent-colored, hairline.
  const rim = new Graphics()
  rim.moveTo(0, -NODE_R)
  rim.lineTo(NODE_R, 0)
  rim.lineTo(0, NODE_R)
  rim.lineTo(-NODE_R, 0)
  rim.closePath()
  rim.stroke({ width: 1, color: ACCENT, alpha: 0.55 })
  root.addChild(rim)

  // Directional hint — for splitters draw outward tick marks from center,
  // for mergers draw inward. Subtle; helps disambiguate the two kinds at
  // a glance when ports are hidden.
  const hint = new Graphics()
  const tick = 4
  if (kind === 'splitter') {
    hint.moveTo(0, 0)
    hint.lineTo(tick, 0)
    hint.moveTo(0, 0)
    hint.lineTo(-tick, 0)
    hint.moveTo(0, 0)
    hint.lineTo(0, tick)
    hint.moveTo(0, 0)
    hint.lineTo(0, -tick)
  } else {
    hint.moveTo(tick, 0)
    hint.lineTo(0, 0)
    hint.moveTo(-tick, 0)
    hint.lineTo(0, 0)
    hint.moveTo(0, tick)
    hint.lineTo(0, 0)
    hint.moveTo(0, -tick)
    hint.lineTo(0, 0)
  }
  hint.stroke({ width: 1, color: ACCENT, alpha: 0.7 })
  root.addChild(hint)

  if (label) {
    const text = new Text({
      text: label,
      style: {
        fontFamily: 'Inter, system-ui, sans-serif',
        fontSize: 10,
        fontWeight: '500',
        fill: TEXT_TERTIARY,
        letterSpacing: 0.6,
      },
    })
    text.anchor.set(0.5, 0)
    text.y = NODE_R + 6
    root.addChild(text)
  }

  return {
    kind,
    center,
    leftPort: { x: center.x - NODE_R, y: center.y, dir: { x: -1, y: 0 } },
    rightPort: { x: center.x + NODE_R, y: center.y, dir: { x: 1, y: 0 } },
    topPort: { x: center.x, y: center.y - NODE_R, dir: { x: 0, y: -1 } },
    bottomPort: { x: center.x, y: center.y + NODE_R, dir: { x: 0, y: 1 } },
    update() {
      // No ambient animation yet — nodes are quiet. Hook in if we want
      // splitter-activating pulses when items route through.
    },
  }
}

export function buildTunnel(parent: Container, opts: TunnelOptions): TunnelHandle {
  // Tunnel endpoints — small rounded squares marking where a belt enters
  // or exits an "underground" routing segment. Entrance and exit are
  // drawn identically; the directional meaning is implicit from the
  // edges connected to them. Three stacked dashes inside suggest "layers
  // of depth" (the tunnel).
  //
  // Unlike poles (which have all four ports), a tunnel endpoint exposes
  // ONLY the side facing the external belt. The other three sides would
  // conflict with the invisible tunnel connection they're paired to.
  const { role, side, center } = opts

  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  const shadow = new Graphics()
  shadow.roundRect(-TUNNEL_R, -TUNNEL_R + 2, TUNNEL_R * 2, TUNNEL_R * 2, 3)
  shadow.fill({ color: 0x000000, alpha: 0.1 })
  root.addChild(shadow)

  const body = new Graphics()
  body.roundRect(-TUNNEL_R, -TUNNEL_R, TUNNEL_R * 2, TUNNEL_R * 2, 3)
  body.fill({ color: 0xffffff, alpha: 0.93 })
  body.stroke({ width: 0.75, color: ACCENT, alpha: 0.6 })
  root.addChild(body)

  // Tunnel-depth hint — three small dashes stacked, suggesting layered
  // darkness beneath. Orientation matches the tunnel axis: if the port
  // is on left/right the tunnel runs horizontally (dashes vertical), if
  // top/bottom the tunnel runs vertically (dashes horizontal).
  const dashes = new Graphics()
  const horizontal = side === 'left' || side === 'right'
  for (let i = -1; i <= 1; i++) {
    if (horizontal) {
      dashes.rect(-0.6, i * 3 - 0.6, 1.2, 1.2)
    } else {
      dashes.rect(i * 3 - 0.6, -0.6, 1.2, 1.2)
    }
  }
  dashes.fill({ color: ACCENT, alpha: 0.55 })
  root.addChild(dashes)

  // Small opening indicator on the port side — a thin gap in the border
  // suggesting "the belt comes in/out here."
  const opening = new Graphics()
  const OP_LEN = 7
  switch (side) {
    case 'left':
      opening.moveTo(-TUNNEL_R, -OP_LEN / 2)
      opening.lineTo(-TUNNEL_R, OP_LEN / 2)
      break
    case 'right':
      opening.moveTo(TUNNEL_R, -OP_LEN / 2)
      opening.lineTo(TUNNEL_R, OP_LEN / 2)
      break
    case 'top':
      opening.moveTo(-OP_LEN / 2, -TUNNEL_R)
      opening.lineTo(OP_LEN / 2, -TUNNEL_R)
      break
    case 'bottom':
      opening.moveTo(-OP_LEN / 2, TUNNEL_R)
      opening.lineTo(OP_LEN / 2, TUNNEL_R)
      break
  }
  opening.stroke({ width: 1.5, color: 0xffffff, alpha: 1 })
  root.addChild(opening)

  // Port position on the specified side.
  const port: Port = (() => {
    switch (side) {
      case 'left':
        return { x: center.x - TUNNEL_R, y: center.y, dir: { x: -1, y: 0 } }
      case 'right':
        return { x: center.x + TUNNEL_R, y: center.y, dir: { x: 1, y: 0 } }
      case 'top':
        return { x: center.x, y: center.y - TUNNEL_R, dir: { x: 0, y: -1 } }
      case 'bottom':
        return { x: center.x, y: center.y + TUNNEL_R, dir: { x: 0, y: 1 } }
    }
  })()

  return {
    kind: role === 'entrance' ? 'tunnel_entrance' : 'tunnel_exit',
    center,
    leftPort: side === 'left' ? port : undefined,
    rightPort: side === 'right' ? port : undefined,
    topPort: side === 'top' ? port : undefined,
    bottomPort: side === 'bottom' ? port : undefined,
    update() {},
  }
}

export function buildPole(parent: Container, center: { x: number; y: number }): PoleHandle {
  // Poles are pass-through waypoints that let belts bend at grid corners
  // without loading additional semantics onto stations. Visually they're
  // a small disc — subtle enough not to compete with stations but visible
  // enough to read as "the belt actually turns here."
  const root = new Container()
  root.x = center.x
  root.y = center.y
  parent.addChild(root)

  const shadow = new Graphics()
  shadow.circle(0, 1.5, POLE_R)
  shadow.fill({ color: 0x000000, alpha: 0.08 })
  root.addChild(shadow)

  const outer = new Graphics()
  outer.circle(0, 0, POLE_R)
  outer.fill({ color: 0xffffff, alpha: 0.95 })
  outer.stroke({ width: 0.75, color: ACCENT, alpha: 0.55 })
  root.addChild(outer)

  const inner = new Graphics()
  inner.circle(0, 0, 2)
  inner.fill({ color: ACCENT, alpha: 0.65 })
  root.addChild(inner)

  return {
    kind: 'pole',
    center,
    leftPort: { x: center.x - POLE_R, y: center.y, dir: { x: -1, y: 0 } },
    rightPort: { x: center.x + POLE_R, y: center.y, dir: { x: 1, y: 0 } },
    topPort: { x: center.x, y: center.y - POLE_R, dir: { x: 0, y: -1 } },
    bottomPort: { x: center.x, y: center.y + POLE_R, dir: { x: 0, y: 1 } },
    update() {},
  }
}
