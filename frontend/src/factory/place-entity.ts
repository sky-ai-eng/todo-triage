// Snapshot-authoritative entity placement.
//
// Single projection function used by both station tray counts AND
// chip positions on the floor: given an entity from the snapshot,
// the set of station-mapped event_types, a per-pair animation
// duration source, and the current wall clock, returns whether the
// entity is parked at a station or in transit between two stations
// (with progress in [0,1]).
//
// Tray counts and chip placement read this same function so the two
// can never drift. Reload-stable rendering falls out for free —
// recent_events + last_event_at on the snapshot encode exactly enough
// state to resume mid-animation after a refresh, with no client-side
// cache.

import type { FactoryEntity } from '../types'

export type EntityPlacement =
  | { kind: 'parked'; station: string }
  | { kind: 'transit'; from: string; to: string; progress: number }

/** Project an entity to its current factory position.
 *
 *  The most recent station-mapped event in `recent_events` (or
 *  `current_event_type` as a fallback) determines the destination.
 *  If a *different* station-mapped event appears earlier in the tail
 *  AND `last_event_at` is within the configured animation window,
 *  the entity is in transit; otherwise it's parked at the destination.
 *
 *  Returns null when the entity has no station-mapped event at all
 *  (e.g., a Jira ticket on a GitHub-only floor) so the caller can
 *  filter it out cleanly. */
export function placeEntity(
  entity: FactoryEntity,
  knownStations: Set<string>,
  /** ms duration for the from→to chip ride. Backed by the routing
   *  table (sum of segment lengths / belt speed) so chip travel time
   *  matches the visual length of the bridges. Returns a fallback
   *  for unknown pairs — those animate the default duration and then
   *  snap to parked. */
  getDuration: (from: string, to: string) => number,
  now: number,
): EntityPlacement | null {
  const recent = entity.recent_events ?? []

  // Walk the tail for the most recent station-mapped event.
  let toIdx = -1
  for (let i = recent.length - 1; i >= 0; i--) {
    if (knownStations.has(recent[i].event_type)) {
      toIdx = i
      break
    }
  }

  let to: string | undefined
  let toAtMs: number | undefined
  if (toIdx >= 0) {
    to = recent[toIdx].event_type
    const t = Date.parse(recent[toIdx].at)
    if (!Number.isNaN(t)) toAtMs = t
  } else if (entity.current_event_type && knownStations.has(entity.current_event_type)) {
    to = entity.current_event_type
    if (entity.last_event_at) {
      const t = Date.parse(entity.last_event_at)
      if (!Number.isNaN(t)) toAtMs = t
    }
  } else {
    return null
  }

  // Search earlier in the tail for a different station-mapped event —
  // that's the source of an in-flight transit. Self-loops (same
  // station as `to`) are skipped: there's nothing to traverse.
  let from: string | undefined
  for (let i = toIdx - 1; i >= 0; i--) {
    const ev = recent[i].event_type
    if (knownStations.has(ev) && ev !== to) {
      from = ev
      break
    }
  }

  if (from && toAtMs != null) {
    const age = now - toAtMs
    // Guard against client clock skew producing a future timestamp.
    // Negative age would otherwise yield negative progress and pose
    // chips outside their itinerary.
    if (age >= 0) {
      const dur = getDuration(from, to)
      if (dur > 0 && age < dur) {
        return { kind: 'transit', from, to, progress: age / dur }
      }
    }
  }
  return { kind: 'parked', station: to }
}
