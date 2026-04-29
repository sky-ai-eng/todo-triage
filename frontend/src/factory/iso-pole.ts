// Pole — a 1-cell routing tile, built from belt segments. The pole's
// "physical" appearance is whatever belt geometry the port config
// produces:
//
//   1 port               → dead-end leg from cell edge to center,
//                          rounded cap at center (the terminus). Items
//                          emerge from / disappear into the cap.
//   2 opposite ports     → single straight belt running edge-to-edge
//                          through the cell (visually continuous with
//                          neighboring belts; pole has no marker of
//                          its own).
//   2 adjacent ports     → two perpendicular legs meeting at the
//                          cell center, forming a 90° turn.
//
// Connecting belts butt up flat-to-flat against the pole's leg(s) at
// the cell edge. Cap suppression at the junction is handled by the
// caller passing capStart/capEnd: false on the connecting belt.
//
// Chevron continuity at the cell-edge junction is preserved via
// pathOffset arithmetic (mod CHEVRON_SPACING_WORLD); when continuity
// at multiple junctions can't be simultaneously satisfied (e.g.,
// when the chain length isn't an exact multiple of the chevron
// spacing), prioritize the longest stretch and accept a small phase
// shift at the shorter junction.

import { type Mesh, type PBRMaterial, type Scene, TransformNode, Vector3 } from '@babylonjs/core'

import { buildBelt, buildCurvedBelt } from './iso-belt'
import { CONVEYOR_HEIGHT } from './iso-port'
import type { PortDirection, PortHandle } from './iso-port'

export interface PolePort {
  direction: PortDirection
  /** Flow direction at this port:
   *    'output' → items flow OUT through this port (source side).
   *    'input'  → items flow IN through this port (sink side).
   *  This sets the pole's internal belt orientation so chevrons
   *  match the connecting belt's flow direction. */
  kind: 'input' | 'output'
}

export interface Pole {
  /** Grid cell occupied by the pole. */
  col: number
  row: number
  /** Ports on the pole's perimeter.
   *    1 entry: dead-end. kind='output' = source, kind='input' = sink.
   *    2 opposite entries (one input + one output): straight
   *      pass-through, flow input → output.
   *    2 adjacent entries (one input + one output): 90° turn,
   *      flow input → corner → output. */
  ports: PolePort[]
}

export interface PoleBuild {
  root: TransformNode
  meshes: Mesh[]
  /** Snap point for each port direction. The connecting belt's
   *  spec endpoint goes here (with z dropped to floor level). */
  ports: Map<PortDirection, PortHandle>
}

const OUTWARD: Record<PortDirection, Vector3> = {
  east: new Vector3(1, 0, 0),
  west: new Vector3(-1, 0, 0),
  north: new Vector3(0, 1, 0),
  south: new Vector3(0, -1, 0),
}

function isOpposite(a: PortDirection, b: PortDirection): boolean {
  return (
    (a === 'east' && b === 'west') ||
    (a === 'west' && b === 'east') ||
    (a === 'north' && b === 'south') ||
    (a === 'south' && b === 'north')
  )
}

export function buildPoleMesh(
  scene: Scene,
  spec: Pole,
  cellSize: number,
  beltMaterial: PBRMaterial,
  pathOffset: number = 0,
): PoleBuild {
  const root = new TransformNode(`pole_${spec.col}_${spec.row}`, scene)
  const meshes: Mesh[] = []
  const ports: Map<PortDirection, PortHandle> = new Map()

  const cellOriginX = spec.col * cellSize
  const cellOriginY = spec.row * cellSize
  const cellCenterX = cellOriginX + cellSize / 2
  const cellCenterY = cellOriginY + cellSize / 2
  const center = new Vector3(cellCenterX, cellCenterY, 0)

  const edgePoint = (dir: PortDirection): Vector3 => {
    switch (dir) {
      case 'east':
        return new Vector3(cellOriginX + cellSize, cellCenterY, 0)
      case 'west':
        return new Vector3(cellOriginX, cellCenterY, 0)
      case 'north':
        return new Vector3(cellCenterX, cellOriginY + cellSize, 0)
      case 'south':
        return new Vector3(cellCenterX, cellOriginY, 0)
    }
  }

  const handle = (dir: PortDirection): PortHandle => ({
    port: { kind: 'output', direction: dir, offset: 0.5, recessDepth: 0 },
    worldPos: new Vector3(edgePoint(dir).x, edgePoint(dir).y, CONVEYOR_HEIGHT / 2),
    outward: OUTWARD[dir].clone(),
  })

  const attachBelt = (
    start: Vector3,
    end: Vector3,
    pathOffset: number,
    capStart: boolean,
    capEnd: boolean,
  ) => {
    const belt = buildBelt(scene, { start, end, pathOffset, capStart, capEnd }, beltMaterial)
    belt.root.parent = root
    meshes.push(...belt.meshes)
    return belt
  }

  if (spec.ports.length === 1) {
    // Dead-end. Belt orientation depends on kind so the chevron flow
    // matches the connecting belt at the cell edge:
    //   output (source): cap at center, belt flows center → edge.
    //   input  (sink):   cap at center, belt flows edge → center.
    const p = spec.ports[0]
    if (p.kind === 'output') {
      attachBelt(center, edgePoint(p.direction), pathOffset, true, false)
    } else {
      attachBelt(edgePoint(p.direction), center, pathOffset, false, true)
    }
    ports.set(p.direction, handle(p.direction))
  } else if (spec.ports.length === 2) {
    const input = spec.ports.find((p) => p.kind === 'input')
    const output = spec.ports.find((p) => p.kind === 'output')
    if (!input || !output) {
      throw new Error(
        `Pole at (${spec.col}, ${spec.row}) with 2 ports needs one input + one output.`,
      )
    }

    if (isOpposite(input.direction, output.direction)) {
      // Straight pass-through: one belt running input edge → output
      // edge, no caps (both ends are belt junctions).
      attachBelt(edgePoint(input.direction), edgePoint(output.direction), pathOffset, false, false)
    } else {
      // 90° turn: a single curved belt that smoothly arcs from the
      // input edge to the output edge. The arc is a quarter-circle
      // centered on the cell corner where the two edges meet (the
      // "inside" corner of the turn). Belt centerline radius =
      // cellSize/2; chevrons follow the arc and visually rotate from
      // the input direction to the output direction across the curve.
      const cornerX =
        input.direction === 'east' || output.direction === 'east'
          ? cellOriginX + cellSize
          : cellOriginX
      const cornerY =
        input.direction === 'north' || output.direction === 'north'
          ? cellOriginY + cellSize
          : cellOriginY
      const arcRadius = cellSize / 2

      const inStart = edgePoint(input.direction)
      const outEnd = edgePoint(output.direction)
      const startAngle = Math.atan2(inStart.y - cornerY, inStart.x - cornerX)
      const endAngle = Math.atan2(outEnd.y - cornerY, outEnd.x - cornerX)
      // Shortest signed sweep — ±π/2 for any 90° turn.
      let delta = endAngle - startAngle
      while (delta > Math.PI) delta -= 2 * Math.PI
      while (delta < -Math.PI) delta += 2 * Math.PI

      const TURN_TESSELLATION = 16
      const arcPath: Vector3[] = []
      for (let i = 0; i <= TURN_TESSELLATION; i++) {
        const t = i / TURN_TESSELLATION
        const angle = startAngle + t * delta
        arcPath.push(
          new Vector3(
            cornerX + arcRadius * Math.cos(angle),
            cornerY + arcRadius * Math.sin(angle),
            0,
          ),
        )
      }

      const belt = buildCurvedBelt(scene, arcPath, pathOffset, beltMaterial)
      belt.root.parent = root
      meshes.push(...belt.meshes)
    }
    ports.set(input.direction, handle(input.direction))
    ports.set(output.direction, handle(output.direction))
  } else {
    throw new Error(
      `Pole at (${spec.col}, ${spec.row}) needs 1 or 2 ports; got ${spec.ports.length}. ` +
        'Use a splitter/merger primitive for 3+ port routing.',
    )
  }

  return { root, meshes, ports }
}
