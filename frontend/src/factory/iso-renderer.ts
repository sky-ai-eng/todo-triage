// 3D rendering layer for the factory scene, built on Babylon.js.
//
// One Babylon `ArcRotateCamera` is the single source of truth for the
// view: alpha (yaw), beta (pitch), radius (zoom level), target (focal
// point). Babylon's default inputs handle gestures — left-mouse drag
// orbits, right-mouse (or ctrl+click) drags pan, wheel zooms.
//
// We're orthographic-only for now. `radius` doesn't affect projection
// in ortho mode, but Babylon still uses it as the wheel-input target —
// so we mirror it into `orthoLeft/Right/Top/Bottom` via a one-line
// observer on every camera-state change.
//
// What this file owns:
//   - The Babylon Engine, Scene, ArcRotateCamera, lights.
//   - The floor grid (LineSystem) and station meshes.
//   - The render loop.
//
// What this file does NOT own:
//   - Gestures. Babylon's default ArcRotateCamera inputs handle them.
//   - Camera-state mirroring or projection helpers. The camera *is*
//     the state. For projecting world points to screen (Stage 6 HTML
//     overlays), use Babylon's `Vector3.Project` directly with the
//     scene's view+projection matrices.

import {
  ArcRotateCamera,
  Camera,
  Color3,
  Color4,
  DirectionalLight,
  Engine,
  HemisphericLight,
  LinesMesh,
  MeshBuilder,
  Scene,
  TransformNode,
  Vector3,
} from '@babylonjs/core'

import {
  buildStationMesh,
  createStationMaterials,
  type Station,
  type StationMaterials,
} from './iso-station'

const DEFAULT_FLOOR_SIZE = 1200
// Closest the camera can get before its near plane starts clipping
// objects in the scene. Expressed as a max zoom multiplier so the
// physical limit derives from the initial view (radius_min =
// initial_radius / max_zoom).
const MAX_ZOOM = 2.25

export class IsoScene {
  readonly engine: Engine
  readonly scene: Scene
  readonly camera: ArcRotateCamera
  private materials: StationMaterials | null = null
  private gridMesh: LinesMesh | null = null

  constructor(canvas: HTMLCanvasElement) {
    this.engine = new Engine(canvas, true, {
      adaptToDeviceRatio: true,
      stencil: false,
    })

    this.scene = new Scene(this.engine)
    // Warm off-white background — matches the previous renderer's tone
    // and reads well against the warm body color.
    this.scene.clearColor = new Color4(0.969, 0.961, 0.949, 1)
    // Right-handed coordinates so our world (+x right, +y forward,
    // +z up) is what Babylon expects for cross products and culling.
    this.scene.useRightHandedSystem = true

    // ArcRotateCamera: an orbit camera. With `upVector = (0, 0, 1)`
    // it orbits around `target` on the xy plane, with `alpha` as
    // azimuth and `beta` as polar angle from +z (so beta=0 is
    // top-down looking down). `radius` would be camera distance in
    // perspective; in ortho we use it as the zoom control.
    const initialTarget = new Vector3(DEFAULT_FLOOR_SIZE / 2, DEFAULT_FLOOR_SIZE / 2, 0)
    this.camera = new ArcRotateCamera(
      'iso-camera',
      -Math.PI / 2, // alpha — initial azimuth
      0.001, // beta — tiny offset from top-down to avoid lookAt degeneracy
      DEFAULT_FLOOR_SIZE / 2, // radius — initial half-height of visible ortho bounds
      initialTarget,
      this.scene,
    )
    this.camera.upVector = new Vector3(0, 0, 1)
    this.camera.minZ = 0.1
    this.camera.maxZ = 100000
    this.camera.mode = Camera.ORTHOGRAPHIC_CAMERA

    // Mirror radius → orthoBounds. Whenever the camera moves (rotate,
    // pan, wheel), this fires and keeps the ortho frustum in sync.
    const updateOrthoBounds = () => {
      const aspect = this.engine.getRenderWidth() / this.engine.getRenderHeight()
      const halfH = this.camera.radius
      this.camera.orthoTop = halfH
      this.camera.orthoBottom = -halfH
      this.camera.orthoLeft = -halfH * aspect
      this.camera.orthoRight = halfH * aspect
    }
    this.camera.onViewMatrixChangedObservable.add(updateOrthoBounds)
    this.engine.onResizeObservable.add(updateOrthoBounds)
    updateOrthoBounds()

    // Attach Babylon's default inputs. The ArcRotateCamera's pointer
    // input handles LMB-drag = orbit, RMB-drag (or ctrl+LMB) = pan,
    // wheel = zoom. attachControl(true) means "don't preventDefault
    // on canvas events" so the surrounding page still receives them
    // when appropriate (we suppress only what we explicitly need to).
    this.camera.attachControl(true)

    // Sensitivity tuning for our world's scale (~1200 units across):
    // smaller `*Sensibility` = more pan/rotation per pixel. Babylon's
    // defaults are tuned for tiny default scenes; ours is bigger.
    this.camera.panningSensibility = 100
    this.camera.angularSensibilityX = 1000
    this.camera.angularSensibilityY = 1000
    this.camera.wheelPrecision = 0.5

    // Constrain orbit so users can't flip below the floor or zoom
    // past usable extremes.
    this.camera.lowerBetaLimit = 0.001
    this.camera.upperBetaLimit = Math.PI / 2 - 0.01
    this.camera.lowerRadiusLimit = DEFAULT_FLOOR_SIZE / 2 / MAX_ZOOM
    this.camera.upperRadiusLimit = 5000

    this.setupLighting()

    this.engine.runRenderLoop(() => {
      this.scene.render()
    })
  }

  resize(): void {
    this.engine.resize()
  }

  resetView(): void {
    this.camera.alpha = -Math.PI / 2
    this.camera.beta = 0.001
    this.camera.radius = DEFAULT_FLOOR_SIZE / 2
    this.camera.target = new Vector3(DEFAULT_FLOOR_SIZE / 2, DEFAULT_FLOOR_SIZE / 2, 0)
  }

  buildFloor(size: number, cell: number): void {
    if (this.gridMesh) {
      this.gridMesh.dispose()
      this.gridMesh = null
    }
    const lines: Vector3[][] = []
    for (let i = 0; i <= size; i += cell) {
      lines.push([new Vector3(0, i, 0), new Vector3(size, i, 0)])
      lines.push([new Vector3(i, 0, 0), new Vector3(i, size, 0)])
    }
    const grid = MeshBuilder.CreateLineSystem('iso-grid', { lines }, this.scene)
    grid.color = new Color3(0.1, 0.1, 0.1)
    grid.alpha = 0.12
    grid.alwaysSelectAsActiveMesh = true
    this.gridMesh = grid
  }

  addStation(spec: Station): TransformNode {
    if (!this.materials) {
      this.materials = createStationMaterials(this.scene)
    }
    return buildStationMesh(this.scene, spec, this.materials)
  }

  destroy(): void {
    this.gridMesh?.dispose()
    if (this.materials) {
      this.materials.body.dispose()
      this.materials.chamberFloor.dispose()
      this.materials.pad.dispose()
      this.materials.queuedChip.dispose()
      this.materials.wipChip.dispose()
      this.materials = null
    }
    this.scene.dispose()
    this.engine.dispose()
  }

  private setupLighting(): void {
    const hemi = new HemisphericLight('hemi', new Vector3(0, 0, 1), this.scene)
    hemi.diffuse = Color3.FromHexString('#fff8ee')
    hemi.groundColor = Color3.FromHexString('#a89e8e')
    hemi.intensity = 0.85

    const key = new DirectionalLight('key', new Vector3(-0.3, -0.5, -1), this.scene)
    key.diffuse = Color3.FromHexString('#ffffff')
    key.intensity = 0.45
  }
}
