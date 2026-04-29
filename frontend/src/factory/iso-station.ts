// Stage-1 station, built as a hierarchy of Babylon meshes (SKY-197).
//
// Aesthetic: liquid glass + Halo Reach + Transcendence — a warm
// ceramic chassis with a recessed chamber, a thin emissive trim
// ringing the chamber lip, a clear glass canopy hovering over it, and
// chips that read as glass tokens with glowing cores rather than
// painted boxes. PBR throughout so direct lights actually sculpt the
// form (no HDR env yet — that's the next polish pass).
//
// Form factor:
//
//   ┌──────────────────────────────────────────┐
//   │  ┌───┐    ┌──────────────┐               │
//   │  │pad│    │   chamber    │  ← glass canopy floats above
//   │  └───┘    └──────────────┘               │
//   └──────────────────────────────────────────┘
//      LEFT       CENTER (recessed)     RIGHT
//      queued      WIP items            (exit, future)
//
// The chamber is carved from the body via CSG.subtract, so it's a
// real recess on a single mesh. Pad, LED trim, canopy, and chips are
// independent meshes parented to a common TransformNode so the
// station moves as a unit.

import {
  Color3,
  CSG,
  DynamicTexture,
  Material,
  Mesh,
  MeshBuilder,
  PBRMaterial,
  Scene,
  TransformNode,
  Vector3,
} from '@babylonjs/core'

import { buildBelt, createBeltMaterial, type BeltBuild } from './iso-belt'
import {
  CONVEYOR_HEIGHT,
  CONVEYOR_WIDTH,
  PORT_RECESS_MARGIN_ABOVE,
  PORT_RECESS_MARGIN_ACROSS,
  type Port,
  type PortHandle,
} from './iso-port'

export interface Station {
  /** World position of the station's bottom-back-left corner. */
  x: number
  y: number
  z: number
  /** Footprint and height. */
  w: number
  d: number
  h: number
  /** Visible queued chips on the landing pad (capped at MAX_QUEUED). */
  queuedCount?: number
  /** Visible WIP chips standing in the chamber (capped at MAX_WIP). */
  wipCount?: number
  /** Conveyor attach points on the station's walls. Each produces a
   *  recess + LED frame + internal belt stub. */
  ports?: Port[]
}

// ─── Layout fractions ───────────────────────────────────────────────────────

const PAD_X_FRAC: readonly [number, number] = [0.06, 0.22]
const PAD_Y_FRAC: readonly [number, number] = [0.28, 0.72]
const PAD_LIFT_FRAC = 0.18

const CHAMBER_X_FRAC: readonly [number, number] = [0.32, 0.92]
const CHAMBER_Y_FRAC: readonly [number, number] = [0.18, 0.82]
const CHAMBER_DEPTH_FRAC = 0.55

// ─── LED ring (recessed in chamber wall) ───────────────────────────────────
// The LED sits in a CSG-cut groove on the chamber's interior walls, partway
// down from the lip. Reads as "machined recessed light strip" rather than
// a glow line painted on the rim.

const LED_RING_FROM_FLOOR = 14 // height above chamber floor where the ring sits
const LED_GROOVE_DEPTH = 1.6 // how deep the groove cuts into the wall
const LED_GROOVE_HEIGHT = 2.6 // groove vertical extent (taller than the LED for a small bezel)
const LED_HEIGHT = 1.6 // visible LED extent in z
const LED_END_INSET = 1.2 // gap at each corner so the strips don't fight at the joins

// ─── Glass canopy ─────────────────────────────────────────────────────────
// Sits on the body's top surface, slightly larger than the chamber
// opening so it reads as a glass lid covering the recess.

const CANOPY_OVERHANG = 6
const CANOPY_THICKNESS = 3
const CANOPY_LIFT = 0

// ─── Side details ──────────────────────────────────────────────────────────
// Vents on the back face, status panel on the front, heat sink fins
// on the output end, anchor discs at the corners. Together these
// give each face a distinct purpose so the station reads as a thing
// that does work, not an inert slab.

// Vent slot array (back / high-y face)
const VENT_COUNT = 5
const VENT_WIDTH_FRAC = 0.55
const VENT_HEIGHT = 2.4
const VENT_DEPTH = 6
const VENT_Z_FRAC_RANGE: readonly [number, number] = [0.3, 0.85]
const VENT_GLOW_INSET = 1.4 // x/z inset of the inner glow slab from the slot edge
const VENT_GLOW_THICKNESS = 0.5 // y extent of the glow slab inside the recess

// Status panel recess (front / low-y face)
const STATUS_PANEL_X_FRAC = 0.7
const STATUS_PANEL_W = 64
const STATUS_PANEL_H = 28
const STATUS_PANEL_DEPTH = 3
const STATUS_PANEL_Z_FRAC = 0.5
const STATUS_SCREEN_INSET = 1.6
const STATUS_SCREEN_THICKNESS = 0.5

// Heat sink fin array (output end / high-x face). Pushed toward the
// back-corner so the front-half of the right face is clear for the
// future output conveyor port. Fins mount on a back plate inside a
// shallow housing recess, so the right face reads as "chassis with a
// bolted-in heatsink module" rather than fins glued onto a flat wall.
const HEATSINK_FIN_COUNT = 7
const HEATSINK_FIN_EXTENT = 8
const HEATSINK_FIN_THICKNESS = 2.6
const HEATSINK_FIN_HEIGHT_FRAC = 0.6
const HEATSINK_FIN_SPACING = 7
const HEATSINK_FIN_Z_FRAC = 0.5
const HEATSINK_FIN_Y_FRAC = 0.88 // center of fin stack along body depth (back-corner)
const HEATSINK_HOUSING_PADDING_Y = 5
const HEATSINK_HOUSING_PADDING_Z = 5
const HEATSINK_HOUSING_DEPTH = 3
const HEATSINK_BACKPLATE_THICKNESS = 1.5

// Corner anchor discs (4 — one per body corner on the long faces)
const ANCHOR_DIAM = 14
const ANCHOR_THICKNESS = 1.4
const ANCHOR_X_INSET = 22
const ANCHOR_Z_FRAC = 0.18

// ─── Chip dimensions ──────────────────────────────────────────────────────

const QUEUED_CHIP_DIAM = 22
const QUEUED_CHIP_H = 6
const QUEUED_CHIP_GAP = 1.6
const QUEUED_CORE_DIAM = 14
const QUEUED_CORE_H = 2

const WIP_CHIP_DIAM = 16
const WIP_CHIP_H = 28
const WIP_CORE_DIAM = 6
const WIP_CORE_H = 20

const MAX_QUEUED = 6
const MAX_WIP = 3

// ─── Materials ────────────────────────────────────────────────────────────

export interface StationMaterials {
  body: PBRMaterial
  chamberFloor: PBRMaterial
  pad: PBRMaterial
  glass: PBRMaterial
  ledTrim: PBRMaterial
  ventGlow: PBRMaterial
  heatsinkFin: PBRMaterial
  screen: PBRMaterial
  beltSurface: PBRMaterial
  recessInterior: PBRMaterial
  queuedShell: PBRMaterial
  queuedCore: PBRMaterial
  wipShell: PBRMaterial
  wipCore: PBRMaterial
}

export function createStationMaterials(scene: Scene): StationMaterials {
  // Body — warm off-white ceramic. ClearCoat layer adds a subtle
  // lacquered sheen so direct lights still pick out the form even
  // without an HDR environment for real reflections.
  const body = new PBRMaterial('station-body', scene)
  body.albedoColor = Color3.FromHexString('#ece6d8')
  body.metallic = 0
  body.roughness = 0.45
  body.clearCoat.isEnabled = true
  body.clearCoat.intensity = 0.4
  body.clearCoat.roughness = 0.18

  // Chamber floor — warm dark machined finish. Slightly metallic so
  // the sun glints across it and reads as "precision recess" against
  // the ceramic body.
  const chamberFloor = new PBRMaterial('chamber-floor', scene)
  chamberFloor.albedoColor = Color3.FromHexString('#3d3a35')
  chamberFloor.metallic = 0.7
  chamberFloor.roughness = 0.55

  // Pad — brighter polished stage that the queued chips sit on.
  const pad = new PBRMaterial('pad', scene)
  pad.albedoColor = Color3.FromHexString('#f5f1e6')
  pad.metallic = 0.15
  pad.roughness = 0.2
  pad.clearCoat.isEnabled = true
  pad.clearCoat.intensity = 0.6
  pad.clearCoat.roughness = 0.08

  // Glass canopy — clear-blue protective panel hovering over the
  // chamber. Mostly transparent; picks up sharp specular highlights
  // from the sun, which is what sells "this is glass" without an HDR
  // env to actually reflect.
  const glass = new PBRMaterial('canopy-glass', scene)
  glass.albedoColor = Color3.FromHexString('#bfd9ff')
  glass.metallic = 0
  glass.roughness = 0.06
  glass.alpha = 0.18
  glass.transparencyMode = Material.MATERIAL_ALPHABLEND
  glass.indexOfRefraction = 1.5

  // LED trim — pure emissive cyan ring around the chamber lip. Black
  // albedo means lighting contributes nothing; emissive is the only
  // output. GlowLayer in the renderer blooms this without bleeding
  // into nearby PBR responses.
  const ledTrim = new PBRMaterial('led-trim', scene)
  ledTrim.albedoColor = Color3.Black()
  ledTrim.emissiveColor = Color3.FromHexString('#7cf7ec')
  ledTrim.emissiveIntensity = 1.6
  ledTrim.metallic = 0
  ledTrim.roughness = 1

  // Vent glow — warm orange ember sitting at the back of each vent
  // slot. Same emissive-only pattern as the LED trim but tuned to
  // the warm side of our palette so the back of the station has
  // matching "active heat" energy.
  const ventGlow = new PBRMaterial('vent-glow', scene)
  ventGlow.albedoColor = Color3.Black()
  ventGlow.emissiveColor = Color3.FromHexString('#ff8a3a')
  ventGlow.emissiveIntensity = 1.3
  ventGlow.metallic = 0
  ventGlow.roughness = 1

  // Heatsink fin — slightly lighter and smoother than the chamber-
  // floor material used for the back plate, so the fins read as
  // distinct from the plate they're mounted on when viewed head-on.
  const heatsinkFin = new PBRMaterial('heatsink-fin', scene)
  heatsinkFin.albedoColor = Color3.FromHexString('#605b52')
  heatsinkFin.metallic = 0.75
  heatsinkFin.roughness = 0.42

  // Status-panel screen — dim "off but powered" tablet face on the
  // front of the station. Subtle blue emissive at low intensity reads
  // as sleeping electronics rather than an active display, with a
  // glassy clearcoat so it picks up sharp specular highlights from
  // the sun.
  const screen = new PBRMaterial('status-screen', scene)
  screen.albedoColor = Color3.FromHexString('#15171c')
  screen.metallic = 0.1
  screen.roughness = 0.35
  screen.emissiveColor = Color3.FromHexString('#2a3550')
  screen.emissiveIntensity = 0.25
  screen.clearCoat.isEnabled = true
  screen.clearCoat.intensity = 0.7
  screen.clearCoat.roughness = 0.05

  // Belt surface — chevron-textured PBR material shared across all
  // belts in the scene. Animation observer is registered inside
  // createBeltMaterial; per-belt UVs are baked at construction time
  // in iso-belt.ts.
  const beltSurface = createBeltMaterial(scene)

  // Recess interior — vertical gradient texture. Painted light at the
  // top of the canvas, dark at the bottom. Mapping to each wall face
  // depends on Babylon's box face UV layout, but the result is a
  // subtle directional shading that breaks up the otherwise-flat
  // dark interior.
  const recessTex = new DynamicTexture('recess-gradient', { width: 64, height: 256 }, scene, false)
  const recessCtx = recessTex.getContext()
  const recessGrad = recessCtx.createLinearGradient(0, 0, 0, 256)
  recessGrad.addColorStop(0, '#322a22')
  recessGrad.addColorStop(1, '#0a0807')
  recessCtx.fillStyle = recessGrad
  recessCtx.fillRect(0, 0, 64, 256)
  recessTex.update()

  const recessInterior = new PBRMaterial('recess-interior', scene)
  recessInterior.albedoTexture = recessTex
  recessInterior.metallic = 0.25
  recessInterior.roughness = 0.7

  // Queued chip shell — amber glass token. More opaque than the
  // canopy so the inner core reads as "suggested" rather than fully
  // exposed.
  const queuedShell = new PBRMaterial('queued-shell', scene)
  queuedShell.albedoColor = Color3.FromHexString('#ffce7a')
  queuedShell.metallic = 0
  queuedShell.roughness = 0.15
  queuedShell.alpha = 0.55
  queuedShell.transparencyMode = Material.MATERIAL_ALPHABLEND
  queuedShell.indexOfRefraction = 1.45

  // Queued chip core — glowing amber heart inside each token.
  const queuedCore = new PBRMaterial('queued-core', scene)
  queuedCore.albedoColor = Color3.Black()
  queuedCore.emissiveColor = Color3.FromHexString('#ff9c3a')
  queuedCore.emissiveIntensity = 1.4
  queuedCore.metallic = 0
  queuedCore.roughness = 1

  // WIP chip shell — cool blue glass capsule.
  const wipShell = new PBRMaterial('wip-shell', scene)
  wipShell.albedoColor = Color3.FromHexString('#a8c4ff')
  wipShell.metallic = 0
  wipShell.roughness = 0.08
  wipShell.alpha = 0.45
  wipShell.transparencyMode = Material.MATERIAL_ALPHABLEND
  wipShell.indexOfRefraction = 1.5

  // WIP chip core — bright blue heart, hotter than queued because
  // WIP is the active state.
  const wipCore = new PBRMaterial('wip-core', scene)
  wipCore.albedoColor = Color3.Black()
  wipCore.emissiveColor = Color3.FromHexString('#7aa3ff')
  wipCore.emissiveIntensity = 2.0
  wipCore.metallic = 0
  wipCore.roughness = 1

  return {
    body,
    chamberFloor,
    pad,
    glass,
    ledTrim,
    ventGlow,
    heatsinkFin,
    screen,
    beltSurface,
    recessInterior,
    queuedShell,
    queuedCore,
    wipShell,
    wipCore,
  }
}

// ─── Port mesh builder ────────────────────────────────────────────────────

interface PortBuild {
  /** Temporary cutout box, subtracted from the body in the CSG chain
   *  then disposed. */
  cutout: Mesh
  /** LED frame bars (top + two sides) sitting on the wall surface
   *  around the recess opening. Caller parents to the station root. */
  frameMeshes: Mesh[]
  /** Recess interior walls — back, top, and two sides. Cover the
   *  body's CSG-cut surfaces with darker material. */
  recessWalls: Mesh[]
  /** The belt itself — top plane plus visible end cap at the wall.
   *  The cap on the back-of-recess side is suppressed (would clip
   *  through the recess back wall). */
  belt: BeltBuild
  /** Snap point + outward direction in world space. */
  handle: PortHandle
}

const FRAME_THICKNESS = 2
const FRAME_PROTRUSION = 0.6
const STUB_BACK_GAP = 2

// ─── Recess interior walls ──────────────────────────────────────────────
// Thin slabs covering the body's CSG-cut walls inside the recess.
// Inset by the larger of these so the slabs sit closer to the camera
// than the body wall and win the depth test cleanly.
const RECESS_WALL_THICKNESS = 0.2
const RECESS_WALL_INSET = 0.1

function buildPortMeshes(
  scene: Scene,
  station: Station,
  port: Port,
  materials: StationMaterials,
  index: number,
): PortBuild {
  // Outward direction along x/y, plus the across axis (the one running
  // along the face). For east/west the across axis is +y; for
  // north/south it's +x.
  const isXAxis = port.direction === 'east' || port.direction === 'west'
  const outwardSign = port.direction === 'east' || port.direction === 'north' ? 1 : -1
  const outwardX = isXAxis ? outwardSign : 0
  const outwardY = isXAxis ? 0 : outwardSign
  const acrossX = isXAxis ? 0 : 1
  const acrossY = isXAxis ? 1 : 0
  const acrossLen = isXAxis ? station.d : station.w

  // Center of the face at floor level, in world coords.
  let faceX: number
  let faceY: number
  switch (port.direction) {
    case 'east':
      faceX = station.x + station.w
      faceY = station.y + station.d / 2
      break
    case 'west':
      faceX = station.x
      faceY = station.y + station.d / 2
      break
    case 'north':
      faceX = station.x + station.w / 2
      faceY = station.y + station.d
      break
    case 'south':
      faceX = station.x + station.w / 2
      faceY = station.y
      break
  }

  // Snap point = face center shifted along the across axis by the
  // port's offset, raised to the conveyor's vertical centerline.
  const acrossDelta = (port.offset - 0.5) * acrossLen
  const snapX = faceX + acrossX * acrossDelta
  const snapY = faceY + acrossY * acrossDelta
  const snapZ = station.z + CONVEYOR_HEIGHT / 2

  // Recess opening: small frame across, generous head clearance above
  // (items ride into the station on the belt and need room to fit).
  const recessOpenW = CONVEYOR_WIDTH + 2 * PORT_RECESS_MARGIN_ACROSS
  const recessOpenH = CONVEYOR_HEIGHT + PORT_RECESS_MARGIN_ABOVE

  // ─── Cutout box ───
  // Outward overshoots the wall face by 1 unit; bottom overshoots
  // below the floor by 1 unit. Both overshoots break coplanar faces
  // cleanly so CSG doesn't leave hairlines.
  const cutoutOvershoot = 1
  const cutoutAlong = port.recessDepth + cutoutOvershoot
  const cutoutAlongOffset = (cutoutOvershoot - port.recessDepth) / 2
  const cutoutVertical = recessOpenH + 1
  const cutoutVerticalCenter = station.z + (recessOpenH - 1) / 2
  const cutoutSize = isXAxis
    ? { width: cutoutAlong, height: recessOpenW, depth: cutoutVertical }
    : { width: recessOpenW, height: cutoutAlong, depth: cutoutVertical }

  const cutout = MeshBuilder.CreateBox(`port-cut-${index}`, cutoutSize, scene)
  cutout.position.set(
    faceX + outwardX * cutoutAlongOffset + acrossX * acrossDelta,
    faceY + outwardY * cutoutAlongOffset + acrossY * acrossDelta,
    cutoutVerticalCenter,
  )

  // ─── LED frame (3 bars: top, left, right) ───
  // Sit on the wall surface, protruding outward by FRAME_PROTRUSION.
  // Bottom edge of the frame is the floor itself — no bottom bar.
  const recessTopZ = station.z + recessOpenH
  const frameOutwardCenter = FRAME_PROTRUSION / 2
  const frameMeshes: Mesh[] = []

  // Top bar — full outer width, sits above the recess opening.
  const topAcrossLen = recessOpenW + 2 * FRAME_THICKNESS
  const topZ = recessTopZ + FRAME_THICKNESS / 2
  const topSize = isXAxis
    ? { width: FRAME_PROTRUSION, height: topAcrossLen, depth: FRAME_THICKNESS }
    : { width: topAcrossLen, height: FRAME_PROTRUSION, depth: FRAME_THICKNESS }
  const topBar = MeshBuilder.CreateBox(`port-frame-${index}-top`, topSize, scene)
  topBar.position.set(
    faceX + outwardX * frameOutwardCenter + acrossX * acrossDelta,
    faceY + outwardY * frameOutwardCenter + acrossY * acrossDelta,
    topZ,
  )
  topBar.material = materials.ledTrim
  frameMeshes.push(topBar)

  // Side bars — span floor to top of recess opening (no overlap with
  // top bar in volume; they share the line at z=recessTopZ).
  const sideZ = station.z + recessOpenH / 2
  const sideAcrossOffset = recessOpenW / 2 + FRAME_THICKNESS / 2
  const sideSize = isXAxis
    ? { width: FRAME_PROTRUSION, height: FRAME_THICKNESS, depth: recessOpenH }
    : { width: FRAME_THICKNESS, height: FRAME_PROTRUSION, depth: recessOpenH }
  for (const sign of [-1, 1] as const) {
    const bar = MeshBuilder.CreateBox(
      `port-frame-${index}-${sign < 0 ? 'left' : 'right'}`,
      sideSize,
      scene,
    )
    bar.position.set(
      faceX + outwardX * frameOutwardCenter + acrossX * (acrossDelta + sign * sideAcrossOffset),
      faceY + outwardY * frameOutwardCenter + acrossY * (acrossDelta + sign * sideAcrossOffset),
      sideZ,
    )
    bar.material = materials.ledTrim
    frameMeshes.push(bar)
  }

  // ─── Recess interior walls ───
  // Thin slabs covering the body's CSG-cut surfaces so the interior
  // doesn't read as warm cream where the recess meets the body.
  // Inset slightly so the slabs sit closer to the camera than the
  // body wall and win the depth test cleanly.
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
      faceX + inX * alongCenter + acrossX * (acrossDelta + acrossOffset),
      faceY + inY * alongCenter + acrossY * (acrossDelta + acrossOffset),
      zCenter,
    )
    slab.material = materials.recessInterior
    return slab
  }

  // Back wall — at along = recessDepth - inset - t/2.
  recessWalls.push(
    placeWall(
      `port-recess-back-${index}`,
      port.recessDepth - wallInset - wallT / 2,
      0,
      station.z + recessOpenH / 2,
      wallT,
      recessOpenW,
      recessOpenH,
    ),
  )

  // Top wall — at z = recessOpenH - inset - t/2, spans the inner
  // length of the recess (less side insets so it doesn't poke out
  // past the side walls).
  recessWalls.push(
    placeWall(
      `port-recess-top-${index}`,
      port.recessDepth / 2,
      0,
      station.z + recessOpenH - wallInset - wallT / 2,
      port.recessDepth - 2 * wallInset,
      recessOpenW,
      wallT,
    ),
  )

  // Side walls (low + high across).
  for (const sign of [-1, 1] as const) {
    recessWalls.push(
      placeWall(
        `port-recess-side-${index}-${sign < 0 ? 'l' : 'r'}`,
        port.recessDepth / 2,
        sign * (recessOpenW / 2 - wallInset - wallT / 2),
        station.z + recessOpenH / 2,
        port.recessDepth - 2 * wallInset,
        wallT,
        recessOpenH,
      ),
    )
  }

  // ─── Port-stub belt ───
  // The stub IS a belt — same primitive used for connecting
  // conveyors. Flow direction depends on port kind: output ports
  // push items outward (back-of-recess → wall plane), input ports
  // pull them inward (wall plane → back-of-recess).
  //
  // No caps on the stub: the back-of-recess cap would clip through
  // the recess back-wall slab, and the wall-plane cap's bottom half
  // would extend below the belt surface inside the LED frame —
  // visually awkward where the connecting belt approaches. With
  // both ends flat the connecting belt butts up cleanly and the
  // chevron pattern flows continuously through the wall plane.
  const stubLength = port.recessDepth - STUB_BACK_GAP
  const wallEnd = new Vector3(
    faceX + acrossX * acrossDelta,
    faceY + acrossY * acrossDelta,
    station.z,
  )
  const recessEnd = new Vector3(
    faceX + inX * stubLength + acrossX * acrossDelta,
    faceY + inY * stubLength + acrossY * acrossDelta,
    station.z,
  )
  const isOutput = port.kind === 'output'
  const belt = buildBelt(
    scene,
    {
      start: isOutput ? recessEnd : wallEnd,
      end: isOutput ? wallEnd : recessEnd,
      pathOffset: 0,
      capStart: false,
      capEnd: false,
    },
    materials.beltSurface,
  )

  const handle: PortHandle = {
    port,
    worldPos: new Vector3(snapX, snapY, snapZ),
    outward: new Vector3(outwardX, outwardY, 0),
    // Stub belt's segment doubles as the port's internal path. For
    // input ports the segment runs wall → recess back (item enters
    // here, despawns at recess back unless a station-processing
    // layer claims it); for output ports it runs recess back →
    // wall (item emerges from box, exits onto a connecting belt).
    segment: belt.segment,
  }

  return { cutout, frameMeshes, recessWalls, belt, handle }
}

// ─── Builder ───────────────────────────────────────────────────────────────

/** Build one station as a hierarchy of Babylon meshes. Returns the
 *  TransformNode parent (move/animate it to translate the whole
 *  station, dispose it to remove all sub-meshes at once) plus a list
 *  of port handles (snap points for connecting conveyors). */
export function buildStationMesh(
  scene: Scene,
  station: Station,
  materials: StationMaterials,
): { root: TransformNode; ports: PortHandle[] } {
  const root = new TransformNode(`station_${station.x}_${station.y}`, scene)

  const cx = station.x + station.w / 2
  const cy = station.y + station.d / 2

  // ─── Body with carved chamber ─────────────────────────────────────────
  // CSG.subtract carves the chamber out of the body so the recess is
  // a real geometric hole, not a shadow trick. Result is a single
  // mesh with the body material.

  const bodyTmp = MeshBuilder.CreateBox(
    'body-tmp',
    { width: station.w, height: station.d, depth: station.h },
    scene,
  )
  bodyTmp.position.set(cx, cy, station.z + station.h / 2)

  const chamberW = station.w * (CHAMBER_X_FRAC[1] - CHAMBER_X_FRAC[0])
  const chamberD = station.d * (CHAMBER_Y_FRAC[1] - CHAMBER_Y_FRAC[0])
  const chamberH = station.h * CHAMBER_DEPTH_FRAC
  const chamberCx = station.x + (station.w * (CHAMBER_X_FRAC[0] + CHAMBER_X_FRAC[1])) / 2
  const chamberCy = station.y + (station.d * (CHAMBER_Y_FRAC[0] + CHAMBER_Y_FRAC[1])) / 2
  const chamberTopZ = station.z + station.h
  // The cutout is slightly taller than the chamber and shifted up so
  // its top breaks through the body's top face cleanly. Without this
  // overshoot, CSG can leave a hairline rim at the chamber opening.
  const cutoutOvershoot = 2

  const cutoutTmp = MeshBuilder.CreateBox(
    'cutout-tmp',
    {
      width: chamberW,
      height: chamberD,
      depth: chamberH + cutoutOvershoot,
    },
    scene,
  )
  cutoutTmp.position.set(chamberCx, chamberCy, chamberTopZ - chamberH / 2 + cutoutOvershoot / 2)

  // LED groove — a thin band that's wider than the chamber, restricted
  // to a narrow vertical slice partway up from the floor. CSG-subtract
  // it from the body and the chamber walls recede by LED_GROOVE_DEPTH
  // for that slice — a real machined channel where the LED sits.
  const chamberFloorTopZ = chamberTopZ - chamberH
  const chamberFloorLift = 1
  const ledRingZ = chamberFloorTopZ + chamberFloorLift + LED_RING_FROM_FLOOR
  const grooveTmp = MeshBuilder.CreateBox(
    'groove-tmp',
    {
      width: chamberW + LED_GROOVE_DEPTH * 2,
      height: chamberD + LED_GROOVE_DEPTH * 2,
      depth: LED_GROOVE_HEIGHT,
    },
    scene,
  )
  grooveTmp.position.set(chamberCx, chamberCy, ledRingZ)

  // Vent slot cutouts — five thin horizontal slots through the back
  // (high-y) face. Each cutout overshoots the front face slightly so
  // the cut breaks through cleanly without a hairline.
  const ventCutouts: Array<ReturnType<typeof MeshBuilder.CreateBox>> = []
  const ventZs: number[] = []
  const ventCutoutDepth = VENT_DEPTH + 0.5
  const ventW = station.w * VENT_WIDTH_FRAC
  const ventCy = station.y + station.d - ventCutoutDepth / 2 + 0.25
  for (let i = 0; i < VENT_COUNT; i++) {
    const t = (i + 0.5) / VENT_COUNT
    const zFrac = VENT_Z_FRAC_RANGE[0] + t * (VENT_Z_FRAC_RANGE[1] - VENT_Z_FRAC_RANGE[0])
    const ventZ = station.z + station.h * zFrac
    const v = MeshBuilder.CreateBox(
      `vent-cut-${i}`,
      { width: ventW, height: ventCutoutDepth, depth: VENT_HEIGHT },
      scene,
    )
    v.position.set(cx, ventCy, ventZ)
    ventCutouts.push(v)
    ventZs.push(ventZ)
  }

  // Status panel recess — single shallow cutout on the front (low-y)
  // face. A separate dark "screen" mesh sits inside the recess (added
  // after body creation).
  const panelCx = station.x + station.w * STATUS_PANEL_X_FRAC
  const panelCutDepth = STATUS_PANEL_DEPTH + 0.5
  const panelCz = station.z + station.h * STATUS_PANEL_Z_FRAC
  const panelTmp = MeshBuilder.CreateBox(
    'panel-cut',
    { width: STATUS_PANEL_W, height: panelCutDepth, depth: STATUS_PANEL_H },
    scene,
  )
  panelTmp.position.set(panelCx, station.y + panelCutDepth / 2 - 0.25, panelCz)

  // Heat sink housing recess — shallow rectangular cut on the output
  // (high-x) face, sized to contain the fin array with a small frame
  // of body material around it. Body wall becomes a visible frame
  // around the heatsink module rather than fins glued to a flat side.
  const finsCenterY = station.y + station.d * HEATSINK_FIN_Y_FRAC
  const finsCenterZ = station.z + station.h * HEATSINK_FIN_Z_FRAC
  const finHeight = station.h * HEATSINK_FIN_HEIGHT_FRAC
  const finsTotalY = (HEATSINK_FIN_COUNT - 1) * HEATSINK_FIN_SPACING + HEATSINK_FIN_THICKNESS
  const housingCutDepth = HEATSINK_HOUSING_DEPTH + 0.5
  const housingD = finsTotalY + HEATSINK_HOUSING_PADDING_Y * 2
  const housingH = finHeight + HEATSINK_HOUSING_PADDING_Z * 2
  const housingTmp = MeshBuilder.CreateBox(
    'housing-cut',
    { width: housingCutDepth, height: housingD, depth: housingH },
    scene,
  )
  housingTmp.position.set(
    station.x + station.w - housingCutDepth / 2 + 0.25,
    finsCenterY,
    finsCenterZ,
  )

  // Port meshes — built before CSG so each port's cutout joins the
  // single subtraction chain. Frames and stubs are independent meshes
  // we'll parent to root after the body is materialized.
  const portBuilds: PortBuild[] = (station.ports ?? []).map((p, i) =>
    buildPortMeshes(scene, station, p, materials, i),
  )

  // Single CSG chain — chamber, LED groove, vents, status panel,
  // heatsink housing, port recesses — so the body is computed once
  // with all subtractions applied.
  let bodyCSG = CSG.FromMesh(bodyTmp)
    .subtract(CSG.FromMesh(cutoutTmp))
    .subtract(CSG.FromMesh(grooveTmp))
    .subtract(CSG.FromMesh(panelTmp))
    .subtract(CSG.FromMesh(housingTmp))
  for (const v of ventCutouts) {
    bodyCSG = bodyCSG.subtract(CSG.FromMesh(v))
  }
  for (const pb of portBuilds) {
    bodyCSG = bodyCSG.subtract(CSG.FromMesh(pb.cutout))
  }
  const body = bodyCSG.toMesh('station-body', materials.body, scene, true)
  body.parent = root

  bodyTmp.dispose()
  cutoutTmp.dispose()
  grooveTmp.dispose()
  panelTmp.dispose()
  housingTmp.dispose()
  for (const v of ventCutouts) v.dispose()
  for (const pb of portBuilds) {
    pb.cutout.dispose()
    for (const f of pb.frameMeshes) f.parent = root
    for (const w of pb.recessWalls) w.parent = root
    pb.belt.root.parent = root
  }

  // Status screen — dark "tablet face" inset into the panel recess.
  // Sits at the back wall of the recess with a tiny lift to avoid
  // z-fight against the CSG-cut back face.
  const statusScreen = MeshBuilder.CreateBox(
    'status-screen',
    {
      width: STATUS_PANEL_W - STATUS_SCREEN_INSET * 2,
      height: STATUS_SCREEN_THICKNESS,
      depth: STATUS_PANEL_H - STATUS_SCREEN_INSET * 2,
    },
    scene,
  )
  statusScreen.position.set(
    panelCx,
    station.y + STATUS_PANEL_DEPTH - STATUS_SCREEN_THICKNESS / 2 - 0.05,
    panelCz,
  )
  statusScreen.material = materials.screen
  statusScreen.parent = root

  // ─── Vent glow slabs ──────────────────────────────────────────────────
  // One emissive orange slab inside each vent recess, sitting flush
  // against the recess back wall (with a tiny lift to avoid z-fight).
  // Picked up by the renderer's GlowLayer so each slot blooms warmly
  // — reads as "active heat venting" through the slot bars left by
  // the body material around the slab.

  const ventGlowBackY = station.y + station.d - VENT_DEPTH + VENT_GLOW_THICKNESS / 2 + 0.05
  const ventGlowW = ventW - VENT_GLOW_INSET * 2
  const ventGlowH = VENT_HEIGHT - VENT_GLOW_INSET * 2
  for (let i = 0; i < ventZs.length; i++) {
    const glow = MeshBuilder.CreateBox(
      `vent-glow-${i}`,
      { width: ventGlowW, height: VENT_GLOW_THICKNESS, depth: ventGlowH },
      scene,
    )
    glow.position.set(cx, ventGlowBackY, ventZs[i])
    glow.material = materials.ventGlow
    glow.parent = root
  }

  // ─── Chamber floor inset ──────────────────────────────────────────────
  // Lifted 1 unit above the body's CSG-cut floor to give the depth
  // test a clean winner — without the lift, two coplanar surfaces
  // fight and flicker on camera move.
  // (chamberFloorTopZ and chamberFloorLift are already declared above
  // for the LED groove math.)

  const floorThickness = 4
  const chamberFloorInset = 1
  const floor = MeshBuilder.CreateBox(
    'chamber-floor',
    {
      width: chamberW - chamberFloorInset * 2,
      height: chamberD - chamberFloorInset * 2,
      depth: floorThickness,
    },
    scene,
  )
  floor.position.set(chamberCx, chamberCy, chamberFloorTopZ + chamberFloorLift - floorThickness / 2)
  floor.material = materials.chamberFloor
  floor.parent = root

  // ─── LED ring inside chamber wall groove ──────────────────────────────
  // Four emissive strips, one per chamber wall, sitting inside the
  // groove we CSG-cut above. Each strip's chamber-facing surface is
  // flush with the original (un-grooved) chamber wall so the LED
  // reads as recessed into the wall rather than glued onto it. The
  // renderer's GlowLayer blooms the emissive contribution.

  const chamberX0 = station.x + station.w * CHAMBER_X_FRAC[0]
  const chamberX1 = station.x + station.w * CHAMBER_X_FRAC[1]
  const chamberY0 = station.y + station.d * CHAMBER_Y_FRAC[0]
  const chamberY1 = station.y + station.d * CHAMBER_Y_FRAC[1]
  const ledStripT = LED_GROOVE_DEPTH * 0.85 // slight gap to back of groove

  const ledStrips: Array<{
    name: string
    cx: number
    cy: number
    width: number
    height: number
  }> = [
    // South wall (low y)
    {
      name: 'led-s',
      cx: (chamberX0 + chamberX1) / 2,
      cy: chamberY0 - ledStripT / 2,
      width: chamberX1 - chamberX0 - 2 * LED_END_INSET,
      height: ledStripT,
    },
    // North wall (high y)
    {
      name: 'led-n',
      cx: (chamberX0 + chamberX1) / 2,
      cy: chamberY1 + ledStripT / 2,
      width: chamberX1 - chamberX0 - 2 * LED_END_INSET,
      height: ledStripT,
    },
    // West wall (low x)
    {
      name: 'led-w',
      cx: chamberX0 - ledStripT / 2,
      cy: (chamberY0 + chamberY1) / 2,
      width: ledStripT,
      height: chamberY1 - chamberY0 - 2 * LED_END_INSET,
    },
    // East wall (high x)
    {
      name: 'led-e',
      cx: chamberX1 + ledStripT / 2,
      cy: (chamberY0 + chamberY1) / 2,
      width: ledStripT,
      height: chamberY1 - chamberY0 - 2 * LED_END_INSET,
    },
  ]
  for (const cfg of ledStrips) {
    const strip = MeshBuilder.CreateBox(
      cfg.name,
      { width: cfg.width, height: cfg.height, depth: LED_HEIGHT },
      scene,
    )
    strip.position.set(cfg.cx, cfg.cy, ledRingZ)
    strip.material = materials.ledTrim
    strip.parent = root
  }

  // ─── Glass canopy ─────────────────────────────────────────────────────
  // Thin transparent lid sitting on the body's top surface, slightly
  // wider than the chamber opening so it reads as a glass cover with
  // a small bezel rather than a floating shield.

  const canopyW = chamberW + CANOPY_OVERHANG * 2
  const canopyD = chamberD + CANOPY_OVERHANG * 2
  const canopy = MeshBuilder.CreateBox(
    'canopy',
    { width: canopyW, height: canopyD, depth: CANOPY_THICKNESS },
    scene,
  )
  canopy.position.set(chamberCx, chamberCy, chamberTopZ + CANOPY_LIFT + CANOPY_THICKNESS / 2)
  canopy.material = materials.glass
  canopy.parent = root

  // ─── Heat sink module (back plate + fins) ─────────────────────────────
  // Sits inside the housing recess we CSG-cut into the right face.
  // The back plate gives the fins a visible mounting surface; the
  // recess walls (body material from CSG) frame the whole module so
  // it reads as a heatsink unit bolted into the chassis rather than
  // fins floating on a featureless wall.

  // Back plate — thin metallic slab against the recess back wall.
  const backplateOuterX =
    station.x + station.w - HEATSINK_HOUSING_DEPTH + HEATSINK_BACKPLATE_THICKNESS
  const backplate = MeshBuilder.CreateBox(
    'heatsink-backplate',
    {
      width: HEATSINK_BACKPLATE_THICKNESS,
      height: housingD - 1.5,
      depth: housingH - 1.5,
    },
    scene,
  )
  backplate.position.set(
    backplateOuterX - HEATSINK_BACKPLATE_THICKNESS / 2,
    finsCenterY,
    finsCenterZ,
  )
  backplate.material = materials.chamberFloor
  backplate.parent = root

  // Fins mount on the back plate's outer face — their inner edge sits
  // flush with the plate, their outer edge protrudes past the body's
  // original right face.
  const finStartY = finsCenterY - ((HEATSINK_FIN_COUNT - 1) * HEATSINK_FIN_SPACING) / 2
  const finInnerX = backplateOuterX + 0.05 // tiny lift to avoid z-fight on the plate face
  for (let i = 0; i < HEATSINK_FIN_COUNT; i++) {
    const fin = MeshBuilder.CreateBox(
      `heatsink-${i}`,
      {
        width: HEATSINK_FIN_EXTENT,
        height: HEATSINK_FIN_THICKNESS,
        depth: finHeight,
      },
      scene,
    )
    fin.position.set(
      finInnerX + HEATSINK_FIN_EXTENT / 2,
      finStartY + i * HEATSINK_FIN_SPACING,
      finsCenterZ,
    )
    fin.material = materials.heatsinkFin
    fin.parent = root
  }

  // ─── Corner anchor discs ──────────────────────────────────────────────
  // Four small metallic discs near the bottom corners of the long
  // faces. Reads as "this thing is bolted to the factory floor" —
  // grounds the silhouette without going full mil-spec rivet pattern.
  // CreateCylinder is y-axis aligned by default, which is exactly
  // what we want here (axis perpendicular to the y-facing wall).

  const anchorZ = station.z + station.h * ANCHOR_Z_FRAC
  const anchorPositions: Array<{ x: number; y: number }> = [
    { x: station.x + ANCHOR_X_INSET, y: station.y - ANCHOR_THICKNESS / 2 },
    { x: station.x + station.w - ANCHOR_X_INSET, y: station.y - ANCHOR_THICKNESS / 2 },
    { x: station.x + ANCHOR_X_INSET, y: station.y + station.d + ANCHOR_THICKNESS / 2 },
    {
      x: station.x + station.w - ANCHOR_X_INSET,
      y: station.y + station.d + ANCHOR_THICKNESS / 2,
    },
  ]
  for (let i = 0; i < anchorPositions.length; i++) {
    const p = anchorPositions[i]
    const anchor = MeshBuilder.CreateCylinder(
      `anchor-${i}`,
      { height: ANCHOR_THICKNESS, diameter: ANCHOR_DIAM, tessellation: 24 },
      scene,
    )
    anchor.position.set(p.x, p.y, anchorZ)
    anchor.material = materials.chamberFloor
    anchor.parent = root
  }

  // ─── Landing pad ──────────────────────────────────────────────────────

  const padW = station.w * (PAD_X_FRAC[1] - PAD_X_FRAC[0])
  const padD = station.d * (PAD_Y_FRAC[1] - PAD_Y_FRAC[0])
  const padH = station.h * PAD_LIFT_FRAC
  const padCx = station.x + (station.w * (PAD_X_FRAC[0] + PAD_X_FRAC[1])) / 2
  const padCy = station.y + (station.d * (PAD_Y_FRAC[0] + PAD_Y_FRAC[1])) / 2
  const padBottomZ = station.z + station.h
  const padTopZ = padBottomZ + padH

  const pad = MeshBuilder.CreateBox(
    'landing-pad',
    { width: padW, height: padD, depth: padH },
    scene,
  )
  pad.position.set(padCx, padCy, padBottomZ + padH / 2)
  pad.material = materials.pad
  pad.parent = root

  // ─── Queued chips on pad ──────────────────────────────────────────────
  // Each chip is a glass cylinder shell with a smaller emissive core
  // visible inside. CreateCylinder's natural axis is +y; rotation.x
  // = π/2 stands it on +z (our up axis).

  const queuedCount = Math.min(station.queuedCount ?? 0, MAX_QUEUED)
  for (let i = 0; i < queuedCount; i++) {
    const chipZ = padTopZ + QUEUED_CHIP_H / 2 + i * (QUEUED_CHIP_H + QUEUED_CHIP_GAP)
    const shell = MeshBuilder.CreateCylinder(
      `queued-shell-${i}`,
      { height: QUEUED_CHIP_H, diameter: QUEUED_CHIP_DIAM, tessellation: 28 },
      scene,
    )
    shell.rotation.x = Math.PI / 2
    shell.position.set(padCx, padCy, chipZ)
    shell.material = materials.queuedShell
    shell.parent = root

    const core = MeshBuilder.CreateCylinder(
      `queued-core-${i}`,
      { height: QUEUED_CORE_H, diameter: QUEUED_CORE_DIAM, tessellation: 20 },
      scene,
    )
    core.rotation.x = Math.PI / 2
    core.position.set(padCx, padCy, chipZ)
    core.material = materials.queuedCore
    core.parent = root
  }

  // ─── WIP chips in chamber ─────────────────────────────────────────────

  const wipCount = Math.min(station.wipCount ?? 0, MAX_WIP)
  if (wipCount > 0) {
    const span = chamberW * 0.6
    const startX = chamberCx - span / 2
    // Sit chips on the lifted chamber floor inset, not on the body's
    // CSG-cut floor below it.
    const wipBottomZ = chamberFloorTopZ + chamberFloorLift
    for (let i = 0; i < wipCount; i++) {
      const t = wipCount === 1 ? 0.5 : i / (wipCount - 1)
      const chipX = startX + t * span
      const chipZ = wipBottomZ + WIP_CHIP_H / 2

      const shell = MeshBuilder.CreateCylinder(
        `wip-shell-${i}`,
        { height: WIP_CHIP_H, diameter: WIP_CHIP_DIAM, tessellation: 28 },
        scene,
      )
      shell.rotation.x = Math.PI / 2
      shell.position.set(chipX, chamberCy, chipZ)
      shell.material = materials.wipShell
      shell.parent = root

      const core = MeshBuilder.CreateCylinder(
        `wip-core-${i}`,
        { height: WIP_CORE_H, diameter: WIP_CORE_DIAM, tessellation: 20 },
        scene,
      )
      core.rotation.x = Math.PI / 2
      core.position.set(chipX, chamberCy, chipZ)
      core.material = materials.wipCore
      core.parent = root
    }
  }

  return { root, ports: portBuilds.map((pb) => pb.handle) }
}
