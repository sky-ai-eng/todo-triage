// Belt primitive — a single ribbon mesh traced around the belt's
// outer perimeter (back cap → top edge → front cap), with chevron
// texture UVs baked at construction time so the pattern tiles
// continuously around the path. Conceptually this is the "tank
// tread" / escalator model: one closed surface where the chevrons
// are printed once and the entire surface appears to rotate via
// uOffset animation.
//
// One standardized geometry is used by every belt-shaped thing in
// the scene: port stubs, station-to-station conveyors, and belts
// spanning poles. Chevron continuity at junctions is achieved via
// path-offset arithmetic — each belt knows its cumulative path
// position from its chain origin and bakes that into UVs, so two
// adjacent belts produce UVs that agree mod 1 at the seam.
//
// At a junction between two belts, suppress the cap on whichever
// side meets another belt's flat end (capStart/capEnd: false). The
// belts butt up flat-to-flat and chevrons flow continuously across
// them. Free ends (e.g. inside a port's recess back, or against a
// pole column) keep their cap rendered — the wraparound chevron
// reads as the belt's roller.

import {
  Color3,
  DynamicTexture,
  Mesh,
  PBRMaterial,
  Scene,
  Texture,
  TransformNode,
  Vector3,
  VertexData,
} from '@babylonjs/core'

import { CONVEYOR_HEIGHT, CONVEYOR_WIDTH } from './iso-port'

/** World units between adjacent chevrons along the belt path. Shared
 *  across all belts so chevron continuity at junctions is purely a
 *  matter of pathOffset arithmetic. */
export const CHEVRON_SPACING_WORLD = 54

/** Texture U units per second. With CHEVRON_SPACING_WORLD baked into
 *  UVs, 1 unit of uOffset == one chevron's worth of scroll. */
const BELT_SCROLL_SPEED = 5

/** Radius of the rounded end caps. Half the belt's vertical extent so
 *  each cap fills the belt-height envelope exactly. */
const CAP_RADIUS = CONVEYOR_HEIGHT / 2

/** Tessellation segments around each half-cap. */
const CAP_TESSELLATION = 12

export interface BeltSpec {
  /** World position of the back end of the belt's top surface (at
   *  the floor level — z is the floor; the belt top sits at
   *  z + CONVEYOR_HEIGHT). */
  start: Vector3
  /** World position of the front end of the belt's top surface. */
  end: Vector3
  /** Cumulative path length from the chain origin to this belt's
   *  start. Used for chevron continuity across joined segments. */
  pathOffset: number
  /** Render the back rounded cap? Defaults true. Suppress at any
   *  junction where another belt's flat end butts up. */
  capStart?: boolean
  /** Render the front rounded cap? Defaults true. */
  capEnd?: boolean
}

export interface BeltBuild {
  /** Parent transform — set its parent to your scene/station root. */
  root: TransformNode
  /** Visible mesh (single ribbon around the perimeter). */
  meshes: Mesh[]
  /** Total world-units traversed by this segment, including any
   *  rendered caps. Add to the next segment's pathOffset to keep
   *  chevrons continuous. */
  pathLength: number
}

/** Build a belt segment as a single ribbon mesh tracing the outer
 *  perimeter (back cap, top edge, front cap). The chevron texture
 *  in `material` is the one returned by `createBeltMaterial`; its
 *  uOffset is animated globally and per-belt UV continuity is
 *  handled by the path-offset bake done here. */
export function buildBelt(scene: Scene, spec: BeltSpec, material: PBRMaterial): BeltBuild {
  const startToEnd = spec.end.subtract(spec.start)
  const topLength = startToEnd.length()
  const flowAngle = topLength > 0 ? Math.atan2(startToEnd.y, startToEnd.x) : 0

  const r = CAP_RADIUS
  const H = CONVEYOR_HEIGHT
  const halfW = CONVEYOR_WIDTH / 2
  const capPerim = Math.PI * r
  const spacing = CHEVRON_SPACING_WORLD

  const renderCapStart = spec.capStart ?? true
  const renderCapEnd = spec.capEnd ?? true

  // Walk the perimeter as a list of (x, z, u_path) tuples in
  // belt-local frame: back-cap (bottom up around -x to top) →
  // top edge → front-cap (top down around +x to bottom). Each
  // u_path entry is the cumulative path length from the start of
  // the perimeter, so the chevron texture tiles naturally along it.
  type PathPoint = { x: number; z: number; u: number }
  const path: PathPoint[] = []
  let pathLen = 0

  if (renderCapStart) {
    // Half-arc centered at (0, H/2), radius r. a=0 → bottom (0, 0);
    // a=π/2 → outermost (-r, H/2); a=π → top (0, H).
    for (let i = 0; i <= CAP_TESSELLATION; i++) {
      const a = (i / CAP_TESSELLATION) * Math.PI
      path.push({
        x: -r * Math.sin(a),
        z: H / 2 - r * Math.cos(a),
        u: pathLen + a * r,
      })
    }
    pathLen += capPerim
  } else {
    // No back cap — start at the back-top corner.
    path.push({ x: 0, z: H, u: 0 })
  }

  // Top edge — last path point above is at (0, H). Continue to the
  // far end at (topLength, H).
  path.push({ x: topLength, z: H, u: pathLen + topLength })
  pathLen += topLength

  if (renderCapEnd) {
    // Half-arc centered at (topLength, H/2), radius r. a=0 → top
    // (topLength, H); a=π/2 → outermost (topLength+r, H/2); a=π →
    // bottom (topLength, 0). Skip i=0 — the previous push already
    // added the top vertex.
    for (let i = 1; i <= CAP_TESSELLATION; i++) {
      const a = (i / CAP_TESSELLATION) * Math.PI
      path.push({
        x: topLength + r * Math.sin(a),
        z: H / 2 + r * Math.cos(a),
        u: pathLen + a * r,
      })
    }
    pathLen += capPerim
  }

  // Build the ribbon. Each path point becomes 2 vertices at y=±halfW;
  // adjacent path points form a quad (two triangles) connecting them.
  // UVs: u along the path, v across the width.
  const positions: number[] = []
  const uvs: number[] = []
  const indices: number[] = []

  // Normalize the path offset modulo CHEVRON_SPACING_WORLD so all
  // baked UV values stay in a small positive range. Adjacent belts
  // produce different normalized offsets but their junction UV values
  // still agree mod 1 (the texture's WRAP-mode sampling handles the
  // rest), preserving chevron continuity at the seam.
  const offsetWorld = ((spec.pathOffset % spacing) + spacing) % spacing

  for (const p of path) {
    const u = (offsetWorld + p.u) / spacing
    positions.push(p.x, -halfW, p.z) // back-side vertex (low y)
    uvs.push(u, 0)
    positions.push(p.x, +halfW, p.z) // front-side vertex (high y)
    uvs.push(u, 1)
  }

  for (let i = 0; i < path.length - 1; i++) {
    const v0 = i * 2 // back of point i
    const v1 = v0 + 1 // front of point i
    const v2 = (i + 1) * 2 // back of point i+1
    const v3 = v2 + 1 // front of point i+1
    // Winding chosen so the quad's normal points outward from the
    // belt's interior (radially-outward on caps, +z on the top edge).
    indices.push(v0, v3, v1)
    indices.push(v0, v2, v3)
  }

  const vd = new VertexData()
  vd.positions = positions
  vd.uvs = uvs
  vd.indices = indices
  const normals: number[] = []
  VertexData.ComputeNormals(positions, indices, normals)
  vd.normals = normals

  const mesh = new Mesh('belt', scene)
  vd.applyToMesh(mesh)
  mesh.material = material

  // Wrap in a TransformNode so the caller can move/rotate the whole
  // belt as a unit (e.g., parent to a station root for stub belts).
  const root = new TransformNode('belt-root', scene)
  root.position.copyFrom(spec.start)
  root.rotation.z = flowAngle
  mesh.parent = root

  return { root, meshes: [mesh], pathLength: pathLen }
}

/** Build a flat belt mesh that follows an arbitrary 2D path in the
 *  xy plane. Each waypoint becomes two vertices — one on each side of
 *  the path, perpendicular to the local tangent. Used by turn poles to
 *  smoothly curve a belt around a corner; the texture's chevrons tile
 *  along the path's arc length and visually rotate as the path bends.
 *
 *  No caps — curved belts are always belt-to-belt junctions.
 *
 *  The path is given in world coords with z = floor level (the belt's
 *  top sits at z + CONVEYOR_HEIGHT). For tight curves the belt's inner
 *  edge will be more compressed than the outer; this is normal — pick
 *  a tessellation high enough that the chord error is negligible. */
export function buildCurvedBelt(
  scene: Scene,
  pathPoints: Vector3[],
  pathOffset: number,
  material: PBRMaterial,
): BeltBuild {
  if (pathPoints.length < 2) {
    throw new Error('buildCurvedBelt requires at least 2 waypoints')
  }

  const halfW = CONVEYOR_WIDTH / 2
  const spacing = CHEVRON_SPACING_WORLD

  // Cumulative arc length at each waypoint (for chevron UV).
  const cumLengths: number[] = [0]
  for (let i = 1; i < pathPoints.length; i++) {
    const dx = pathPoints[i].x - pathPoints[i - 1].x
    const dy = pathPoints[i].y - pathPoints[i - 1].y
    const dz = pathPoints[i].z - pathPoints[i - 1].z
    cumLengths.push(cumLengths[i - 1] + Math.sqrt(dx * dx + dy * dy + dz * dz))
  }
  const pathLen = cumLengths[cumLengths.length - 1]

  const positions: number[] = []
  const uvs: number[] = []
  const indices: number[] = []

  for (let i = 0; i < pathPoints.length; i++) {
    const p = pathPoints[i]

    // Tangent in xy. Average forward + backward chords at interior
    // points; one-sided at the endpoints.
    let tx: number, ty: number
    if (i === 0) {
      tx = pathPoints[1].x - pathPoints[0].x
      ty = pathPoints[1].y - pathPoints[0].y
    } else if (i === pathPoints.length - 1) {
      tx = pathPoints[i].x - pathPoints[i - 1].x
      ty = pathPoints[i].y - pathPoints[i - 1].y
    } else {
      tx = pathPoints[i + 1].x - pathPoints[i - 1].x
      ty = pathPoints[i + 1].y - pathPoints[i - 1].y
    }
    const tLen = Math.sqrt(tx * tx + ty * ty)
    tx /= tLen
    ty /= tLen

    // Perpendicular, 90° CCW (looking from +z): (tx, ty) → (-ty, tx).
    const px = -ty
    const py = tx

    const u = (pathOffset + cumLengths[i]) / spacing
    const z = p.z + CONVEYOR_HEIGHT

    positions.push(p.x - halfW * px, p.y - halfW * py, z)
    uvs.push(u, 0)
    positions.push(p.x + halfW * px, p.y + halfW * py, z)
    uvs.push(u, 1)
  }

  for (let i = 0; i < pathPoints.length - 1; i++) {
    const v0 = i * 2
    const v1 = v0 + 1
    const v2 = (i + 1) * 2
    const v3 = v2 + 1
    indices.push(v0, v3, v1)
    indices.push(v0, v2, v3)
  }

  const vd = new VertexData()
  vd.positions = positions
  vd.uvs = uvs
  vd.indices = indices
  const normals: number[] = []
  VertexData.ComputeNormals(positions, indices, normals)
  vd.normals = normals

  const mesh = new Mesh('curved-belt', scene)
  vd.applyToMesh(mesh)
  mesh.material = material

  // No internal rotation — vertex positions are already in world
  // coords. Wrap in a TransformNode for parenting consistency with
  // buildBelt.
  const root = new TransformNode('curved-belt-root', scene)
  mesh.parent = root

  return { root, meshes: [mesh], pathLength: pathLen }
}

/** Create the shared belt PBR material. Call once per scene; this
 *  paints the chevron texture and registers the global animation
 *  observer. The returned material is reused by every belt. */
export function createBeltMaterial(scene: Scene): PBRMaterial {
  const tex = new DynamicTexture('belt-chevron', { width: 256, height: 64 }, scene, false)
  // Explicit WRAP mode so UVs > 1 tile the chevrons across the
  // perimeter rather than clamping to the texture's right edge.
  tex.wrapU = Texture.WRAP_ADDRESSMODE
  tex.wrapV = Texture.WRAP_ADDRESSMODE
  const ctx = tex.getContext()
  ctx.fillStyle = '#050506'
  ctx.fillRect(0, 0, 256, 64)
  ctx.strokeStyle = '#7cf7ec'
  ctx.lineWidth = 18
  // One large chevron per texture, centered. The texture's 256px width
  // maps to one chevron-spacing in world units, so going from 4
  // chevrons-per-texture to 1 puts chevrons 4x farther apart in U
  // space — combined with the 3x bump in CHEVRON_SPACING_WORLD this
  // gives the "3x apart, 2x bigger" the user asked for.
  ctx.beginPath()
  ctx.moveTo(64, 6)
  ctx.lineTo(192, 32)
  ctx.lineTo(64, 58)
  ctx.stroke()
  tex.update()

  const mat = new PBRMaterial('belt-surface', scene)
  mat.albedoColor = Color3.FromHexString('#08080a')
  mat.metallic = 0.4
  mat.roughness = 0.6
  mat.emissiveTexture = tex
  mat.emissiveColor = new Color3(1, 1, 1)
  mat.emissiveIntensity = 1.0
  mat.backFaceCulling = false

  // Babylon adds uOffset when sampling, so decrementing makes the
  // chevrons appear to slide in their pointing direction (the
  // direction the path was traced — top forward, front cap down,
  // back cap up).
  scene.onBeforeRenderObservable.add(() => {
    const dt = scene.getEngine().getDeltaTime() / 1000
    tex.uOffset = (tex.uOffset - dt * BELT_SCROLL_SPEED) % 1
  })

  return mat
}
