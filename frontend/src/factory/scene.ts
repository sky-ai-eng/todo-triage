// First-slice factory POC: two stations + one canonical belt between them.
//
// Stations are built from a (FactoryEvent, FieldSchema[]) pair and rendered
// as procedural liquid-glass machines (see station.ts). The belt is a
// quadratic curve with small pulses flowing along it; items periodically
// emerge from the left station's right port, traverse the belt, and enter
// the right station's left port.
//
// Coordinate system is a fixed 1400x700 world. The Viewport handles pan
// (drag), zoom (wheel / pinch), and decelerated release; on mount it fits
// the world into whatever canvas size we're rendered into, centered.

import { Application, Container, Graphics, Text, Ticker } from 'pixi.js'
import { Viewport } from 'pixi-viewport'
import type { FieldSchema } from '../types'
import { FACTORY_EVENTS } from './events'
import { buildStation, BELT_WIDTH, type Port, type StationHandle } from './station'
import {
  buildNode,
  buildPole,
  buildTunnel,
  type NodeHandle,
  type PoleHandle,
  type TunnelHandle,
} from './nodes'

const COLOR_ACCENT = 0xc47a5a
/** Grid cell size. All primitives position their centers on integer grid
 * coordinates, so axis-aligned belts between them are guaranteed straight. */
const GRID = 80
const g = (col: number) => col * GRID
const SCENE_W = g(44)
const SCENE_H = g(18)

type Side = 'left' | 'right' | 'top' | 'bottom'
type GraphNode = StationHandle | NodeHandle | PoleHandle | TunnelHandle

type NodeDef =
  | { kind: 'station'; eventType: string; col: number; row: number }
  | { kind: 'splitter' | 'merger'; col: number; row: number; label?: string; orientation?: Side }
  | { kind: 'pole'; col: number; row: number }
  | {
      kind: 'tunnel_entrance' | 'tunnel_exit'
      col: number
      row: number
      side: Side
    }

// Graph — stations carry event semantics, splitters/mergers are pure
// routing, poles are pass-through waypoints. All centers sit on integer
// grid cells (cell size = GRID = 80px), so axis-aligned belts between
// same-row or same-column primitives are guaranteed perfectly straight.
//
// Layout:
//  - Main line runs across row 6. Build branch dips to row 8 via poles +
//    a merger right before new_commits; ci_failed sits at row 10.
//  - CI-failed loopback wraps through poles at row 12 (below everything)
//    to reach new_commits from the left without sharing any port.
//  - Direct-approval arc is still splitter A.top → merger_main.top.
//  - Review-outcome branches hang off splitter B at col 36.
const NODE_DEFS: NodeDef[] = [
  { kind: 'station', eventType: 'github:pr:opened', col: 4, row: 6 }, //                     0
  { kind: 'station', eventType: 'github:pr:ready_for_review', col: 8, row: 6 }, //           1
  { kind: 'splitter', label: 'next?', col: 12, row: 6, orientation: 'left' }, //             2
  { kind: 'merger', col: 13, row: 8, orientation: 'right' }, //                              3  merger_nc
  { kind: 'station', eventType: 'github:pr:new_commits', col: 16, row: 8 }, //               4
  { kind: 'splitter', label: 'ci?', col: 20, row: 8, orientation: 'left' }, //               5  shifted right 1
  { kind: 'station', eventType: 'github:pr:conflicts', col: 23, row: 5 }, //                 6  up 1, right 1
  { kind: 'station', eventType: 'github:pr:ci_check_passed', col: 23, row: 8 }, //           7  shifted right 1
  { kind: 'station', eventType: 'github:pr:ci_check_failed', col: 23, row: 11 }, //          8  shifted right 1
  { kind: 'pole', col: 26, row: 11 }, //                                                     9  east of ci_failed (right 1)
  { kind: 'pole', col: 26, row: 13 }, //                                                     10 south of pole 9 (right 1)
  { kind: 'pole', col: 13, row: 13 }, //                                                     11 west of pole 10, below merger_nc
  // Review cycle moved UP to row 2. Flow goes: merger_main → review_requested
  // → review_received, then loops back down via poles (35, 36, 37, 38) to
  // splitter_B. This frees row 6 horizontally (saving ~6 cols) and lifts
  // the skip-build arc, so conflicts sits at row 4 without needing a tunnel.
  { kind: 'merger', col: 26, row: 4, orientation: 'right' }, //                              12 merger_main (up 1)
  { kind: 'station', eventType: 'github:pr:review_requested', col: 29, row: 4 }, //          13 up 1
  { kind: 'station', eventType: 'github:pr:review_received', col: 33, row: 4 }, //           14 up 1
  { kind: 'splitter', label: 'review result', col: 30, row: 11, orientation: 'left' }, //    15 row 6→11 to match review_approved
  { kind: 'pole', col: 30, row: 8 }, //                                                      16 E-shape top corner (review_commented row)
  { kind: 'pole', col: 30, row: 14 }, //                                                     17 E-shape bottom corner (changes_requested row)
  { kind: 'station', eventType: 'github:pr:review_commented', col: 34, row: 8 }, //          18 down 4, spaced 3 from approved
  { kind: 'station', eventType: 'github:pr:review_approved', col: 34, row: 11 }, //          19 down 5 (middle)
  { kind: 'station', eventType: 'github:pr:review_changes_requested', col: 34, row: 14 }, // 20 down 6, spaced 3 from approved
  { kind: 'station', eventType: 'github:pr:merged', col: 40, row: 11 }, //                   21 aligned with review_approved row 11
  { kind: 'station', eventType: 'github:pr:closed', col: 40, row: 16 }, //                   22 closed-bus terminal (bus moved down 2)

  // ─── Closed-bus infrastructure ─────────────────────────────────────────────
  { kind: 'splitter', label: 'continue?', col: 11, row: 6, orientation: 'left' }, //         23 s_rfr (moved right 1)
  { kind: 'splitter', label: 'continue?', col: 19, row: 8, orientation: 'left' }, //         24 s_commits (right 1)
  { kind: 'splitter', label: 'respond?', col: 37, row: 14, orientation: 'left' }, //         25 s_changes (down to changes_requested row)
  { kind: 'pole', col: 11, row: 16 }, //                                                     26 bus entry (moved right 1, down 2)
  { kind: 'merger', col: 19, row: 16, orientation: 'right' }, //                             27 bus + new_commits drop (aligned with s_commits col 19)
  { kind: 'pole', col: 38, row: 8 }, //                                                      28 review_commented east-turn (east of review_commented row 8)
  { kind: 'merger', col: 38, row: 16, orientation: 'right' }, //                             29 bus + review_commented drop
  { kind: 'merger', col: 37, row: 16, orientation: 'right' }, //                             30 bus + changes_requested drop

  // ─── changes_requested retry path ───────────────────────────────────────────
  // Vertical tunnel from s_changes straight up to near retry pole 31.
  // The old row-2 horizontal tunnel is now a regular belt (nothing in the
  // way anymore since the review chain moved down to row 5).
  { kind: 'pole', col: 37, row: 2 }, //                                                      31 retry: east end, turns south→west
  { kind: 'tunnel_entrance', col: 37, row: 12, side: 'bottom' }, //                          32 vertical tunnel entrance (near s_changes)
  { kind: 'tunnel_exit', col: 37, row: 4, side: 'top' }, //                                  33 vertical tunnel exit (near retry pole 31)
  { kind: 'pole', col: 13, row: 2 }, //                                                      34 retry: west end, turns east→south into merger_nc.top

  // ─── Review-cycle return path (row 2 → row 3 → row 6) ──────────────────────
  // Loop-back poles route review_received.right south then west, finally
  // east into splitter_B.left. Two turns at the top (east→south→west),
  // two at the left (west→south→east).
  { kind: 'pole', col: 36, row: 4 }, //                                                      35 Pole_R1: up 1 to match review_received
  { kind: 'pole', col: 36, row: 6 }, //                                                      36 Pole_R2: up 1
  { kind: 'pole', col: 28, row: 6 }, //                                                      37 Pole_R3: up 1
  { kind: 'pole', col: 28, row: 11 }, //                                                     38 Pole_R4: aligned with splitter_B row 11, feeds splitter_B.left horizontally

  // ─── splitter_A top path (replaces direct arc to merger_main.top) ──────────
  // splitter_A.top goes up to pole A, east across to pole B (2 rows above
  // merger_main), then down into merger_main.top. Keeps the top path
  // axis-aligned with two gentle 90° bends.
  { kind: 'pole', col: 12, row: 3 }, //                                                      39 pole A above splitter_A
  { kind: 'pole', col: 26, row: 3 }, //                                                      40 pole B above merger_main

  // ─── Tunnel on the review_commented drop where it crosses approved→merged ─
  // Review_commented.right → east-turn pole 28 → VERTICAL DROP at col 38 →
  // bus merger 2. With merged now at row 11, the approved→merged belt
  // crosses col 38 row 11 — this tunnel lets the drop pass under it.
  { kind: 'tunnel_entrance', col: 38, row: 10, side: 'top' }, //                             41 above approved→merged belt
  { kind: 'tunnel_exit', col: 38, row: 12, side: 'bottom' }, //                              42 below approved→merged belt

  // ─── Conflicts retry: routes up to the row-2 retry line ─────────────────────
  // Conflicts.right → east-turn pole → vertical up to a new merger on the
  // retry line. The merger sits mid-line, splitting the old direct belt
  // from pole 31 → pole 34 into two segments with conflicts joining from
  // below.
  { kind: 'pole', col: 27, row: 5 }, //                                                      43 conflicts east-turn pole
  { kind: 'merger', col: 27, row: 2, orientation: 'left' }, //                               44 retry-line merger (conflicts drops in from south)
]

interface EdgeDef {
  from: number
  to: number
  fromSide: Side
  toSide: Side
}

const BELT_EDGES: EdgeDef[] = [
  // ─── Main flow row 6 ───────────────────────────────────────────────────────
  { from: 0, to: 1, fromSide: 'right', toSide: 'left' }, //   opened → rfr
  { from: 1, to: 23, fromSide: 'right', toSide: 'left' }, //  rfr → s_rfr
  { from: 23, to: 2, fromSide: 'right', toSide: 'left' }, //  s_rfr.right → splitter A
  { from: 23, to: 26, fromSide: 'bottom', toSide: 'top' }, // s_rfr.bottom → bus entry pole (abandon drop)
  { from: 2, to: 3, fromSide: 'bottom', toSide: 'left' }, //  A.bottom → merger_nc  (dip into build branch)
  // splitter_A top path: up to pole 39, east to pole 40, down to merger_main.top
  { from: 2, to: 39, fromSide: 'top', toSide: 'bottom' }, //  A.top → pole 39.bottom  (vertical up)
  { from: 39, to: 40, fromSide: 'right', toSide: 'left' }, // pole 39 → pole 40  (horizontal east at row 3)
  { from: 40, to: 12, fromSide: 'bottom', toSide: 'top' }, // pole 40 → merger_main.top  (vertical down)
  { from: 3, to: 4, fromSide: 'right', toSide: 'left' }, //   merger_nc → new_commits
  { from: 4, to: 24, fromSide: 'right', toSide: 'left' }, //  new_commits → s_commits
  { from: 24, to: 5, fromSide: 'right', toSide: 'left' }, //  s_commits.right → ci_splitter
  { from: 24, to: 27, fromSide: 'bottom', toSide: 'top' }, // s_commits.bottom → bus merger (abandon drop)

  // ─── CI branch (row 8 + loopback) ──────────────────────────────────────────
  // Conflicts is now reachable via a direct diagonal belt from ci_splitter.top.
  // The old tunnel was needed because the skip-build arc was at the same row
  // as conflicts; with merger_main moved up to row 2, the arc lifts and
  // conflicts drops to row 4, clearing the path.
  { from: 5, to: 6, fromSide: 'top', toSide: 'left' }, //     ci_splitter.top → conflicts.left (direct diagonal)
  { from: 5, to: 7, fromSide: 'right', toSide: 'left' }, //   ci_splitter → ci_passed
  { from: 5, to: 8, fromSide: 'bottom', toSide: 'left' }, //  ci_splitter.bottom → ci_failed
  { from: 7, to: 12, fromSide: 'right', toSide: 'bottom' }, // ci_passed → merger_main.bottom (climb up to row 2)
  { from: 8, to: 9, fromSide: 'right', toSide: 'left' }, //   ci_failed.right → pole(9)
  { from: 9, to: 10, fromSide: 'bottom', toSide: 'top' }, //  pole(9) → pole(10)
  { from: 10, to: 11, fromSide: 'left', toSide: 'right' }, // pole(10) → pole(11)
  { from: 11, to: 3, fromSide: 'top', toSide: 'bottom' }, //  pole(11) → merger_nc.bottom

  // ─── Review cycle row 2 + loop-back via poles ──────────────────────────────
  // merger_main → review_requested → review_received on row 2, then loop
  // back down through Pole_R1..R4 into splitter_B.left on row 6.
  { from: 12, to: 13, fromSide: 'right', toSide: 'left' }, // merger_main → review_requested
  { from: 13, to: 14, fromSide: 'right', toSide: 'left' }, // review_requested → review_received
  { from: 14, to: 35, fromSide: 'right', toSide: 'left' }, // review_received → Pole_R1 (row 2 east)
  { from: 35, to: 36, fromSide: 'bottom', toSide: 'top' }, // Pole_R1 → Pole_R2 (south, row 2→3)
  { from: 36, to: 37, fromSide: 'left', toSide: 'right' }, // Pole_R2 → Pole_R3 (west along row 3)
  { from: 37, to: 38, fromSide: 'bottom', toSide: 'top' }, // Pole_R3 → Pole_R4 (south, row 3→6)
  { from: 38, to: 15, fromSide: 'right', toSide: 'left' }, // Pole_R4 → splitter_B.left (east into splitter)

  // ─── E-shape review outcomes ───────────────────────────────────────────────
  { from: 15, to: 16, fromSide: 'top', toSide: 'bottom' }, // B.top → top pole
  { from: 16, to: 18, fromSide: 'right', toSide: 'left' }, // top pole → review_commented
  { from: 15, to: 19, fromSide: 'right', toSide: 'left' }, // B.right → review_approved
  { from: 15, to: 17, fromSide: 'bottom', toSide: 'top' }, // B.bottom → bottom pole
  { from: 17, to: 20, fromSide: 'right', toSide: 'left' }, // bottom pole → changes_requested
  { from: 19, to: 21, fromSide: 'right', toSide: 'left' }, // review_approved → merged

  // ─── Closed-bus chain ──────────────────────────────────────────────────────
  { from: 18, to: 28, fromSide: 'right', toSide: 'left' }, //  review_commented → east-turn pole
  // Drop passes under the approved→merged belt at row 11 via a vertical tunnel.
  { from: 28, to: 41, fromSide: 'bottom', toSide: 'top' }, //  east-turn pole → tunnel entrance (short vertical)
  { from: 41, to: 42, fromSide: 'top', toSide: 'bottom' }, //  vertical tunnel (invisible, crosses row 11)
  { from: 42, to: 29, fromSide: 'bottom', toSide: 'top' }, //  tunnel exit → bus merger (drop continues south)
  { from: 20, to: 25, fromSide: 'right', toSide: 'left' }, //  changes_requested → s_changes
  { from: 25, to: 30, fromSide: 'bottom', toSide: 'top' }, //  s_changes.bottom → bus merger (abandon drop)
  { from: 26, to: 27, fromSide: 'right', toSide: 'left' }, //  bus entry pole → bus merger 1
  { from: 27, to: 29, fromSide: 'right', toSide: 'left' }, //  bus merger 1 → bus merger 2
  { from: 29, to: 30, fromSide: 'right', toSide: 'left' }, //  bus merger 2 → bus merger 3
  { from: 30, to: 22, fromSide: 'right', toSide: 'left' }, //  bus merger 3 → closed

  // ─── changes_requested retry loopback ──────────────────────────────────────
  // New: vertical tunnel from s_changes straight up to near retry pole 31.
  // Row-2 horizontal is now a regular belt (review chain moved, nothing
  // in the way).
  { from: 25, to: 32, fromSide: 'top', toSide: 'bottom' }, //  s_changes.top → tunnel entrance (below, col 37)
  { from: 32, to: 33, fromSide: 'bottom', toSide: 'top' }, //  vertical tunnel (invisible, col 37 rows 12→4)
  { from: 33, to: 31, fromSide: 'top', toSide: 'bottom' }, //  tunnel exit → retry pole 31 (short vertical up)
  // Retry line is split by merger 44 so conflicts can join from below.
  { from: 31, to: 44, fromSide: 'left', toSide: 'right' }, //  retry pole 31 → merger 44 (row 2 west)
  { from: 44, to: 34, fromSide: 'left', toSide: 'right' }, //  merger 44 → retry end pole 34 (row 2 west)
  { from: 34, to: 3, fromSide: 'bottom', toSide: 'top' }, //   retry end pole → merger_nc.top
  // Conflicts routes up to the retry line.
  { from: 6, to: 43, fromSide: 'right', toSide: 'left' }, //   conflicts.right → pole 43 (horizontal east)
  { from: 43, to: 44, fromSide: 'top', toSide: 'bottom' }, //  pole 43.top → merger 44.bottom (vertical north to retry line)
]

function getPort(node: GraphNode, side: Side): Port | undefined {
  switch (side) {
    case 'left':
      return node.leftPort
    case 'right':
      return node.rightPort
    case 'top':
      return node.topPort
    case 'bottom':
      return node.bottomPort
  }
}

interface Belt {
  pointAt: (t: number) => { x: number; y: number }
}

interface SceneHandle {
  destroy: () => void
}

/** Predicate-field schemas keyed by event_type — what `GET /api/event-schemas` returns. */
export type SchemaIndex = Record<string, { event_type: string; fields: FieldSchema[] }>

export async function createFactoryScene(
  container: HTMLDivElement,
  schemas: SchemaIndex,
): Promise<SceneHandle> {
  const app = new Application()
  await app.init({
    resizeTo: container,
    background: 0xf7f5f2,
    antialias: true,
    resolution: window.devicePixelRatio || 1,
    autoDensity: true,
  })
  container.appendChild(app.canvas)

  const scene = new Viewport({
    screenWidth: app.screen.width,
    screenHeight: app.screen.height,
    worldWidth: SCENE_W,
    worldHeight: SCENE_H,
    events: app.renderer.events,
  })
  app.stage.addChild(scene)

  // Pan (drag), mouse-wheel zoom, touchpad pinch, inertia on release.
  // Clamping the zoom keeps users from zooming into the pixel grid or out
  // to where the world is a speck.
  scene
    .drag()
    .pinch()
    .wheel({ smooth: 5, percent: 0.1 })
    .decelerate({ friction: 0.94 })
    .clampZoom({ minScale: 0.35, maxScale: 3 })

  const fitAndCenter = () => {
    scene.resize(app.screen.width, app.screen.height, SCENE_W, SCENE_H)
    scene.fit(true, SCENE_W, SCENE_H)
    scene.moveCenter(SCENE_W / 2, SCENE_H / 2)
  }
  fitAndCenter()
  const ro = new ResizeObserver(() => {
    // Preserve the user's current center/scale on resize — refitting mid-
    // interaction would fight with their pan/zoom intent.
    scene.resize(app.screen.width, app.screen.height, SCENE_W, SCENE_H)
  })
  ro.observe(container)

  // Build all graph nodes at their grid-defined positions.
  const nodes: GraphNode[] = NODE_DEFS.map((def): GraphNode => {
    const center = { x: g(def.col), y: g(def.row) }
    switch (def.kind) {
      case 'station':
        return buildStation(scene, {
          event: FACTORY_EVENTS[def.eventType],
          fields: schemas[def.eventType]?.fields ?? [],
          enabled: true,
          center,
        })
      case 'pole':
        return buildPole(scene, center)
      case 'tunnel_entrance':
      case 'tunnel_exit':
        return buildTunnel(scene, {
          role: def.kind === 'tunnel_entrance' ? 'entrance' : 'exit',
          side: def.side,
          center,
        })
      case 'splitter':
      case 'merger':
        return buildNode(scene, {
          kind: def.kind,
          center,
          label: def.label,
          orientation: def.orientation,
        })
    }
  })

  // Build belts from the edge list. Each edge names the source and target
  // sides explicitly so splitter/merger multi-port wiring is unambiguous.
  const edges = BELT_EDGES.map((def) => {
    const fromNode = nodes[def.from]
    const toNode = nodes[def.to]
    const fromPort = getPort(fromNode, def.fromSide)
    const toPort = getPort(toNode, def.toSide)
    if (!fromPort || !toPort) {
      throw new Error(
        `Edge ${def.from} → ${def.to} references a missing port (${def.fromSide}/${def.toSide})`,
      )
    }
    // Tunnel edges connect an entrance to its exit — rendered invisibly
    // so items appear to dive under and pop back up. Item alpha handles
    // the fade-out / fade-in during the (unrendered) traversal.
    const isTunnel = fromNode.kind === 'tunnel_entrance' && toNode.kind === 'tunnel_exit'
    return {
      from: def.from,
      to: def.to,
      belt: buildBelt(scene, fromPort, toPort, {
        visible: !isTunnel,
        tunnel: isTunnel,
      }),
      isTunnel,
    }
  })
  // Chevrons only on visible belts — tunnel segments have no surface.
  const chevronLayers = edges
    .filter((e) => !e.isTunnel)
    .map((e) => buildBeltChevrons(scene, e.belt))
  const items = buildItemSpawner(scene, nodes, edges)

  const tick = (t: Ticker) => {
    const dt = t.deltaMS / 1000
    for (const n of nodes) n.update(dt)
    for (const ch of chevronLayers) ch.update(dt)
    items.update(dt)
  }
  app.ticker.add(tick)

  return {
    destroy: () => {
      app.ticker.remove(tick)
      ro.disconnect()
      app.destroy(true, { children: true, texture: true })
    },
  }
}

// ─── Belt ────────────────────────────────────────────────────────────────────

interface FlatBelt extends Belt {
  tangentAt: (t: number) => { x: number; y: number }
  length: number
}

function buildBelt(
  parent: Container,
  from: Port,
  to: Port,
  opts: { visible?: boolean; tunnel?: boolean } = {},
): FlatBelt {
  const { visible = true, tunnel = false } = opts
  // S-curve conveyor — cubic bezier that exits `from` tangent to the port's
  // outward direction and arrives at `to` tangent to -to.dir (i.e. moving
  // INTO the port). This makes belts emerge smoothly from horizontal port
  // stubs no matter where the target station sits, and gives shared-port
  // situations clear directional read: two belts at the same port have
  // opposite tangents and their chevrons/items flow in opposite directions.
  //
  // Polygon rendering samples the curve N times, computes a CCW-perpendicular
  // edge offset at each sample, and stitches near/far edge strips into a
  // filled body + separate highlight/shadow strokes. Perpendicular is
  // recomputed per-sample so the belt width stays uniform under curvature;
  // lighting consistency (highlight always on one specific geometric side)
  // is a tradeoff we accept against width distortion.
  const c = new Container()
  parent.addChild(c)

  const dist = Math.hypot(to.x - from.x, to.y - from.y)
  // Offset magnitude for the control points. For opposite-facing collinear
  // ports (the common station-to-station horizontal case), the curve
  // overshoots and S-loops when `offset > dist/2` because P1 ends up past
  // P3 along the flow axis. Keep offset strictly below half the distance
  // to guarantee no overshoot, and cap at 160 so long arcs don't balloon.
  // Short belts naturally become near-straight, which is exactly what we
  // want for close primitives on the same row or column.
  const offset = Math.min(160, dist * 0.4)

  // Regular belts have OUTWARD control points (item leaves source going
  // in port.dir, enters destination going in -port.dir). Tunnel edges
  // flip this: items ENTER the tunnel through the source's external port
  // (heading inward, opposite to port.dir) and EXIT through the
  // destination's external port (also inward from the tunnel's
  // perspective). Inverting the offset sign gives tangents that track
  // the actual flow direction instead of swooping back out through the
  // external ports — otherwise the bezier loops the item around before
  // fading in/out.
  const offsetSign = tunnel ? -1 : 1
  const P0 = { x: from.x, y: from.y }
  const P1 = {
    x: from.x + from.dir.x * offset * offsetSign,
    y: from.y + from.dir.y * offset * offsetSign,
  }
  const P2 = {
    x: to.x + to.dir.x * offset * offsetSign,
    y: to.y + to.dir.y * offset * offsetSign,
  }
  const P3 = { x: to.x, y: to.y }

  const pointAt = (t: number) => {
    const u = 1 - t
    return {
      x: u * u * u * P0.x + 3 * u * u * t * P1.x + 3 * u * t * t * P2.x + t * t * t * P3.x,
      y: u * u * u * P0.y + 3 * u * u * t * P1.y + 3 * u * t * t * P2.y + t * t * t * P3.y,
    }
  }

  const tangentAt = (t: number) => {
    const u = 1 - t
    return {
      x: 3 * u * u * (P1.x - P0.x) + 6 * u * t * (P2.x - P1.x) + 3 * t * t * (P3.x - P2.x),
      y: 3 * u * u * (P1.y - P0.y) + 6 * u * t * (P2.y - P1.y) + 3 * t * t * (P3.y - P2.y),
    }
  }

  // Sample the curve and compute perpendicular edge points.
  const N = 28
  const hw = BELT_WIDTH / 2
  const nearPts: Array<{ x: number; y: number }> = []
  const farPts: Array<{ x: number; y: number }> = []
  let length = 0
  let prev: { x: number; y: number } | null = null

  for (let i = 0; i <= N; i++) {
    const t = i / N
    const p = pointAt(t)
    const tan = tangentAt(t)
    const tlen = Math.hypot(tan.x, tan.y) || 1
    // CCW-perpendicular to tangent. Consistent throughout the curve so the
    // polygon doesn't self-intersect; may not always point toward smaller y.
    const nx = tan.y / tlen
    const ny = -tan.x / tlen
    nearPts.push({ x: p.x + nx * hw, y: p.y + ny * hw })
    farPts.push({ x: p.x - nx * hw, y: p.y - ny * hw })

    if (prev) length += Math.hypot(p.x - prev.x, p.y - prev.y)
    prev = p
  }

  // Only render visible belt geometry when requested. Tunnel edges pass
  // visible=false — they still need pointAt/tangentAt for item movement,
  // but no surface is drawn (items fade to invisible during traversal).
  if (visible) {
    // Body polygon: near edge forward, far edge reverse, close.
    const body = new Graphics()
    body.moveTo(nearPts[0].x, nearPts[0].y)
    for (let i = 1; i <= N; i++) body.lineTo(nearPts[i].x, nearPts[i].y)
    for (let i = N; i >= 0; i--) body.lineTo(farPts[i].x, farPts[i].y)
    body.closePath()
    body.fill({ color: 0xffffff, alpha: 0.82 })
    c.addChild(body)

    // Warm tint (same shape).
    const tint = new Graphics()
    tint.moveTo(nearPts[0].x, nearPts[0].y)
    for (let i = 1; i <= N; i++) tint.lineTo(nearPts[i].x, nearPts[i].y)
    for (let i = N; i >= 0; i--) tint.lineTo(farPts[i].x, farPts[i].y)
    tint.closePath()
    tint.fill({ color: COLOR_ACCENT, alpha: 0.1 })
    c.addChild(tint)

    // Near-edge highlight stroke along the curve.
    const topEdge = new Graphics()
    topEdge.moveTo(nearPts[0].x, nearPts[0].y)
    for (let i = 1; i <= N; i++) topEdge.lineTo(nearPts[i].x, nearPts[i].y)
    topEdge.stroke({ width: 1.25, color: 0xffffff, alpha: 0.95 })
    c.addChild(topEdge)

    // Far-edge shadow stroke along the curve.
    const botEdge = new Graphics()
    botEdge.moveTo(farPts[0].x, farPts[0].y)
    for (let i = 1; i <= N; i++) botEdge.lineTo(farPts[i].x, farPts[i].y)
    botEdge.stroke({ width: 1.25, color: 0x000000, alpha: 0.18 })
    c.addChild(botEdge)
  }

  return { pointAt, tangentAt, length }
}

function buildBeltChevrons(parent: Container, belt: FlatBelt) {
  // Small ">" marks along the belt that slide in the flow direction. Each
  // chevron re-rotates every frame to match the local belt tangent at its
  // current t, so on curved belts the chevrons lean along the curve
  // naturally. Spacing is length-based so long and short belts look
  // consistent rather than scaling mark density with distance.
  const container = new Container()
  parent.addChild(container)

  const CHEVRON_SPACING = 36
  const count = Math.max(2, Math.round(belt.length / CHEVRON_SPACING))

  const chevrons = Array.from({ length: count }, () => {
    const g = new Graphics()
    g.moveTo(-4, -3)
    g.lineTo(1, 0)
    g.lineTo(-4, 3)
    g.stroke({ width: 1.25, color: COLOR_ACCENT, alpha: 0.55 })
    container.addChild(g)
    return g
  })

  let t = 0
  const speed = 0.32

  return {
    update(dt: number) {
      t = (t + dt * speed) % 1
      for (let i = 0; i < chevrons.length; i++) {
        const u = (t + i / chevrons.length) % 1
        const pos = belt.pointAt(u)
        const tan = belt.tangentAt(u)
        chevrons[i].x = pos.x
        chevrons[i].y = pos.y
        chevrons[i].rotation = Math.atan2(tan.y, tan.x)
        const fade = Math.min(1, u / 0.08, (1 - u) / 0.08)
        chevrons[i].alpha = 0.25 + 0.4 * Math.max(0, fade)
      }
    },
  }
}

// ─── Items ───────────────────────────────────────────────────────────────────

interface Edge {
  from: number
  to: number
  belt: FlatBelt
  isTunnel: boolean
}

function buildItemSpawner(parent: Container, nodes: GraphNode[], edges: Edge[]) {
  // Items are scripted visitors: each spawns with a list of station indices
  // to traverse in order. Pathfinding (next-hop shortest-path BFS) handles
  // getting from one scripted stop to the next — intermediate splitter/
  // merger nodes are transited automatically.
  //
  // This is deliberately the same primitive we'll use for real events:
  // an entity's script IS its event history. In the real-event model a
  // new event for entity X appends to its script and the item animates
  // from its current position to that next stop. The demo pre-generates
  // weighted scripts so we see varied journeys including CI retry loops,
  // direct approvals, and the review-outcome branches.
  //
  // Behaviour at each arrival:
  //   - arrived node matches the next scripted stop (station): dwell
  //     briefly, then advance script, then pathfind to the new next stop
  //   - arrived node is transit (splitter / merger): pathfind toward the
  //     current scripted stop and keep going, no dwell
  //   - reached the last scripted stop: dwell briefly, then despawn
  const layer = new Container()
  parent.addChild(layer)

  const LEG_DURATION = 2.8
  const DWELL = 0.55
  const MAX_HOPS = 40

  const outgoingByNode = new Map<number, Edge[]>()
  for (const e of edges) {
    const list = outgoingByNode.get(e.from) ?? []
    list.push(e)
    outgoingByNode.set(e.from, list)
  }

  // Precompute next-hop: for every (from, to) pair, the first edge to take
  // when traveling from `from` to reach `to` along a shortest path.
  // Cycles in the graph (the ci_failed → new_commits loopback) are handled
  // correctly by BFS — visited-set prevents re-expansion.
  const nextHop = new Map<string, Edge>()
  for (let src = 0; src < nodes.length; src++) {
    const firstEdge = new Map<number, Edge>()
    const visited = new Set<number>([src])
    const queue: number[] = [src]
    while (queue.length > 0) {
      const curr = queue.shift()!
      const outs = outgoingByNode.get(curr) ?? []
      for (const e of outs) {
        if (visited.has(e.to)) continue
        visited.add(e.to)
        const rootEdge = curr === src ? e : firstEdge.get(curr)!
        firstEdge.set(e.to, rootEdge)
        queue.push(e.to)
      }
    }
    for (const [tgt, e] of firstEdge) {
      nextHop.set(`${src}:${tgt}`, e)
    }
  }

  const edgeToward = (fromNode: number, target: number): Edge | null => {
    if (fromNode === target) return null
    return nextHop.get(`${fromNode}:${target}`) ?? null
  }

  // Weighted scripted journeys — each is a list of station node indices.
  // Pathfinding handles transitions through splitter / merger / pole nodes
  // between consecutive scripted stops. Station indices in the current
  // graph:
  //   0 opened · 1 rfr · 4 new_commits · 6 conflicts · 7 ci_passed
  //   8 ci_failed · 13 review_requested · 14 review_received
  //   18 review_commented · 19 review_approved · 20 changes_requested
  //   21 merged · 22 closed
  const SCRIPTS: { weight: number; stops: number[] }[] = [
    { weight: 18, stops: [0, 1, 4, 7, 13, 14, 19, 21] }, //                     happy path
    { weight: 15, stops: [0, 1, 4, 8, 4, 7, 13, 14, 19, 21] }, //               CI retry once
    { weight: 6, stops: [0, 1, 4, 8, 4, 8, 4, 7, 13, 14, 19, 21] }, //          CI retry twice
    { weight: 6, stops: [0, 1, 13, 14, 19, 21] }, //                            direct approval (skip build via arc)
    { weight: 8, stops: [0, 1, 4, 7, 13, 14, 20] }, //                          changes requested (not addressed)
    { weight: 8, stops: [0, 1, 4, 7, 13, 14, 20, 4, 7, 13, 14, 19, 21] }, //    changes requested → retry → approved (retry loopback!)
    { weight: 6, stops: [0, 1, 4, 7, 13, 14, 20, 22] }, //                      changes requested → abandoned (via closed bus)
    { weight: 6, stops: [0, 1, 4, 7, 13, 14, 18] }, //                          review commented (terminal)
    { weight: 6, stops: [0, 1, 4, 7, 13, 14, 18, 22] }, //                      review commented → abandoned (via closed bus)
    { weight: 5, stops: [0, 1, 4, 6] }, //                                      merge conflicts (abandoned there)
    { weight: 6, stops: [0, 1, 4, 6, 4, 7, 13, 14, 19, 21] }, //                 merge conflicts → retry → merged
    { weight: 5, stops: [0, 1, 22] }, //                                        abandoned at rfr (via closed bus)
    { weight: 6, stops: [0, 1, 4, 22] }, //                                     abandoned at new_commits (via closed bus)
  ]
  const totalScriptWeight = SCRIPTS.reduce((s, x) => s + x.weight, 0)
  const pickScript = (): number[] => {
    let r = Math.random() * totalScriptWeight
    for (const s of SCRIPTS) {
      r -= s.weight
      if (r <= 0) return s.stops
    }
    return SCRIPTS[0].stops
  }

  interface Item {
    gfx: Container
    script: number[]
    /** Index of the NEXT scripted stop to reach. scriptIdx-1 is the stop
     * we most recently arrived at (or script[0] if we haven't moved yet). */
    scriptIdx: number
    currentEdge: Edge | null
    t: number
    dwelling: number
    hopsRemaining: number
  }

  const items: Item[] = []
  let sinceSpawn = 1.0
  const spawnInterval = 3.2

  const ITEM_LIFT = 12
  const ITEM_W = 60
  const ITEM_H = 22

  // Ownership tint — warm terracotta for entities where the session user
  // authored the PR, cooler muted blue for entities authored by others.
  // We track every PR we can see, so the floor should show both.
  const TINT_MINE = COLOR_ACCENT // 0xc47a5a terracotta
  const TINT_OTHER = 0x7a9aad // muted slate-blue

  const createItem = (label: string, mine: boolean): Item => {
    const g = new Container()
    layer.addChild(g)
    const tint = mine ? TINT_MINE : TINT_OTHER

    const shadow = new Graphics()
    shadow.ellipse(0, 2, ITEM_W / 2 - 2, 4)
    shadow.fill({ color: 0x000000, alpha: 0.22 })
    g.addChild(shadow)

    const body = new Container()
    body.y = -ITEM_LIFT
    g.addChild(body)

    const bg = new Graphics()
    bg.roundRect(-ITEM_W / 2, -ITEM_H / 2, ITEM_W, ITEM_H, ITEM_H / 2)
    bg.fill({ color: 0xffffff, alpha: 0.97 })
    bg.stroke({ width: 1, color: tint, alpha: 0.55 })
    body.addChild(bg)

    const inner = new Graphics()
    inner.roundRect(-ITEM_W / 2 + 2, -ITEM_H / 2 + 2, ITEM_W - 4, ITEM_H - 4, (ITEM_H - 4) / 2)
    inner.fill({ color: tint, alpha: 0.1 })
    body.addChild(inner)

    const topHighlight = new Graphics()
    topHighlight.moveTo(-ITEM_W / 2 + 6, -ITEM_H / 2 + 1.5)
    topHighlight.lineTo(ITEM_W / 2 - 6, -ITEM_H / 2 + 1.5)
    topHighlight.stroke({ width: 1, color: 0xffffff, alpha: 0.9 })
    body.addChild(topHighlight)

    const text = new Text({
      text: label,
      style: {
        fontFamily: 'Inter, system-ui, sans-serif',
        fontSize: 11,
        fontWeight: '600',
        fill: tint,
        letterSpacing: 0.2,
      },
    })
    text.anchor.set(0.5, 0.5)
    body.addChild(text)

    // Pick a scripted journey and seed the item on the first belt heading
    // toward script[1] (item starts at script[0] and travels from there).
    const script = pickScript()
    const initial = script.length >= 2 ? edgeToward(script[0], script[1]) : null
    return {
      gfx: g,
      script,
      scriptIdx: 1,
      currentEdge: initial,
      t: 0,
      dwelling: 0,
      hopsRemaining: MAX_HOPS,
    }
  }

  const despawn = (i: number, item: Item) => {
    layer.removeChild(item.gfx)
    item.gfx.destroy({ children: true })
    items.splice(i, 1)
  }

  let nextId = 1042

  return {
    update(dt: number) {
      sinceSpawn += dt
      if (sinceSpawn >= spawnInterval) {
        sinceSpawn = 0
        // ~60% mine / 40% others — rough mix to exercise both tints. In
        // the real-event model this will come from entity metadata
        // (author_is_self) rather than a coin flip.
        const mine = Math.random() < 0.6
        items.push(createItem(`PR #${nextId++}`, mine))
      }

      for (let i = items.length - 1; i >= 0; i--) {
        const it = items[i]

        // Dwelling at a scripted station stop.
        if (it.dwelling > 0) {
          it.dwelling -= dt
          it.gfx.visible = false
          if (it.dwelling <= 0) {
            it.dwelling = 0
            // If we've consumed the whole script, dwell was the final-stop
            // farewell — despawn now.
            if (it.scriptIdx >= it.script.length) {
              despawn(i, it)
              continue
            }
            // Otherwise find the edge toward the next scripted stop.
            if (!it.currentEdge) {
              despawn(i, it)
              continue
            }
            const atNode = it.currentEdge.to
            const next = edgeToward(atNode, it.script[it.scriptIdx])
            if (!next || it.hopsRemaining <= 0) {
              despawn(i, it)
              continue
            }
            it.currentEdge = next
            it.t = 0
            it.hopsRemaining -= 1
          }
          continue
        }

        if (!it.currentEdge) {
          despawn(i, it)
          continue
        }

        it.gfx.visible = true
        it.t += dt / LEG_DURATION
        if (it.t >= 1) {
          const atNode = it.currentEdge.to
          // Are we at the next scripted stop?
          if (atNode === it.script[it.scriptIdx]) {
            it.scriptIdx += 1
            // Stations always dwell (including the terminal — brief farewell
            // dwell, then despawn). Non-stations as scripted stops would be
            // a script-authoring error, but handle gracefully by transiting.
            const arrived = nodes[atNode]
            if (arrived.kind === 'station') {
              it.dwelling = DWELL
              it.gfx.visible = false
            } else if (it.scriptIdx >= it.script.length) {
              despawn(i, it)
            } else {
              const next = edgeToward(atNode, it.script[it.scriptIdx])
              if (!next || it.hopsRemaining <= 0) {
                despawn(i, it)
                continue
              }
              it.currentEdge = next
              it.t = 0
              it.hopsRemaining -= 1
            }
            continue
          }
          // Transit node (splitter/merger) — no dwell, route onward.
          if (it.hopsRemaining <= 0) {
            despawn(i, it)
            continue
          }
          const next = edgeToward(atNode, it.script[it.scriptIdx])
          if (!next) {
            despawn(i, it)
            continue
          }
          it.currentEdge = next
          it.t = 0
          it.hopsRemaining -= 1
          continue
        }

        const pos = it.currentEdge.belt.pointAt(it.t)
        it.gfx.x = pos.x
        it.gfx.y = pos.y
        if (it.currentEdge.isTunnel) {
          // Tunnel fade: visible at the edges, invisible in the middle —
          // item "dives under" at entrance and "pops up" at exit.
          let alpha = 0
          if (it.t < 0.2) alpha = 1 - it.t / 0.2
          else if (it.t > 0.8) alpha = (it.t - 0.8) / 0.2
          it.gfx.alpha = Math.max(0, alpha)
        } else {
          const fade = Math.min(1, it.t / 0.08, (1 - it.t) / 0.08)
          it.gfx.alpha = Math.max(0, fade)
        }
      }
    },
  }
}
