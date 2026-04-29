// PathSegment — the parameterized path that an item rides along inside
// the factory simulation. Every belt, pole interior, and port stub
// produces exactly one segment; segments chain via the mutable `next`
// list so a junction is just `predecessor.next = [successor]`.
//
// Geometry is a polyline in world coordinates with z lifted to the
// belt's top surface (so a sampled position can be used directly as
// the item's bottom-of-base position with a small lift). For straight
// belts this is a 2-point polyline; for the turn pole's quarter-arc
// it's the same tessellated polyline used to build the curved-belt
// mesh, so item motion follows the visible belt geometry exactly.
//
// Sampling cost is O(log n) in segment count via the cumulative-length
// table (linear search is fine at our tessellation — 16-18 segments
// per turn, 2 per straight). The simulator calls sample() once per
// item per frame.

import { Vector3 } from '@babylonjs/core'

export interface SampleResult {
  /** World-space position on the path. */
  position: Vector3
  /** Unit tangent in the +s direction (path forward). */
  tangent: Vector3
}

export class PathSegment {
  /** Polyline waypoints in world coordinates (z = belt top). */
  readonly points: Vector3[]
  /** Cumulative arc length at each waypoint. cumLengths[0] = 0. */
  readonly cumLengths: number[]
  /** Total path length (= cumLengths[last]). */
  readonly length: number
  /** Successor segments. Empty = dead end (items despawn). For
   *  splitters we'll add a routing strategy later; for now items
   *  always take next[0]. */
  next: PathSegment[] = []
  /** Human-readable label for debugging (e.g. "west-belt-1",
   *  "turn-pole-arc", "station-W-stub"). */
  readonly label: string

  constructor(points: Vector3[], label: string = 'segment') {
    if (points.length < 2) {
      throw new Error('PathSegment requires at least 2 waypoints')
    }
    this.points = points.map((p) => p.clone())
    this.cumLengths = [0]
    for (let i = 1; i < this.points.length; i++) {
      const dx = this.points[i].x - this.points[i - 1].x
      const dy = this.points[i].y - this.points[i - 1].y
      const dz = this.points[i].z - this.points[i - 1].z
      this.cumLengths.push(this.cumLengths[i - 1] + Math.sqrt(dx * dx + dy * dy + dz * dz))
    }
    this.length = this.cumLengths[this.cumLengths.length - 1]
    this.label = label
  }

  /** Sample the path at distance `s` from the start. `s` is clamped
   *  to [0, length]; the simulator passes pre-bounded values but
   *  clamping protects against floating-point drift at endpoints. */
  sample(s: number): SampleResult {
    const clamped = s <= 0 ? 0 : s >= this.length ? this.length : s

    if (clamped <= 0) {
      return {
        position: this.points[0].clone(),
        tangent: this.points[1].subtract(this.points[0]).normalize(),
      }
    }
    if (clamped >= this.length) {
      const last = this.points.length - 1
      return {
        position: this.points[last].clone(),
        tangent: this.points[last].subtract(this.points[last - 1]).normalize(),
      }
    }

    // Linear scan of cumLengths — short list, and the simulator's
    // monotonic progress means a future cached-index optimization is
    // trivial if it ever shows up in a profile.
    let i = 1
    while (i < this.cumLengths.length && this.cumLengths[i] < clamped) i++
    const a = this.points[i - 1]
    const b = this.points[i]
    const segLen = this.cumLengths[i] - this.cumLengths[i - 1]
    const t = (clamped - this.cumLengths[i - 1]) / segLen
    return {
      position: Vector3.Lerp(a, b, t),
      tangent: b.subtract(a).normalize(),
    }
  }
}
