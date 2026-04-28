// Stage-1 station, built as a hierarchy of Babylon meshes (SKY-197).
//
// Form factor: a chamfered "transistor" slab with three zones along its
// long axis:
//
//   ┌──────────────────────────────────────────┐
//   │  ┌───┐    ┌──────────────┐               │
//   │  │pad│    │   chamber    │               │
//   │  └───┘    └──────────────┘               │
//   └──────────────────────────────────────────┘
//      LEFT       CENTER (recessed)     RIGHT
//      queued      WIP items            (exit, future)
//
// The chamber is carved out of the body via Constructive Solid Geometry
// (CSG.subtract). The result is a single Babylon mesh with the chamber
// as a real recess, no manual face emission. Pad, queued chips, and
// WIP chips are independent boxes parented to a common TransformNode
// so the whole station moves as a unit (and we can later animate
// individual parts — chips lifting, etc.).

import { Color3, CSG, MeshBuilder, Scene, StandardMaterial, TransformNode } from '@babylonjs/core'

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
}

// ─── Layout fractions ───────────────────────────────────────────────────────
// All zones expressed as fractions of the station footprint so geometry
// scales with whatever (w, d, h) is passed in.

const PAD_X_FRAC: readonly [number, number] = [0.06, 0.22]
const PAD_Y_FRAC: readonly [number, number] = [0.28, 0.72]
const PAD_LIFT_FRAC = 0.18

const CHAMBER_X_FRAC: readonly [number, number] = [0.32, 0.92]
const CHAMBER_Y_FRAC: readonly [number, number] = [0.18, 0.82]
const CHAMBER_DEPTH_FRAC = 0.55

// ─── Chip dimensions ───────────────────────────────────────────────────────

const QUEUED_CHIP_W = 22
const QUEUED_CHIP_D = 22
const QUEUED_CHIP_H = 6
const QUEUED_CHIP_GAP = 1.5

const WIP_CHIP_W = 16
const WIP_CHIP_D = 16
const WIP_CHIP_H = 28

const MAX_QUEUED = 6
const MAX_WIP = 3

// ─── Materials ──────────────────────────────────────────────────────────────

export interface StationMaterials {
  body: StandardMaterial
  chamberFloor: StandardMaterial
  pad: StandardMaterial
  queuedChip: StandardMaterial
  wipChip: StandardMaterial
}

export function createStationMaterials(scene: Scene): StationMaterials {
  const make = (
    name: string,
    diffuseHex: string,
    opts?: {
      specular?: number
      specularPower?: number
      emissiveHex?: string
    },
  ): StandardMaterial => {
    const m = new StandardMaterial(name, scene)
    m.diffuseColor = Color3.FromHexString(diffuseHex)
    const spec = opts?.specular ?? 0.12
    m.specularColor = new Color3(spec, spec, spec)
    m.specularPower = opts?.specularPower ?? 80
    if (opts?.emissiveHex) {
      m.emissiveColor = Color3.FromHexString(opts.emissiveHex)
    }
    return m
  }

  return {
    // Body: warm off-white, soft sheen for the "Shanghai hotel lobby"
    // / liquid-glass feel.
    body: make('station-body', '#ece6d8', { specular: 0.18, specularPower: 96 }),
    // Chamber inset: cooler, darker grey — reads as "machined recess"
    // even before we add real ambient occlusion.
    chamberFloor: make('chamber-floor', '#9a9388', {
      specular: 0.04,
      specularPower: 48,
    }),
    // Pad: brighter than body so the queued chips have a clean stage.
    pad: make('pad', '#f3eee0', { specular: 0.22, specularPower: 96 }),
    // Queued chips: warm beige, like brass tokens.
    queuedChip: make('queued-chip', '#d9c894', { specular: 0.12 }),
    // WIP chips: bright accent blue, slight emissive to read as "active"
    // even when the rest of the scene is in shadow.
    wipChip: make('wip-chip', '#6a8cff', {
      specular: 0.4,
      specularPower: 128,
      emissiveHex: '#1a2855',
    }),
  }
}

// ─── Builder ────────────────────────────────────────────────────────────────

/** Build one station as a hierarchy of Babylon meshes. Returns a
 * TransformNode parent — move/animate it to translate the whole station,
 * dispose it to remove all sub-meshes at once. */
export function buildStationMesh(
  scene: Scene,
  station: Station,
  materials: StationMaterials,
): TransformNode {
  const root = new TransformNode(`station_${station.x}_${station.y}`, scene)

  const cx = station.x + station.w / 2
  const cy = station.y + station.d / 2

  // ─── Body with carved chamber ─────────────────────────────────────────
  // CreateBox uses width/height/depth = x/y/z extents, with the box
  // centered at its position. We use CSG.subtract to carve the chamber
  // hole out of the top of the body — the result is one mesh with a
  // real recess, not a shadow trick.

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

  const carved = CSG.FromMesh(bodyTmp).subtract(CSG.FromMesh(cutoutTmp))
  const body = carved.toMesh('station-body', materials.body, scene, true)
  body.parent = root

  bodyTmp.dispose()
  cutoutTmp.dispose()

  // ─── Chamber floor inset ──────────────────────────────────────────────
  // A separate flat slab sits just inside the carved hole, with a
  // different (darker, cooler) material. CSG would put the chamber
  // walls and floor on the same material as the body; this gives us a
  // visible distinction between "machined wall" and "machined floor"
  // for free.
  //
  // The body's CSG-cut chamber floor lives at z = chamberFloorTopZ
  // (the cutout's bottom face becomes part of the body mesh after
  // subtract). If the inset's top is also at that z, the two
  // coplanar surfaces fight for the depth buffer — visible as
  // flicker when the camera moves. Lifting the inset by 1 unit
  // gives the depth test an unambiguous winner.

  const chamberFloorTopZ = chamberTopZ - chamberH
  const floorThickness = 4
  const chamberFloorInset = 1
  const chamberFloorLift = 1
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

  const queuedCount = Math.min(station.queuedCount ?? 0, MAX_QUEUED)
  for (let i = 0; i < queuedCount; i++) {
    const chip = MeshBuilder.CreateBox(
      `queued-chip-${i}`,
      { width: QUEUED_CHIP_W, height: QUEUED_CHIP_D, depth: QUEUED_CHIP_H },
      scene,
    )
    const cz = padTopZ + QUEUED_CHIP_H / 2 + i * (QUEUED_CHIP_H + QUEUED_CHIP_GAP)
    chip.position.set(padCx, padCy, cz)
    chip.material = materials.queuedChip
    chip.parent = root
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
      const chip = MeshBuilder.CreateBox(
        `wip-chip-${i}`,
        { width: WIP_CHIP_W, height: WIP_CHIP_D, depth: WIP_CHIP_H },
        scene,
      )
      chip.position.set(startX + t * span, chamberCy, wipBottomZ + WIP_CHIP_H / 2)
      chip.material = materials.wipChip
      chip.parent = root
    }
  }

  return root
}
