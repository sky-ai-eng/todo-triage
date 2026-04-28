// Debug scene for the 3D rewrite (SKY-196 / SKY-197).
//
// This file is now thin: it mounts a canvas, hands it to IsoScene, and
// wires up the React-side HUD via Babylon's camera observable. Babylon's
// ArcRotateCamera handles all gestures (LMB-drag = orbit, RMB-drag
// or ctrl+LMB = pan, wheel = zoom) — no custom pointer handlers.

import { IsoScene } from './iso-renderer'
import type { Station } from './iso-station'

const FLOOR_SIZE = 1200
const FLOOR_CELL = 120
const INITIAL_ZOOM_RADIUS = FLOOR_SIZE / 2

// Stage-1 test scene: a single station at the center of the floor with
// a few queued + WIP chips.
const TEST_STATION: Station = {
  x: 400,
  y: 480,
  z: 0,
  w: 400,
  d: 240,
  h: 64,
  queuedCount: 4,
  wipCount: 2,
}

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
  renderer.addStation(TEST_STATION)

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
