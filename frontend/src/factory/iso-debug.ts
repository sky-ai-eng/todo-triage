// Debug scene for the 3D rewrite (SKY-196 / SKY-197).
//
// This file is now thin: it mounts a canvas, hands it to IsoScene, and
// wires up the React-side HUD via Babylon's camera observable. Babylon's
// ArcRotateCamera handles all gestures (LMB-drag = orbit, RMB-drag
// or ctrl+LMB = pan, wheel = zoom) — no custom pointer handlers.

import { IsoScene } from './iso-renderer'
import { DEFAULT_PORT_RECESS_DEPTH } from './iso-port'
import type { Pole } from './iso-pole'
import type { Router } from './iso-router'
import type { Station } from './iso-station'

const FLOOR_CELL = 80
const FLOOR_SIZE = 2400 // 30×30 cells of working space
const INITIAL_ZOOM_RADIUS = FLOOR_SIZE / 2

// Station occupies cells (10..14, 14..16) — 5 cols × 3 rows = 400×240
// world units. Sits roughly at the floor center; centered ports on
// west/east faces fall on the middle row's edge midpoints.
const STATION_CELL_COL = 10
const STATION_CELL_ROW = 14
const STATION_CELL_W = 5
const STATION_CELL_D = 3

const TEST_STATION: Station = {
  x: STATION_CELL_COL * FLOOR_CELL,
  y: STATION_CELL_ROW * FLOOR_CELL,
  z: 0,
  w: STATION_CELL_W * FLOOR_CELL,
  d: STATION_CELL_D * FLOOR_CELL,
  h: 64,
  queuedCount: 4,
  wipCount: 2,
  ports: [
    { kind: 'input', direction: 'west', offset: 0.5, recessDepth: DEFAULT_PORT_RECESS_DEPTH },
    { kind: 'output', direction: 'east', offset: 0.5, recessDepth: DEFAULT_PORT_RECESS_DEPTH },
  ],
}

// Test layout:
//
// West chain (turn): source pole south of the station pushes items
// north along a long belt to a 90° turn pole, which smoothly arcs
// the belt east into the station's west port.
//
//   source@(5,5) ──belt 720↑── turn@(5,15) ──belt 320→── station_W
//
// East chain (splitter): station east port → belt → 3-port splitter,
// which fans the items north and south to two sink dead-end poles.
//
//             sink_N@(18,19)
//                  ↑ belt 240
//   station_E ──belt 240→── splitter@(18,15) (W in, N+S out)
//                  ↓ belt 240
//             sink_S@(18,11)
//
// PathOffsets are anchored at the station wall planes (stub.pathOffset
// = 0) and chained outward via mod-CHEVRON_SPACING_WORLD arithmetic.
// The turn pole's internal arc length (quarter-circle of radius
// cellSize/2) shows up in the chain math, so the source-to-station
// chain has awkward fractional offsets.
const STATION_MID_ROW = STATION_CELL_ROW + 1 // row 15
const SPACING = 54
const CAP_PERIM = Math.PI * 4 // ≈ 12.566
const TURN_ARC_LENGTH = (Math.PI * FLOOR_CELL) / 4 // quarter-arc, radius cellSize/2

const mod = (x: number) => ((x % SPACING) + SPACING) % SPACING

// ─── West chain (turn) ─────────────────────────────────────────────
const SOURCE_POLE: Pole = {
  col: 5,
  row: 5,
  ports: [{ direction: 'north', kind: 'output' }],
}

const TURN_POLE: Pole = {
  col: 5,
  row: STATION_MID_ROW,
  ports: [
    { direction: 'south', kind: 'input' },
    { direction: 'east', kind: 'output' },
  ],
}

// Anchor at station: west stub UV at wall plane = 0. Chain back.
const WEST_BELT2_PATH_OFFSET = -320
// Turn's UV at east port (= turn.pathOffset + arcLength) must match
// belt 2W's UV at start (4/54).
const TURN_POLE_PATH_OFFSET = mod(4 - TURN_ARC_LENGTH)
// Belt 1W's UV at end (turn south) must match turn.pathOffset/54.
const WEST_BELT1_PATH_OFFSET = mod(TURN_POLE_PATH_OFFSET - 720)
// Source pole's UV at north port (cap + top = 52.566 of path) must
// match belt 1W's UV at start.
const SOURCE_POLE_PATH_OFFSET = mod(WEST_BELT1_PATH_OFFSET - CAP_PERIM - 40)

// ─── East chain (splitter feeding two dead-ends) ──────────────────
//
// The 3-port splitter sits 3 cells east of the station's east face.
// Its west port consumes the station's output; its north and south
// ports each feed a sink pole 4 cells away.
const SPLITTER_COL = STATION_CELL_COL + STATION_CELL_W + 3 // col 18
const SPLITTER: Router = {
  col: SPLITTER_COL,
  row: STATION_MID_ROW,
  ports: [
    { direction: 'west', kind: 'input' },
    { direction: 'north', kind: 'output' },
    { direction: 'south', kind: 'output' },
  ],
}

const SINK_N_POLE: Pole = {
  col: SPLITTER_COL,
  row: STATION_MID_ROW + 4, // row 19
  ports: [{ direction: 'south', kind: 'input' }],
}

const SINK_S_POLE: Pole = {
  col: SPLITTER_COL,
  row: STATION_MID_ROW - 4, // row 11
  ports: [{ direction: 'north', kind: 'input' }],
}

// Belt 1 (station_E → splitter_W): station east stub end UV at wall
// plane = 58/54 mod 1 = 4/54. Belt 1 pathOffset = 4 makes belt start
// UV match. Belt length 240; end UV at splitter wall = (4+240)/54
// mod 54 = 28/54 → splitter west stub pathOffset = 28.
const EAST_BELT1_PATH_OFFSET = 4
const SPLITTER_WEST_PATH_OFFSET = 28

// Splitter's two output stubs (N, S) anchor at pathOffset = 0 each.
// Their wall-plane UVs are 12/54 (output stub: start at recess back,
// end at wall after stubLength=12).
const SPLITTER_NORTH_PATH_OFFSET = 0
const SPLITTER_SOUTH_PATH_OFFSET = 0

// Belts 2 + 3 (splitter_N → sink_N, splitter_S → sink_S): start UV
// must equal 12/54 → pathOffset = 12. Length 240; end UV =
// (12+240)/54 mod 54 = 36/54 → sink pole pathOffset = 36.
const EAST_BELT2_PATH_OFFSET = 12
const EAST_BELT3_PATH_OFFSET = 12
const SINK_N_PATH_OFFSET = 36
const SINK_S_PATH_OFFSET = 36

export interface CameraStateForHUD {
  /** Polar angle from +z axis, in radians. 0 = top-down. */
  pitch: number
  /** Azimuth around +z axis, in radians. */
  yaw: number
  /** Zoom factor relative to the initial view. >1 = zoomed in. */
  zoom: number
}

export interface IsoDebugSceneHandle {
  destroy: () => void
  resetView: () => void
  /** Subscribe to camera state changes. The HUD uses this to render
   * pitch/yaw/zoom live. Returns an unsubscribe function. */
  onCameraChange: (cb: (s: CameraStateForHUD) => void) => () => void
}

export async function createIsoDebugScene(container: HTMLDivElement): Promise<IsoDebugSceneHandle> {
  // Create our own canvas inside the container — Babylon attaches its
  // engine to whatever <canvas> we hand it, and we need control over
  // sizing and pointer behavior.
  const canvas = document.createElement('canvas')
  canvas.style.width = '100%'
  canvas.style.height = '100%'
  canvas.style.display = 'block'
  canvas.style.touchAction = 'none' // suppress browser pan/zoom on touch
  container.appendChild(canvas)

  const initialRect = container.getBoundingClientRect()
  const dpr = window.devicePixelRatio || 1
  canvas.width = initialRect.width * dpr
  canvas.height = initialRect.height * dpr

  const renderer = new IsoScene(canvas)
  renderer.buildFloor(FLOOR_SIZE, FLOOR_CELL)
  const station = renderer.addStation(TEST_STATION)

  // West chain: source → north belt → turn pole (curved) → east belt → station.
  const sourcePole = renderer.addPole(SOURCE_POLE, FLOOR_CELL, SOURCE_POLE_PATH_OFFSET)
  const turnPole = renderer.addPole(TURN_POLE, FLOOR_CELL, TURN_POLE_PATH_OFFSET)
  const westBelt1 = renderer.addBelt(
    sourcePole.ports.get('north')!,
    turnPole.ports.get('south')!,
    WEST_BELT1_PATH_OFFSET,
    false,
    false,
  )
  const westBelt2 = renderer.addBelt(
    turnPole.ports.get('east')!,
    station.ports[0], // station west input
    WEST_BELT2_PATH_OFFSET,
    false,
    false,
  )

  // ─── Item path graph: chain west-chain segments ─────────────────
  // Items spawn at the source pole's center, ride out to its north
  // edge (sourcePole.internalSegment), continue along westBelt1 to
  // the turn pole, traverse the quarter-arc inside the turn pole,
  // continue along westBelt2 to the station's west input port stub,
  // and despawn at the back of the recess (station port stub has no
  // `next` until station processing is wired up).
  sourcePole.internalSegment.next = [westBelt1.segment]
  westBelt1.segment.next = [turnPole.internalSegment]
  turnPole.internalSegment.next = [westBelt2.segment]
  westBelt2.segment.next = [station.ports[0].segment!]

  // East chain: station → belt → splitter → 2 sink dead-end poles.
  const splitter = renderer.addRouter(SPLITTER, FLOOR_CELL, {
    west: SPLITTER_WEST_PATH_OFFSET,
    north: SPLITTER_NORTH_PATH_OFFSET,
    south: SPLITTER_SOUTH_PATH_OFFSET,
  })
  const sinkNPole = renderer.addPole(SINK_N_POLE, FLOOR_CELL, SINK_N_PATH_OFFSET)
  const sinkSPole = renderer.addPole(SINK_S_POLE, FLOOR_CELL, SINK_S_PATH_OFFSET)
  renderer.addBelt(
    station.ports[1], // station east output
    splitter.ports.get('west')!,
    EAST_BELT1_PATH_OFFSET,
    false,
    false,
  )
  renderer.addBelt(
    splitter.ports.get('north')!,
    sinkNPole.ports.get('south')!,
    EAST_BELT2_PATH_OFFSET,
    false,
    false,
  )
  renderer.addBelt(
    splitter.ports.get('south')!,
    sinkSPole.ports.get('north')!,
    EAST_BELT3_PATH_OFFSET,
    false,
    false,
  )

  // Dummy spawner — emit one item every 1.5s at the source pole's
  // center. They flow source → turn → station west input and despawn
  // at the back of the recess. East chain stays empty until station
  // processing is wired in.
  renderer.startItemSpawner(sourcePole.internalSegment, 1.5)

  const ro = new ResizeObserver(() => {
    renderer.resize()
  })
  ro.observe(container)

  return {
    destroy: () => {
      ro.disconnect()
      renderer.destroy()
      canvas.remove()
    },
    resetView: () => renderer.resetView(),
    onCameraChange: (cb) => {
      // Throttle to one notification per animation frame — Babylon's
      // observable can fire on every input pixel, the HUD doesn't
      // need that resolution.
      let raf: number | null = null
      const snapshot = (): CameraStateForHUD => ({
        pitch: renderer.camera.beta,
        yaw: renderer.camera.alpha,
        // ArcRotateCamera radius is the half-height of the visible
        // ortho frustum. Smaller radius = zoomed in. Express as a
        // multiplier on the initial view for an intuitive HUD value.
        zoom: INITIAL_ZOOM_RADIUS / renderer.camera.radius,
      })
      const wrapped = () => {
        if (raf != null) return
        raf = requestAnimationFrame(() => {
          raf = null
          cb(snapshot())
        })
      }
      const observer = renderer.camera.onViewMatrixChangedObservable.add(wrapped)
      // Fire once immediately so the HUD doesn't start blank.
      cb(snapshot())
      return () => {
        if (raf != null) cancelAnimationFrame(raf)
        renderer.camera.onViewMatrixChangedObservable.remove(observer)
      }
    },
  }
}
