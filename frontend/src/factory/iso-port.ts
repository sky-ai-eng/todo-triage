// Port abstraction for factory entities. A port is a snap point where
// a conveyor terminates against an entity's wall. Ports live in
// entity-local coords; world position resolves at render time from
// the entity's placement + the port's local offset.
//
// Visual contract: each port produces a recessed opening in the entity
// wall, an LED frame around the opening, and a short internal "belt
// stub" so a connecting conveyor reads as continuing into the entity
// for a short distance. The conveyor mesh itself terminates at the
// wall plane — the stuff inside the recess is owned by the entity.

import { Vector3 } from '@babylonjs/core'

import type { PathSegment } from './iso-path'

export type PortDirection = 'north' | 'south' | 'east' | 'west'
export type PortKind = 'input' | 'output'

export interface Port {
  kind: PortKind
  /** Outward face of the entity. North = +y, south = -y, east = +x,
   *  west = -x. */
  direction: PortDirection
  /** Position along the face, normalized 0..1. For east/west ports,
   *  0 = low y, 1 = high y. For north/south, 0 = low x, 1 = high x. */
  offset: number
  /** How far the recess extends into the entity along the inward axis. */
  recessDepth: number
}

/** Live handle on a port after the entity's mesh is built. The snap
 *  point is where a conveyor's endpoint should sit. */
export interface PortHandle {
  port: Port
  /** Center of the conveyor cross-section on the wall plane, in world
   *  coords. Conveyors terminate exactly here. */
  worldPos: Vector3
  /** Unit vector pointing outward through the port. */
  outward: Vector3
  /** Internal path segment associated with this port. For input
   *  ports the segment STARTS at the wall plane (items entering go
   *  inward); for output ports the segment ENDS at the wall plane
   *  (items emerging exit here). For 2-port poles both port handles
   *  share the same internal segment; the simulator only needs to
   *  know which end of the segment a given port refers to via its
   *  kind. Set by the entity builder when wiring permits. */
  segment?: PathSegment
}

// ─── Conveyor cross-section ───────────────────────────────────────────────
// Single global pair. Recesses size to match. If we ever need varied
// conveyor widths, move these onto Port.

export const CONVEYOR_WIDTH = 70
export const CONVEYOR_HEIGHT = 8

// ─── Recess sizing ────────────────────────────────────────────────────────
// Across is just a small visual frame around the belt. Above is
// generous so items riding on the conveyor have room to clear into the
// station — a doorway, not a slot. The conveyor sits flush on the
// floor, so no margin below.

export const PORT_RECESS_MARGIN_ACROSS = 4
export const PORT_RECESS_MARGIN_ABOVE = 16

/** Default recess depth — long enough to read as "the belt continues
 *  well into the entity," short enough that the back stays lit. */
export const DEFAULT_PORT_RECESS_DEPTH = 60
