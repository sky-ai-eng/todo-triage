// ItemSimulator — drives factory items along precomputed itineraries.
//
// State per item is tiny: which itinerary it's riding, which index in
// that itinerary it's currently on, how far along the current segment,
// how fast, and the meshes representing it. Per frame, the simulator
// advances `progress` by `speed * dt`; on overflow it advances the
// itinerary index. Reaching the end of the itinerary disposes the
// item and (if registered) fires its onArrive callback so the spawn
// pipeline can update tray counts.
//
// Itineraries are precomputed by the routing layer (iso-routing.ts) at
// scene construction. The simulator no longer makes routing decisions;
// it just rides whatever sequence of segments it was handed. This is
// what lets a chip travel from PR_OPENED specifically to NEW_COMMITS
// without getting deflected at every fork in the path graph.
//
// Spawners (the demo-only periodic emitter) precompute an itinerary by
// walking next[0] from their seed segment until a dead end, mirroring
// the old "always pick next[0]" behavior so iso-debug.ts's spawner
// keeps producing the visual it always did.
//
// Rendering is one shell + core mesh per item, plus an optional
// billboard label mesh on the top face. We'll instance these with
// thinInstances when the item count grows; for now each item is two
// CreateCylinder calls — fine for the dozen-item demo scale.

import {
  Color3,
  DynamicTexture,
  type Mesh,
  MeshBuilder,
  PBRMaterial,
  type Scene,
  StandardMaterial,
  Texture,
  TransformNode,
  Vector3,
} from '@babylonjs/core'

import { BELT_WORLD_SPEED } from './iso-belt'
import type { PathSegment } from './iso-path'

const ITEM_DIAM = 32
const ITEM_HEIGHT = 14
// Small clearance so the item visibly hovers above the belt rather
// than z-fighting against the chevron texture's top surface.
const ITEM_LIFT = 0.5
const ITEM_CORE_DIAM_FRAC = 0.5
const ITEM_CORE_HEIGHT_FRAC = 0.6

// Items default to riding at the belt's surface speed, so a chip and
// the chevrons under it move at the same rate. Override per-spawn if
// we ever introduce slow conveyors or sprint sections.
const DEFAULT_SPEED = BELT_WORLD_SPEED

// Per-hue core glow tuning. Status is conveyed by which station an
// item is moving to/from (the factory layout IS the status), so the
// chip's color channel is free to encode something stable per entity
// — its repo (GitHub) or project (Jira). Same hue every time the
// user sees a chip from `triage-factory` or `SKY`, no config UI
// required.
const CORE_HUE_SATURATION = 0.78
const CORE_HUE_VALUE = 1.0
const CORE_EMISSIVE_INTENSITY = 1.4

// Label plate sits on top of the chip's shell, slightly recessed so
// it doesn't z-fight. A square texture keeps text legible from any
// rotation; the chip parent's heading is symmetric so we don't need
// to billboard.
const LABEL_PLATE_FRAC = 0.78
const LABEL_PLATE_LIFT = 0.4
const LABEL_TEX_PX = 128

/** Deterministic string → hue ([0, 360)). djb2-derived; the exact
 *  bit-mixing isn't important, just that the same input always gives
 *  the same output and small input changes give large hue jumps. */
export function hashHue(s: string): number {
  let h = 5381
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) + h + s.charCodeAt(i)) >>> 0
  }
  return h % 360
}

interface FactoryItem {
  id: string
  itinerary: PathSegment[]
  index: number
  /** Distance traveled along current segment, in world units. */
  progress: number
  /** Forward speed in world-units per second. */
  speed: number
  /** Parent transform — owns position and heading. The shell + core
   *  are children with a fixed `rotation.x = π/2` to stand them up
   *  in the parent's local frame. This split is needed because
   *  Babylon's `rotation` property applies as Ry*Rx*Rz, which would
   *  apply heading-around-Z BEFORE stand-up-around-X if both lived
   *  on the same node — at non-trivial headings the cylinder ends
   *  up horizontal instead of upright. With the parent owning Z and
   *  the child owning X, each rotation operates in the right local
   *  frame and the cylinder always stands upright. */
  root: TransformNode
  shell: Mesh
  core: Mesh
  labelMesh: Mesh | null
  onArrive: (() => void) | undefined
  arriveFired: boolean
}

interface Spawner {
  itinerary: PathSegment[]
  interval: number
  /** Time since last spawn (or since registration). When this exceeds
   *  `interval` we spawn one item and subtract `interval`. */
  accumulator: number
  speed: number
  /** Round-robin source for namespace-derived hues; demo-only. */
  namespaces?: string[]
  namespaceCursor: number
}

export interface SpawnerOptions {
  speed?: number
  /** Round-robin through these namespaces — one chip per spawn, then
   *  advance the cursor. Demo-only; production spawns call spawnItem
   *  directly with an itinerary and a per-entity hue. */
  namespaces?: string[]
}

export interface SpawnOptions {
  speed?: number
  /** Repo (GitHub) or project (Jira) the entity belongs to. Hashed to
   *  a hue for the chip's core. Ignored if `hue` is also set. */
  namespace?: string
  /** Explicit hue [0, 360) for the chip's core. The spawn pipeline
   *  computes hue from repo/project upstream so callers can color a
   *  chip without going through the namespace string. */
  hue?: number
  /** 1–8 char label drawn on the chip's top face (PR number, Jira
   *  key). Empty/undefined → no label plate. */
  label?: string
  /** Fired when the chip reaches the end of its itinerary, just
   *  before disposal. The animation pipeline uses this to bump the
   *  destination station's queued count or schedule a refetch. */
  onArrive?: () => void
}

export class ItemSimulator {
  private items: FactoryItem[] = []
  private spawners: Spawner[] = []
  private nextId = 0
  private observer: ReturnType<Scene['onBeforeRenderObservable']['add']>
  private scene: Scene
  private shellMat: PBRMaterial
  private fallbackCoreMat: PBRMaterial
  /** One core material per integer hue, lazily created and reused. */
  private coreMaterials: Map<number, PBRMaterial> = new Map()
  /** One label material per text string, lazily created and reused.
   *  The factory rarely sees more than ~50 distinct entity labels in
   *  view at once, so the cache stays bounded in practice. */
  private labelMaterials: Map<string, StandardMaterial> = new Map()

  constructor(scene: Scene, shellMat: PBRMaterial, coreMat: PBRMaterial) {
    this.scene = scene
    this.shellMat = shellMat
    this.fallbackCoreMat = coreMat
    this.observer = scene.onBeforeRenderObservable.add(() => this.tick())
  }

  /** Register a periodic spawner on a path segment. The first item
   *  appears one interval after this call. Items ride the next[0]
   *  chain from `segment` until a dead end — mirrors the original
   *  spawner behavior so the demo scene's visual is preserved. */
  startSpawner(segment: PathSegment, intervalSeconds: number, options: SpawnerOptions = {}): void {
    this.spawners.push({
      itinerary: walkNext0Chain(segment),
      interval: intervalSeconds,
      accumulator: 0,
      speed: options.speed ?? DEFAULT_SPEED,
      namespaces: options.namespaces,
      namespaceCursor: 0,
    })
  }

  /** Spawn a single item that rides the given itinerary from its
   *  start to its end. The first segment is the source; the last is
   *  the destination's arrival segment. Items dispose at end-of-
   *  itinerary; if onArrive is set it fires just before disposal. */
  spawnItem(itinerary: PathSegment[], options: SpawnOptions = {}): void {
    if (itinerary.length === 0) return
    const id = `item-${this.nextId++}`
    const root = new TransformNode(`${id}-root`, this.scene)
    const shell = MeshBuilder.CreateCylinder(
      `${id}-shell`,
      { diameter: ITEM_DIAM, height: ITEM_HEIGHT, tessellation: 28 },
      this.scene,
    )
    shell.rotation.x = Math.PI / 2
    shell.material = this.shellMat
    shell.parent = root
    const core = MeshBuilder.CreateCylinder(
      `${id}-core`,
      {
        diameter: ITEM_DIAM * ITEM_CORE_DIAM_FRAC,
        height: ITEM_HEIGHT * ITEM_CORE_HEIGHT_FRAC,
        tessellation: 20,
      },
      this.scene,
    )
    core.rotation.x = Math.PI / 2
    core.material = this.getCoreMaterial(options)
    core.parent = root

    const labelMesh = options.label ? this.buildLabelMesh(id, options.label, root) : null

    const item: FactoryItem = {
      id,
      itinerary,
      index: 0,
      progress: 0,
      speed: options.speed ?? DEFAULT_SPEED,
      root,
      shell,
      core,
      labelMesh,
      onArrive: options.onArrive,
      arriveFired: false,
    }
    this.items.push(item)
    this.updatePose(item)
  }

  private getCoreMaterial(options: SpawnOptions): PBRMaterial {
    let hue: number | undefined
    if (typeof options.hue === 'number') {
      hue = ((options.hue % 360) + 360) % 360
    } else if (options.namespace) {
      hue = hashHue(options.namespace)
    }
    if (hue == null) return this.fallbackCoreMat
    const key = Math.round(hue)
    const cached = this.coreMaterials.get(key)
    if (cached) return cached
    const m = new PBRMaterial(`item-core-${key}`, this.scene)
    m.albedoColor = Color3.Black()
    m.emissiveColor = Color3.FromHSV(key, CORE_HUE_SATURATION, CORE_HUE_VALUE)
    m.emissiveIntensity = CORE_EMISSIVE_INTENSITY
    m.metallic = 0
    m.roughness = 1
    this.coreMaterials.set(key, m)
    return m
  }

  /** Build a small plate above the chip's top face that shows the
   *  given label. The plate is parented to the item's root so it
   *  rides along with the chip; its material is cached by label
   *  string so repeated PR numbers reuse the same canvas texture. */
  private buildLabelMesh(itemId: string, label: string, parent: TransformNode): Mesh {
    const plate = MeshBuilder.CreatePlane(
      `${itemId}-label`,
      { size: ITEM_DIAM * LABEL_PLATE_FRAC },
      this.scene,
    )
    // Plane spawns vertical (xy plane); rotate to lie flat on the
    // chip's top. Lift slightly so it doesn't z-fight with the
    // shell's top cap.
    plate.position.set(0, 0, ITEM_HEIGHT / 2 + LABEL_PLATE_LIFT)
    plate.rotation.x = 0 // already in xy plane after parent stand-up
    plate.material = this.getLabelMaterial(label)
    plate.parent = parent
    return plate
  }

  private getLabelMaterial(label: string): StandardMaterial {
    const cached = this.labelMaterials.get(label)
    if (cached) return cached
    const tex = new DynamicTexture(
      `chip-label-${label}`,
      { width: LABEL_TEX_PX, height: LABEL_TEX_PX },
      this.scene,
      false,
    )
    tex.hasAlpha = true
    const ctx = tex.getContext() as CanvasRenderingContext2D
    ctx.clearRect(0, 0, LABEL_TEX_PX, LABEL_TEX_PX)
    ctx.fillStyle = '#ffffff'
    // Bold sans label, sized to fit ~6 chars at this resolution.
    const fontPx = label.length <= 4 ? 56 : label.length <= 6 ? 42 : 32
    ctx.font = `700 ${fontPx}px ui-sans-serif, system-ui, -apple-system, sans-serif`
    ctx.textAlign = 'center'
    ctx.textBaseline = 'middle'
    // V-flip — Babylon's right-handed UV layout flips V on +z faces.
    ctx.save()
    ctx.translate(0, LABEL_TEX_PX)
    ctx.scale(1, -1)
    ctx.fillText(label, LABEL_TEX_PX / 2, LABEL_TEX_PX / 2)
    ctx.restore()
    tex.update()

    const m = new StandardMaterial(`chip-label-mat-${label}`, this.scene)
    m.diffuseTexture = tex
    m.diffuseTexture.hasAlpha = true
    m.useAlphaFromDiffuseTexture = true
    m.emissiveTexture = tex
    m.emissiveColor = new Color3(0.95, 0.95, 0.95)
    m.disableLighting = true
    m.backFaceCulling = false
    ;(m.diffuseTexture as Texture).wrapU = Texture.CLAMP_ADDRESSMODE
    ;(m.diffuseTexture as Texture).wrapV = Texture.CLAMP_ADDRESSMODE
    this.labelMaterials.set(label, m)
    return m
  }

  private tick(): void {
    const dt = this.scene.getEngine().getDeltaTime() / 1000

    // Advance spawners. The `while` loop catches catch-up scenarios
    // (e.g., a backgrounded tab returning with dt > interval).
    for (const sp of this.spawners) {
      sp.accumulator += dt
      while (sp.accumulator >= sp.interval) {
        sp.accumulator -= sp.interval
        let namespace: string | undefined
        if (sp.namespaces && sp.namespaces.length > 0) {
          namespace = sp.namespaces[sp.namespaceCursor]
          sp.namespaceCursor = (sp.namespaceCursor + 1) % sp.namespaces.length
        }
        this.spawnItem(sp.itinerary, { speed: sp.speed, namespace })
      }
    }

    // Advance items. Items that reach the end of their itinerary
    // fire onArrive (once) and have their meshes disposed.
    const survivors: FactoryItem[] = []
    for (const item of this.items) {
      item.progress += item.speed * dt
      let alive = true
      while (item.progress > item.itinerary[item.index].length) {
        const nextIndex = item.index + 1
        if (nextIndex >= item.itinerary.length) {
          if (!item.arriveFired) {
            item.arriveFired = true
            item.onArrive?.()
          }
          this.disposeItem(item)
          alive = false
          break
        }
        item.progress -= item.itinerary[item.index].length
        item.index = nextIndex
      }
      if (alive) {
        this.updatePose(item)
        survivors.push(item)
      }
    }
    this.items = survivors
  }

  private updatePose(item: FactoryItem): void {
    const seg = item.itinerary[item.index]
    const { position, tangent } = seg.sample(item.progress)
    // Path z = belt-top surface. Lift by half the item's height plus
    // ITEM_LIFT so the item's visual base sits just above the belt.
    const z = position.z + ITEM_HEIGHT / 2 + ITEM_LIFT
    item.root.position.set(position.x, position.y, z)
    // Heading around world Z, applied to the parent. Children handle
    // the stand-up-around-X separately, so the order is correct.
    item.root.rotation.z = Math.atan2(tangent.y, tangent.x)
  }

  private disposeItem(item: FactoryItem): void {
    item.shell.dispose()
    item.core.dispose()
    item.labelMesh?.dispose()
    item.root.dispose()
  }

  destroy(): void {
    if (this.observer) {
      this.scene.onBeforeRenderObservable.remove(this.observer)
    }
    for (const item of this.items) {
      this.disposeItem(item)
    }
    for (const m of this.coreMaterials.values()) {
      m.dispose()
    }
    this.coreMaterials.clear()
    for (const m of this.labelMaterials.values()) {
      m.dispose()
    }
    this.labelMaterials.clear()
    this.items = []
    this.spawners = []
  }
}

/** Walk segment.next[0] from `start` until a dead end or a cycle is
 *  detected. Used by the demo periodic spawner to precompute its
 *  itinerary at registration time, so chip behavior matches the
 *  pre-rework "always pick next[0]" path exactly. Avoids the (now
 *  removed) per-frame next[0] decision. */
function walkNext0Chain(start: PathSegment): PathSegment[] {
  const chain: PathSegment[] = [start]
  const visited = new Set<PathSegment>([start])
  let cur = start
  while (cur.next.length > 0) {
    const next = cur.next[0]
    if (visited.has(next)) break
    chain.push(next)
    visited.add(next)
    cur = next
  }
  return chain
}

// Suppress unused Vector3 import warning when label/billboard impl
// changes. Currently used only via the typed plate position math.
void Vector3
