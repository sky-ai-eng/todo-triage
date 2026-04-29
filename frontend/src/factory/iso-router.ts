// Router — a 1-cell box that merges or splits multiple belts. The
// "router" name covers both:
//
//   1 input + ≥1 outputs  → splitter (one belt becomes many).
//   ≥1 inputs + 1 output  → merger   (many belts become one).
//
// Visually the router is a small cousin of the station: same warm
// ceramic body, same recessed port openings with LED frames, same
// belt-stub-into-recess treatment. The actual merge/split happens
// inside the solid box and is hidden from view — items appear to
// flow into the box on input belts and emerge on output belts. The
// chevron flow on the connecting belts is what tells the user which
// direction each port is going.
//
// Topping the box: a small body-colored cylinder dome with a thin
// emissive lip ring at its base, signaling "this thing makes routing
// decisions" without committing to a literal merge/split visual.
//
// Per-port chevron continuity: each stub takes its own pathOffset
// so the connecting belt at that face can match it. The internal
// stub end UVs land inside the recess and are never visible at the
// junction with another router stub — only the wall-plane UV needs
// to agree with the connecting belt.
//
// 3-port configurations (T-junctions):
//   - input on one face, outputs on the other three  (1-to-3 splitter)
//   - inputs on three faces, output on the fourth    (3-to-1 merger)
//   - input + 2 perpendicular outputs / 2 inputs + perpendicular output
// 4-port configurations (cross / pass-through-with-tap) are also
// allowed by the same primitive.

import {
  CSG,
  type Mesh,
  MeshBuilder,
  type PBRMaterial,
  type Scene,
  TransformNode,
  Vector3,
} from '@babylonjs/core'

import { type BeltBuild, buildBelt } from './iso-belt'
import { CONVEYOR_HEIGHT, CONVEYOR_WIDTH, type PortDirection, type PortHandle } from './iso-port'
import type { PolePort } from './iso-pole'

// ─── Layout constants ─────────────────────────────────────────────────────

const ROUTER_BODY_HEIGHT = 36

// Port recess — tighter than the station's because the router is only
// 1 cell across; the station's 4-unit margin would dominate the face.
const ROUTER_RECESS_DEPTH = 14
const ROUTER_RECESS_MARGIN_ACROSS = 2
const ROUTER_RECESS_MARGIN_ABOVE = 12

// LED frame — thinner bars than the station's so they don't dominate
// the small face.
const FRAME_THICKNESS = 1.4
const FRAME_PROTRUSION = 0.6

// Stub-belt back gap matches station's STUB_BACK_GAP — the back of the
// stub stops just shy of the recess back wall.
const STUB_BACK_GAP = 2

// Recess interior wall slabs — same insets as station so the depth-test
// behavior is identical.
const RECESS_WALL_THICKNESS = 0.2
const RECESS_WALL_INSET = 0.1

// Top dome (control unit appearance).
const DOME_DIAM = 30
const DOME_HEIGHT = 4
// Emissive lip ring sitting under the dome; only the annular fringe
// outside DOME_DIAM is visible, reading as a glowing ring around the
// dome's base.
const DOME_LIP_DIAM = 38
const DOME_LIP_HEIGHT = 1.2

// ─── Public API ───────────────────────────────────────────────────────────

export interface Router {
  /** Grid cell occupied by the router. */
  col: number
  row: number
  /** Ports on the router's perimeter. 3 or 4 entries, each on a unique
   *  face direction, with at least one input and one output. */
  ports: PolePort[]
}

export interface RouterBuild {
  root: TransformNode
  meshes: Mesh[]
  /** Snap point for each port direction. Use the connecting belt's
   *  spec endpoint here (with z dropped to floor level). */
  ports: Map<PortDirection, PortHandle>
}

/** Materials a router needs. Subset of the station's material bundle —
 *  routers reuse the same ceramic body, LED trim, recess interior, and
 *  shared belt surface so they read as part of the station family. */
export interface RouterMaterials {
  body: PBRMaterial
  ledTrim: PBRMaterial
  recessInterior: PBRMaterial
  beltSurface: PBRMaterial
}

/** Build a router as a hierarchy of meshes parented to a single
 *  TransformNode. The pathOffsets map sets each stub's chevron UV
 *  bake; a connecting belt at that face must use a pathOffset whose
 *  start-UV agrees mod 1 with the stub's wall-plane UV. */
export function buildRouterMesh(
  scene: Scene,
  spec: Router,
  cellSize: number,
  materials: RouterMaterials,
  pathOffsets: Partial<Record<PortDirection, number>> = {},
): RouterBuild {
  if (spec.ports.length < 3 || spec.ports.length > 4) {
    throw new Error(
      `Router at (${spec.col}, ${spec.row}) needs 3 or 4 ports; got ${spec.ports.length}. ` +
        'Use a pole for 1-2 port routing.',
    )
  }
  const inputs = spec.ports.filter((p) => p.kind === 'input')
  const outputs = spec.ports.filter((p) => p.kind === 'output')
  if (inputs.length === 0 || outputs.length === 0) {
    throw new Error(
      `Router at (${spec.col}, ${spec.row}) needs at least one input and one output port.`,
    )
  }
  const dirSet = new Set(spec.ports.map((p) => p.direction))
  if (dirSet.size !== spec.ports.length) {
    throw new Error(`Router at (${spec.col}, ${spec.row}) has duplicate port directions.`)
  }

  const root = new TransformNode(`router_${spec.col}_${spec.row}`, scene)
  const meshes: Mesh[] = []
  const ports = new Map<PortDirection, PortHandle>()

  const cellOriginX = spec.col * cellSize
  const cellOriginY = spec.row * cellSize
  const cellCenterX = cellOriginX + cellSize / 2
  const cellCenterY = cellOriginY + cellSize / 2

  // ─── Body with carved port recesses ───
  // Single CSG chain — one box subtraction per port — so the body is
  // computed once with all recesses applied.
  const bodyTmp = MeshBuilder.CreateBox(
    'router-body-tmp',
    { width: cellSize, height: cellSize, depth: ROUTER_BODY_HEIGHT },
    scene,
  )
  bodyTmp.position.set(cellCenterX, cellCenterY, ROUTER_BODY_HEIGHT / 2)

  const portBuilds = spec.ports.map((p, i) =>
    buildRouterPort(
      scene,
      cellOriginX,
      cellOriginY,
      cellSize,
      p,
      i,
      pathOffsets[p.direction] ?? 0,
      materials,
    ),
  )

  let bodyCSG = CSG.FromMesh(bodyTmp)
  for (const pb of portBuilds) {
    bodyCSG = bodyCSG.subtract(CSG.FromMesh(pb.cutout))
  }
  const body = bodyCSG.toMesh('router-body', materials.body, scene, true)
  body.parent = root
  meshes.push(body)

  bodyTmp.dispose()
  for (const pb of portBuilds) {
    pb.cutout.dispose()
    for (const f of pb.frameMeshes) {
      f.parent = root
      meshes.push(f)
    }
    for (const w of pb.recessWalls) {
      w.parent = root
      meshes.push(w)
    }
    pb.belt.root.parent = root
    meshes.push(...pb.belt.meshes)
    ports.set(pb.handle.port.direction, pb.handle)
  }

  // ─── Top dome with emissive lip ring ───
  // LED lip first (bottom layer) — its top face is at the dome's
  // bottom face, so only the annulus outside the dome's diameter
  // is visible from above, reading as a glowing ring framing the
  // dome's base.
  const lipZ = ROUTER_BODY_HEIGHT + DOME_LIP_HEIGHT / 2
  const lip = MeshBuilder.CreateCylinder(
    'router-dome-lip',
    { diameter: DOME_LIP_DIAM, height: DOME_LIP_HEIGHT, tessellation: 32 },
    scene,
  )
  lip.rotation.x = Math.PI / 2
  lip.position.set(cellCenterX, cellCenterY, lipZ)
  lip.material = materials.ledTrim
  lip.parent = root
  meshes.push(lip)

  const domeBottomZ = ROUTER_BODY_HEIGHT + DOME_LIP_HEIGHT
  const dome = MeshBuilder.CreateCylinder(
    'router-dome',
    { diameter: DOME_DIAM, height: DOME_HEIGHT, tessellation: 28 },
    scene,
  )
  dome.rotation.x = Math.PI / 2
  dome.position.set(cellCenterX, cellCenterY, domeBottomZ + DOME_HEIGHT / 2)
  dome.material = materials.body
  dome.parent = root
  meshes.push(dome)

  return { root, meshes, ports }
}

// ─── Per-port mesh builder ────────────────────────────────────────────────
// Closely follows iso-station.ts buildPortMeshes, simplified for the
// router's 1-cell-square footprint and fixed (constant) recess depth.

interface RouterPortBuild {
  cutout: Mesh
  frameMeshes: Mesh[]
  recessWalls: Mesh[]
  belt: BeltBuild
  handle: PortHandle
}

function buildRouterPort(
  scene: Scene,
  cellOriginX: number,
  cellOriginY: number,
  cellSize: number,
  port: PolePort,
  index: number,
  pathOffset: number,
  materials: RouterMaterials,
): RouterPortBuild {
  const isXAxis = port.direction === 'east' || port.direction === 'west'
  const outwardSign = port.direction === 'east' || port.direction === 'north' ? 1 : -1
  const outwardX = isXAxis ? outwardSign : 0
  const outwardY = isXAxis ? 0 : outwardSign
  const acrossX = isXAxis ? 0 : 1
  const acrossY = isXAxis ? 1 : 0

  const cellCenterX = cellOriginX + cellSize / 2
  const cellCenterY = cellOriginY + cellSize / 2

  let faceX: number, faceY: number
  switch (port.direction) {
    case 'east':
      faceX = cellOriginX + cellSize
      faceY = cellCenterY
      break
    case 'west':
      faceX = cellOriginX
      faceY = cellCenterY
      break
    case 'north':
      faceX = cellCenterX
      faceY = cellOriginY + cellSize
      break
    case 'south':
      faceX = cellCenterX
      faceY = cellOriginY
      break
  }

  const recessOpenW = CONVEYOR_WIDTH + 2 * ROUTER_RECESS_MARGIN_ACROSS
  const recessOpenH = CONVEYOR_HEIGHT + ROUTER_RECESS_MARGIN_ABOVE
  const recessDepth = ROUTER_RECESS_DEPTH

  // ─── Cutout box ───
  // Outward overshoots wall by 1 unit; bottom overshoots floor by 1
  // unit. Both overshoots break coplanar faces cleanly so CSG doesn't
  // leave hairlines.
  const cutoutOvershoot = 1
  const cutoutAlong = recessDepth + cutoutOvershoot
  const cutoutAlongOffset = (cutoutOvershoot - recessDepth) / 2
  const cutoutVertical = recessOpenH + 1
  const cutoutVerticalCenter = (recessOpenH - 1) / 2
  const cutoutSize = isXAxis
    ? { width: cutoutAlong, height: recessOpenW, depth: cutoutVertical }
    : { width: recessOpenW, height: cutoutAlong, depth: cutoutVertical }
  const cutout = MeshBuilder.CreateBox(`router-port-cut-${index}`, cutoutSize, scene)
  cutout.position.set(
    faceX + outwardX * cutoutAlongOffset,
    faceY + outwardY * cutoutAlongOffset,
    cutoutVerticalCenter,
  )

  // ─── LED frame (top + 2 sides) ───
  const recessTopZ = recessOpenH
  const frameOutwardCenter = FRAME_PROTRUSION / 2
  const frameMeshes: Mesh[] = []

  const topAcrossLen = recessOpenW + 2 * FRAME_THICKNESS
  const topZ = recessTopZ + FRAME_THICKNESS / 2
  const topSize = isXAxis
    ? { width: FRAME_PROTRUSION, height: topAcrossLen, depth: FRAME_THICKNESS }
    : { width: topAcrossLen, height: FRAME_PROTRUSION, depth: FRAME_THICKNESS }
  const topBar = MeshBuilder.CreateBox(`router-port-frame-${index}-top`, topSize, scene)
  topBar.position.set(
    faceX + outwardX * frameOutwardCenter,
    faceY + outwardY * frameOutwardCenter,
    topZ,
  )
  topBar.material = materials.ledTrim
  frameMeshes.push(topBar)

  const sideZ = recessOpenH / 2
  const sideAcrossOffset = recessOpenW / 2 + FRAME_THICKNESS / 2
  const sideSize = isXAxis
    ? { width: FRAME_PROTRUSION, height: FRAME_THICKNESS, depth: recessOpenH }
    : { width: FRAME_THICKNESS, height: FRAME_PROTRUSION, depth: recessOpenH }
  for (const sign of [-1, 1] as const) {
    const bar = MeshBuilder.CreateBox(
      `router-port-frame-${index}-${sign < 0 ? 'l' : 'r'}`,
      sideSize,
      scene,
    )
    bar.position.set(
      faceX + outwardX * frameOutwardCenter + acrossX * sign * sideAcrossOffset,
      faceY + outwardY * frameOutwardCenter + acrossY * sign * sideAcrossOffset,
      sideZ,
    )
    bar.material = materials.ledTrim
    frameMeshes.push(bar)
  }

  // ─── Recess interior walls ───
  // Thin slabs covering the body's CSG-cut surfaces inside the recess.
  // Inset slightly so the slabs sit closer to camera than the body
  // wall and win the depth test.
  const inX = -outwardX
  const inY = -outwardY
  const wallT = RECESS_WALL_THICKNESS
  const wallInset = RECESS_WALL_INSET
  const recessWalls: Mesh[] = []

  const placeWall = (
    name: string,
    alongCenter: number,
    acrossOffset: number,
    zCenter: number,
    alongExtent: number,
    acrossExtent: number,
    zExtent: number,
  ): Mesh => {
    const w = isXAxis ? alongExtent : acrossExtent
    const h = isXAxis ? acrossExtent : alongExtent
    const slab = MeshBuilder.CreateBox(name, { width: w, height: h, depth: zExtent }, scene)
    slab.position.set(
      faceX + inX * alongCenter + acrossX * acrossOffset,
      faceY + inY * alongCenter + acrossY * acrossOffset,
      zCenter,
    )
    slab.material = materials.recessInterior
    return slab
  }

  recessWalls.push(
    placeWall(
      `router-port-recess-back-${index}`,
      recessDepth - wallInset - wallT / 2,
      0,
      recessOpenH / 2,
      wallT,
      recessOpenW,
      recessOpenH,
    ),
  )
  recessWalls.push(
    placeWall(
      `router-port-recess-top-${index}`,
      recessDepth / 2,
      0,
      recessOpenH - wallInset - wallT / 2,
      recessDepth - 2 * wallInset,
      recessOpenW,
      wallT,
    ),
  )
  for (const sign of [-1, 1] as const) {
    recessWalls.push(
      placeWall(
        `router-port-recess-side-${index}-${sign < 0 ? 'l' : 'r'}`,
        recessDepth / 2,
        sign * (recessOpenW / 2 - wallInset - wallT / 2),
        recessOpenH / 2,
        recessDepth - 2 * wallInset,
        wallT,
        recessOpenH,
      ),
    )
  }

  // ─── Stub belt ───
  // Same primitive as the station's port stub: short belt segment
  // from the wall plane to just shy of the recess back, with both
  // caps suppressed so the connecting belt butts up flat-to-flat
  // and chevrons flow continuously across the wall plane.
  const stubLength = recessDepth - STUB_BACK_GAP
  const wallEnd = new Vector3(faceX, faceY, 0)
  const recessEnd = new Vector3(faceX + inX * stubLength, faceY + inY * stubLength, 0)
  const isOutput = port.kind === 'output'
  const belt = buildBelt(
    scene,
    {
      start: isOutput ? recessEnd : wallEnd,
      end: isOutput ? wallEnd : recessEnd,
      pathOffset,
      capStart: false,
      capEnd: false,
    },
    materials.beltSurface,
  )

  const handle: PortHandle = {
    port: { kind: port.kind, direction: port.direction, offset: 0.5, recessDepth },
    worldPos: new Vector3(faceX, faceY, CONVEYOR_HEIGHT / 2),
    outward: new Vector3(outwardX, outwardY, 0),
    // Stub belt's segment doubles as the port's internal path. Input
    // stubs end inside the box (item despawns at recess back unless
    // routing logic claims it); output stubs are entered by routing
    // logic from inside and exit at the wall plane onto a belt.
    segment: belt.segment,
  }

  return { cutout, frameMeshes, recessWalls, belt, handle }
}
