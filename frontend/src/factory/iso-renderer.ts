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
//   - The floor grid (LineSystem) and ground plane that catches
//     shadows.
//   - The ShadowGenerator on the sun directional light, and the
//     GlowLayer that blooms emissive trims/cores.
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
  GlowLayer,
  HemisphericLight,
  ImageProcessingConfiguration,
  LinesMesh,
  Mesh,
  MeshBuilder,
  PBRMaterial,
  Scene,
  ShadowGenerator,
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
const MAX_ZOOM = 6

export class IsoScene {
  readonly engine: Engine
  readonly scene: Scene
  readonly camera: ArcRotateCamera
  private materials: StationMaterials | null = null
  private gridMesh: LinesMesh | null = null
  private ground: Mesh | null = null
  private groundMat: PBRMaterial | null = null
  private shadowGenerator: ShadowGenerator | null = null
  private glowLayer: GlowLayer | null = null

  constructor(canvas: HTMLCanvasElement) {
    this.engine = new Engine(canvas, true, {
      adaptToDeviceRatio: true,
      stencil: false,
    })

    this.scene = new Scene(this.engine)
    // Warm off-white background. Pulled noticeably below pure white so
    // the negative space doesn't dominate the eye — combined with the
    // ACES tonemap below, this reads as a soft studio cream rather
    // than a blown-out canvas.
    this.scene.clearColor = new Color4(0.91, 0.895, 0.875, 1)
    // Right-handed coordinates so our world (+x right, +y forward,
    // +z up) is what Babylon expects for cross products and culling.
    this.scene.useRightHandedSystem = true

    // Image-processing pipeline. Without this, Babylon writes linear
    // light straight to sRGB and any bright surface clips toward pure
    // white — on a cream-on-cream scene that reads as blinding. ACES
    // gives a filmic highlight rolloff (the standard cinematic curve);
    // exposure < 1 stops the "camera" down so mid-tones sit lower;
    // contrast > 1 deepens the shadows that the lower exposure
    // creates so the image still has bite.
    const ip = this.scene.imageProcessingConfiguration
    ip.toneMappingEnabled = true
    ip.toneMappingType = ImageProcessingConfiguration.TONEMAPPING_ACES
    ip.exposure = 0.9
    ip.contrast = 1.1

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
    // Negative minZ: in ortho mode the near plane can sit *behind*
    // the camera (no perspective convergence at view-z=0). Without
    // this, at oblique angles the ground plane gets sliced where it
    // crosses behind the camera in view space, producing a visible
    // horizon seam at any tilt. Pushing the near plane back past
    // anything we'd render behind the camera eliminates the issue.
    this.camera.minZ = -DEFAULT_FLOOR_SIZE * 4
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

    // GlowLayer blooms emissive contributions (LED trim, chip cores)
    // without affecting opaque PBR responses. Created before lights
    // so the ShadowGenerator setup can rely on a fully-initialized
    // scene.
    this.glowLayer = new GlowLayer('glow', this.scene, { blurKernelSize: 32 })
    this.glowLayer.intensity = 0.55

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
    if (this.ground) {
      this.ground.dispose()
      this.ground = null
    }

    // Grid lines, drawn just above the ground plane so they're
    // visible against it.
    const lines: Vector3[][] = []
    for (let i = 0; i <= size; i += cell) {
      lines.push([new Vector3(0, i, 0), new Vector3(size, i, 0)])
      lines.push([new Vector3(i, 0, 0), new Vector3(i, size, 0)])
    }
    const grid = MeshBuilder.CreateLineSystem('iso-grid', { lines }, this.scene)
    grid.color = new Color3(0.1, 0.1, 0.1)
    grid.alpha = 0.1
    grid.alwaysSelectAsActiveMesh = true
    this.gridMesh = grid

    // Ground plane — receives shadows from stations and chips so the
    // scene gets soft contact-shadow grounding. Sized larger than
    // the grid so shadows don't clip at the edge of the visible
    // floor when the camera pans. CreatePlane is xy with normal +z,
    // matching our z-up world without needing rotation.
    if (!this.groundMat) {
      this.groundMat = new PBRMaterial('ground-mat', this.scene)
      this.groundMat.albedoColor = Color3.FromHexString('#e3ddd0')
      this.groundMat.metallic = 0
      this.groundMat.roughness = 0.95
    }
    const ground = MeshBuilder.CreatePlane(
      'ground',
      { width: size * 4, height: size * 4, sideOrientation: Mesh.DOUBLESIDE },
      this.scene,
    )
    ground.position = new Vector3(size / 2, size / 2, -0.5)
    ground.material = this.groundMat
    ground.receiveShadows = true
    this.ground = ground
  }

  addStation(spec: Station): TransformNode {
    if (!this.materials) {
      this.materials = createStationMaterials(this.scene)
    }
    const root = buildStationMesh(this.scene, spec, this.materials)

    // Register sub-meshes with the shadow generator. Opaque body
    // pieces both cast and receive; transparent shells cast only
    // (so chips drop a soft shadow into the chamber); emissive
    // trims/cores and the glass canopy are skipped — their shadows
    // would be either invisible or noisy.
    if (this.shadowGenerator) {
      for (const m of root.getChildMeshes()) {
        if (m.name === 'station-body' || m.name === 'chamber-floor' || m.name === 'landing-pad') {
          m.receiveShadows = true
          this.shadowGenerator.addShadowCaster(m)
        } else if (
          m.name.startsWith('queued-shell-') ||
          m.name.startsWith('wip-shell-') ||
          m.name.startsWith('heatsink-')
        ) {
          this.shadowGenerator.addShadowCaster(m)
        }
      }
    }

    return root
  }

  destroy(): void {
    this.gridMesh?.dispose()
    this.ground?.dispose()
    this.groundMat?.dispose()
    this.glowLayer?.dispose()
    this.shadowGenerator?.dispose()
    if (this.materials) {
      for (const m of Object.values(this.materials)) {
        m.dispose()
      }
      this.materials = null
    }
    this.scene.dispose()
    this.engine.dispose()
  }

  private setupLighting(): void {
    // Hemispheric — sky/ground ambient. Tuned warm so unlit faces
    // pick up a soft cream color, not the neutral grey Babylon's
    // default would give.
    const hemi = new HemisphericLight('hemi', new Vector3(0, 0, 1), this.scene)
    hemi.diffuse = Color3.FromHexString('#fff8ee')
    hemi.groundColor = Color3.FromHexString('#a89e8e')
    hemi.intensity = 0.55

    // Sun — warm key directional with shadows. Position is set above
    // and to the back-right of the scene so shadows fall forward and
    // to the left, away from the camera's default view angle.
    const sun = new DirectionalLight('sun', new Vector3(-0.45, -0.55, -1).normalize(), this.scene)
    sun.position = new Vector3(800, 600, 1500)
    sun.diffuse = Color3.FromHexString('#fff5e1')
    sun.intensity = 1.4

    // Soft contact-style shadows. Blur exponential shadow maps
    // tolerate transparent casters more gracefully than PCF and
    // give that "studio hotel lobby" softness for free.
    this.shadowGenerator = new ShadowGenerator(2048, sun)
    this.shadowGenerator.useBlurExponentialShadowMap = true
    this.shadowGenerator.blurKernel = 32
    this.shadowGenerator.darkness = 0.3

    // Cool blue rim — a subtle counter-light from the opposite side
    // that picks out edges and ties into the cyan/blue accent
    // palette. No shadows on this one; it's pure form definition.
    const rim = new DirectionalLight('rim', new Vector3(0.6, 0.5, -0.6).normalize(), this.scene)
    rim.diffuse = Color3.FromHexString('#9fc4ff')
    rim.intensity = 0.25
  }
}
