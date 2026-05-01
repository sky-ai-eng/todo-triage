// Client-side cache of entity_id → current station event_type. Two
// purposes:
//
// 1. WS-event chip animation. When an `event` WS message arrives for
//    entity E with new event_type T, the spawn pipeline needs E's
//    *prior* event_type to know where to animate the chip from. The
//    WS payload only carries the new event_type, so we cache the
//    last-seen-at for each entity client-side.
//
// 2. Surviving page reload. The WS connection is fast but races the
//    initial snapshot fetch. If a transition WS message lands before
//    the snapshot does, we'd have to teleport (no prior). Persisting
//    to localStorage lets us recover prior locations immediately on
//    reload, so only truly-new entities teleport.
//
// The cache is authoritative on prior locations only — what's
// currently on a station's tray comes from the snapshot. Drift
// between cache and snapshot is corrected silently on each snapshot
// fetch (seedFromSnapshot overwrites any cached entry).
//
// We do not currently evict closed/merged entities. With ~500-entity
// snapshot limits and ~80 bytes per entry, the cache stays well
// under localStorage's per-origin quota even after months of use.

import type { FactoryEntity, FactorySnapshot } from '../types'

const STORAGE_KEY = 'factory:entity-cache:v1'
const PERSIST_DEBOUNCE_MS = 500

export class EntityLocationCache {
  private map: Map<string, string> = new Map()
  private persistTimer: ReturnType<typeof setTimeout> | null = null

  constructor() {
    this.loadFromStorage()
  }

  /** Seed the cache from a snapshot. Entities present overwrite any
   *  cached value; absent entities stay (snapshot is limit-bounded
   *  and dropping their cache entry would lose recoverable state). */
  seedFromSnapshot(snapshot: FactorySnapshot, knownStationIds: Set<string>): void {
    for (const e of snapshot.entities) {
      const at = projectEntityLocation(e, knownStationIds)
      if (at) this.map.set(e.id, at)
    }
    this.schedulePersist()
  }

  /** Record an entity transition. Returns the prior event_type if
   *  known. Updates the cache to the new event_type so the next
   *  transition has a valid prior. */
  recordTransition(entityId: string, newEventType: string): { prior: string | undefined } {
    const prior = this.map.get(entityId)
    this.map.set(entityId, newEventType)
    this.schedulePersist()
    return { prior }
  }

  get(entityId: string): string | undefined {
    return this.map.get(entityId)
  }

  private loadFromStorage(): void {
    if (typeof localStorage === 'undefined') return
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return
    try {
      const data = JSON.parse(raw) as Record<string, string>
      for (const [k, v] of Object.entries(data)) {
        if (typeof k === 'string' && typeof v === 'string') {
          this.map.set(k, v)
        }
      }
    } catch {
      // Corrupt entry — ignore; next persist overwrites it.
    }
  }

  private schedulePersist(): void {
    if (typeof localStorage === 'undefined') return
    if (this.persistTimer != null) return
    this.persistTimer = setTimeout(() => {
      this.persistTimer = null
      const obj: Record<string, string> = {}
      for (const [k, v] of this.map) obj[k] = v
      try {
        localStorage.setItem(STORAGE_KEY, JSON.stringify(obj))
      } catch {
        // QuotaExceededError or similar — drop silently. The cache
        // stays in-memory and we'll retry on the next mutation.
      }
    }, PERSIST_DEBOUNCE_MS)
  }

  destroy(): void {
    if (this.persistTimer != null) {
      clearTimeout(this.persistTimer)
      this.persistTimer = null
    }
  }
}

/** Project an entity to the most recent station-mapped event in its
 *  history. Mirrors the rule the old 2.5D Factory.tsx used: walk
 *  recent_events from the tail and pick the most recent entry whose
 *  event_type maps to a known station. Falls back to
 *  current_event_type if recent_events doesn't include a station-
 *  mapped event but current_event_type does. */
export function projectEntityLocation(
  entity: FactoryEntity,
  knownStationIds: Set<string>,
): string | undefined {
  const recent = entity.recent_events ?? []
  for (let i = recent.length - 1; i >= 0; i--) {
    if (knownStationIds.has(recent[i].event_type)) {
      return recent[i].event_type
    }
  }
  if (entity.current_event_type && knownStationIds.has(entity.current_event_type)) {
    return entity.current_event_type
  }
  return undefined
}
