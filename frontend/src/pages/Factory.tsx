import { useEffect, useRef, useState } from 'react'
import {
  createIsoDebugScene,
  hashHue,
  type ClickedStationInfo,
  type IsoSceneHandle,
} from '../factory/iso-debug'
import { EntityLocationCache, projectEntityLocation } from '../factory/entity-cache'
import { useWebSocket } from '../hooks/useWebSocket'
import type { AgentRun, FactoryEntity, FactorySnapshot, FactoryStation, Task } from '../types'

// Production factory page — Babylon scene driven by /api/factory/snapshot
// and the WS event stream. The 12 stations on the floor map 1:1 to
// GitHub PR event types; their tray counts come from the snapshot,
// and chip animations between stations come from `event` WS messages
// resolved against an entity-location cache (kept in localStorage so
// it survives page reloads). Jira event types are ignored for now —
// the floor doesn't have Jira stations yet.

// Debounce window for snapshot refetches. The router publishes one
// `event` WS frame per detection plus a batched `tasks_updated` after
// each poll cycle; collapsing these into one /api/factory/snapshot
// call keeps the spam down. 1.5s feels instant to the user.
const REFETCH_DEBOUNCE_MS = 1500

// Event types the floor has stations for — same 12 as iso-debug.ts's
// hardcoded list. Used to filter snapshot entities and project them
// to the most recent station-mapped event in their history.
const KNOWN_STATION_EVENTS = new Set<string>([
  'github:pr:opened',
  'github:pr:ready_for_review',
  'github:pr:new_commits',
  'github:pr:conflicts',
  'github:pr:ci_check_passed',
  'github:pr:ci_check_failed',
  'github:pr:review_requested',
  'github:pr:review_approved',
  'github:pr:review_commented',
  'github:pr:review_changes_requested',
  'github:pr:closed',
  'github:pr:merged',
])

export default function Factory() {
  const containerRef = useRef<HTMLDivElement>(null)
  const sceneRef = useRef<IsoSceneHandle | null>(null)
  const cacheRef = useRef<EntityLocationCache | null>(null)
  const [picked, setPicked] = useState<ClickedStationInfo | null>(null)

  // Mount the Babylon scene + entity cache once. Both live for the
  // lifetime of the page; cleanup happens on unmount.
  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let cancelled = false
    let unsubClick: (() => void) | null = null
    cacheRef.current = new EntityLocationCache()
    createIsoDebugScene(container).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      sceneRef.current = scene
      unsubClick = scene.onStationClick(setPicked)
    })
    return () => {
      cancelled = true
      unsubClick?.()
      sceneRef.current?.destroy()
      sceneRef.current = null
      cacheRef.current?.destroy()
      cacheRef.current = null
    }
  }, [])

  // Snapshot loader — installs a window-level callback so the WS
  // listener (defined below, separate effect) can trigger refetches
  // without forcing a re-identify of the WS callback every render.
  // Pattern lifted from the old 2.5D Factory page.
  useEffect(() => {
    let cancelled = false
    let pending: ReturnType<typeof setTimeout> | null = null

    const load = () => {
      fetch('/api/factory/snapshot')
        .then((r) => {
          if (!r.ok) throw new Error(`Failed to load factory snapshot (${r.status})`)
          return r.json() as Promise<FactorySnapshot>
        })
        .then((data) => {
          if (cancelled) return
          applySnapshot(data, sceneRef.current, cacheRef.current)
        })
        .catch((err) => {
          if (cancelled) return
          console.warn('[factory] snapshot load failed:', err)
        })
    }

    load()

    const schedule = () => {
      if (pending) return
      pending = setTimeout(() => {
        pending = null
        load()
      }, REFETCH_DEBOUNCE_MS)
    }

    ;(window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch = schedule

    return () => {
      cancelled = true
      if (pending) clearTimeout(pending)
      delete (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
    }
  }, [])

  // WS listener. Three cases:
  //   • `event` for an entity transition → animate a chip from prior
  //     station to new station (if both are known and reachable),
  //     update the cache, and schedule a snapshot refetch.
  //   • `tasks_updated` / `agent_run_update` → just refetch; these
  //     don't move entities, only counts.
  //   • everything else → ignored.
  useWebSocket((evt) => {
    if (evt.type === 'event') {
      const e = evt.data
      const entityId = e.entity_id ?? null
      const newEvent = e.event_type
      const cache = cacheRef.current
      const scene = sceneRef.current
      if (entityId && newEvent && cache && scene && KNOWN_STATION_EVENTS.has(newEvent)) {
        const { prior } = cache.recordTransition(entityId, newEvent)
        if (prior && prior !== newEvent && KNOWN_STATION_EVENTS.has(prior)) {
          // Spawn the animation. Hue / label come from the snapshot's
          // entity record, looked up at spawn time. Returns false on
          // no-path-in-topology — we just skip the animation; the
          // refetch below will reconcile counts.
          scene.spawnChip({
            fromEvent: prior,
            toEvent: newEvent,
            // Label and hue resolved from the most recent snapshot we
            // have on hand. If the entity isn't in our last snapshot
            // (truly new), the chip flies blank — fine for one frame;
            // the next snapshot fills it in.
            ...resolveChipDecor(entityId),
          })
        }
      }
      const refetch = (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
      refetch?.()
    } else if (evt.type === 'tasks_updated' || evt.type === 'agent_run_update') {
      const refetch = (window as unknown as { __factoryRefetch?: () => void }).__factoryRefetch
      refetch?.()
    }
  })

  // Esc closes the drawer — common video-game-y dismiss gesture.
  useEffect(() => {
    if (!picked) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPicked(null)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [picked])

  return (
    <div className="relative -mx-8 -my-8">
      <div
        ref={containerRef}
        className="relative w-full overflow-hidden"
        style={{ height: 'calc(100vh - 69px)' }}
      />
      <button
        type="button"
        onClick={() => sceneRef.current?.resetView()}
        className="absolute bottom-4 right-4 rounded-md bg-white/92 px-3 py-2 text-[11px] font-semibold text-text-primary shadow transition hover:bg-white"
      >
        Reset view
      </button>
      <StationDrawer info={picked} />
    </div>
  )
}

// ─── Snapshot → scene wiring ───────────────────────────────────────

// Latest snapshot, kept module-scoped so the WS handler can resolve
// entity decorations (label, hue) at chip-spawn time without
// piping snapshot state through React refs. Only the most recent
// snapshot is ever read; older entries don't matter.
let lastSnapshotEntities: Map<string, FactoryEntity> = new Map()

function applySnapshot(
  data: FactorySnapshot,
  scene: IsoSceneHandle | null,
  cache: EntityLocationCache | null,
): void {
  // Cache-side: seed location cache from the snapshot's projection.
  // Drift between cache and snapshot is corrected here silently.
  if (cache) cache.seedFromSnapshot(data, KNOWN_STATION_EVENTS)

  // Module-scoped lookup for chip decoration at spawn time. Built
  // from the latest snapshot every refresh.
  const byId = new Map<string, FactoryEntity>()
  for (const e of data.entities) byId.set(e.id, e)
  lastSnapshotEntities = byId

  if (!scene) return

  // Project each entity to the station it's currently parked at —
  // walk recent_events tail, fall back to current_event_type. The
  // 2.5D factory uses the same rule (Factory.tsx:176-191 in the
  // old file) to avoid the "current_event_type ordered by insertion
  // time" bug that surfaced intermediate non-station events.
  const entityParkedAt = new Map<string, string>()
  for (const e of data.entities) {
    const at = projectEntityLocation(e, KNOWN_STATION_EVENTS)
    if (at) entityParkedAt.set(e.id, at)
  }

  // For each known station event_type, build:
  //   - runs   : array of { run, task, mine } from snapshot.stations
  //   - queued : entities parked here that aren't already in any run
  // and push to the scene. Stations with no entry in snapshot.stations
  // still get a setStationData call so their counts go to zero.
  for (const eventType of KNOWN_STATION_EVENTS) {
    const fs: FactoryStation | undefined = data.stations[eventType]
    const runs = fs?.runs ?? []
    const runEntityIds = new Set<string>(runs.map((r) => r.task.entity_id))
    const queued = data.entities.filter(
      (e) => entityParkedAt.get(e.id) === eventType && !runEntityIds.has(e.id),
    )
    scene.setStationData(eventType, {
      queuedCount: queued.length,
      runCount: runs.length,
      queued,
      runs,
    })
  }
}

// ─── Chip decoration ───────────────────────────────────────────────

/** Compute the chip's label + hue from the entity's snapshot record.
 *  GitHub: hue from `repo`, label `#<number>`. Jira: hue from project
 *  prefix of `source_id`, label = source_id (the Jira key). Falls back
 *  to no decor when the entity isn't in our latest snapshot — the
 *  chip still rides, just plainly. */
function resolveChipDecor(entityId: string): { label?: string; hue?: number } {
  const e = lastSnapshotEntities.get(entityId)
  if (!e) return {}
  if (e.source === 'github') {
    const label = e.number != null ? `#${e.number}` : undefined
    const hue = e.repo ? hashHue(e.repo) : undefined
    return { label, hue }
  }
  if (e.source === 'jira') {
    const label = e.source_id || undefined
    // Jira project key is the prefix before "-" (SKY-123 → "SKY").
    const projectKey = e.source_id?.split('-')[0]
    const hue = projectKey ? hashHue(projectKey) : undefined
    return { label, hue }
  }
  return {}
}

// ─── Drawer (top-down chassis view of the clicked station) ────────

// Bottom slide-up sheet — top-down view of the clicked station as
// pure HTML. Reads as the station's chassis seen from above with two
// recessed trays (intake left, main right), each ringed by a cyan
// LED glow and a dark machined floor. Mirrors the 3D scene's
// material palette so the drawer feels like a HUD readout of the
// thing on screen, not a generic data panel.
function StationDrawer({ info }: { info: ClickedStationInfo | null }) {
  const open = info != null
  return (
    <div
      className={`pointer-events-none absolute inset-x-0 bottom-0 z-40 transition-transform duration-300 ease-out ${
        open ? 'translate-y-0' : 'translate-y-full'
      }`}
      style={{ height: '46vh' }}
      aria-hidden={!open}
    >
      <div className="pointer-events-auto relative h-full bg-surface-raised/95 backdrop-blur-xl border-t border-border-glass shadow-2xl shadow-black/[0.12] flex items-stretch p-5">
        <StationChassis info={info} />
      </div>
    </div>
  )
}

// Cream chassis carrying the two trays. Background color, rounded
// edges, and inner padding mimic the 3D station body's silhouette
// at a higher zoom. The trays inside are inset boxes with cyan LED
// borders; the cream surrounding them carries the same warm
// off-white as the station body material (`#ece6d8`).
function StationChassis({ info }: { info: ClickedStationInfo | null }) {
  const queued = info?.queued ?? []
  const runs = info?.runs ?? []
  return (
    <div
      className="relative flex w-full gap-4 rounded-2xl p-4"
      style={{
        background: 'linear-gradient(180deg, #f1ebdc 0%, #e6e0d2 100%)',
        boxShadow:
          'inset 0 1px 0 rgba(255,255,255,0.8), inset 0 -2px 0 rgba(0,0,0,0.06), 0 4px 16px rgba(0,0,0,0.05)',
      }}
    >
      <Tray
        label="Queue"
        accent="#ff9c3a"
        widthClass="w-[28%]"
        emptyMessage="Idle — no entities waiting"
        items={queued.map((e) => ({
          key: e.id,
          dot: '#ff9c3a',
          body: <QueuedEntityRow entity={e} />,
        }))}
      />
      <Tray
        label={info?.label ?? '—'}
        accent="#7cf7ec"
        widthClass="flex-1"
        emptyMessage="No runs in flight"
        items={runs.map((r) => ({
          key: r.run.ID,
          dot: runStatusColor(r.run.Status),
          body: <RunRow run={r.run} task={r.task} />,
        }))}
      />
    </div>
  )
}

// One inset tray panel — dark machined floor, cyan LED ring around
// the rim, etched header at the top. The LED ring is two box-shadows:
// a tight 1px line (the trim itself) and a soft outer glow (the
// bloom). Combines with a dark interior to read like the 3D tray
// opening seen from above.
function Tray({
  label,
  accent,
  widthClass,
  items,
  emptyMessage,
}: {
  label: string
  accent: string
  widthClass: string
  items: { key: string; dot: string; body: React.ReactNode }[]
  emptyMessage: string
}) {
  return (
    <div
      className={`relative flex flex-col rounded-xl ${widthClass}`}
      style={{
        background: 'linear-gradient(180deg, #14120d 0%, #0d0c08 100%)',
        boxShadow: `
          inset 0 0 0 1px ${hexToRgba(accent, 0.55)},
          inset 0 1px 0 rgba(255,255,255,0.04),
          0 0 0 2px ${hexToRgba(accent, 0.12)},
          0 0 18px ${hexToRgba(accent, 0.28)}
        `,
      }}
    >
      <header
        className="px-4 py-2.5 border-b text-center"
        style={{ borderColor: hexToRgba(accent, 0.22) }}
      >
        <span
          className="text-[12px] font-semibold uppercase tracking-[0.18em]"
          style={{
            color: '#ffffff',
            textShadow: `0 0 10px ${hexToRgba(accent, 0.65)}, 0 0 2px rgba(255,255,255,0.6)`,
          }}
        >
          {label}
        </span>
      </header>
      <ul className="flex-1 overflow-y-auto px-3 py-3 flex flex-col gap-1.5">
        {items.length === 0 ? (
          <li className="px-2 py-1 text-[11px] italic text-white/35">{emptyMessage}</li>
        ) : (
          items.map((it) => (
            <li
              key={it.key}
              className="flex items-center gap-2.5 rounded-md px-2.5 py-1.5"
              style={{
                background: 'rgba(255,255,255,0.04)',
                boxShadow: `inset 0 0 0 1px ${hexToRgba(it.dot, 0.18)}`,
              }}
            >
              <span
                aria-hidden
                className="inline-block h-1.5 w-1.5 shrink-0 rounded-full"
                style={{ background: it.dot, boxShadow: `0 0 6px ${it.dot}` }}
              />
              {it.body}
            </li>
          ))
        )}
      </ul>
    </div>
  )
}

function QueuedEntityRow({ entity }: { entity: FactoryEntity }) {
  const label = entityLabel(entity)
  const title = entity.title || entity.source_id
  return (
    <div className="flex min-w-0 flex-1 items-baseline gap-2">
      <span className="font-mono text-[11px] text-white/85">{label}</span>
      <span className="truncate text-[10.5px] text-white/55">{title}</span>
    </div>
  )
}

function RunRow({ run, task }: { run: AgentRun; task: Task }) {
  const ref = task.source_id || task.entity_id
  return (
    <div className="flex min-w-0 flex-1 items-baseline gap-2">
      <span className="font-mono text-[11px] text-white/85">{ref}</span>
      <span
        className="text-[10px] uppercase tracking-wider"
        style={{ color: runStatusColor(run.Status) }}
      >
        {runStatusLabel(run.Status)}
      </span>
      <span className="ml-auto font-mono text-[10px] text-white/45">{formatRunMeta(run)}</span>
    </div>
  )
}

function entityLabel(e: FactoryEntity): string {
  if (e.source === 'github' && e.number != null) return `#${e.number}`
  return e.source_id || e.id.slice(0, 8)
}

// Status-keyed colors for the run-row indicator dot and the inline
// status label. Active states use the project cyan; pending_approval
// flips to amber; failures and cancellations read red. Anything else
// shown in the active runs list (rare) gets a neutral white.
function runStatusColor(status: string): string {
  switch (status) {
    case 'initializing':
    case 'cloning':
    case 'fetching':
    case 'worktree_created':
    case 'agent_starting':
    case 'running':
      return '#7cf7ec'
    case 'pending_approval':
      return '#ff9c3a'
    case 'failed':
      return '#ff6b6b'
    case 'cancelled':
      return '#b8805a'
    default:
      return '#ffffff'
  }
}

function runStatusLabel(status: string): string {
  switch (status) {
    case 'pending_approval':
      return 'pending'
    case 'agent_starting':
      return 'starting'
    case 'worktree_created':
      return 'preparing'
    default:
      return status
  }
}

function formatRunMeta(run: AgentRun): string {
  const parts: string[] = []
  if (run.DurationMs && run.DurationMs > 0) {
    const sec = Math.round(run.DurationMs / 1000)
    if (sec < 60) parts.push(`${sec}s`)
    else parts.push(`${Math.floor(sec / 60)}m ${sec % 60}s`)
  }
  if (run.TotalCostUSD && run.TotalCostUSD > 0) {
    parts.push(`$${run.TotalCostUSD.toFixed(2)}`)
  }
  return parts.join(' · ')
}

function hexToRgba(hex: string, alpha: number): string {
  const h = hex.replace('#', '')
  const r = parseInt(h.slice(0, 2), 16)
  const g = parseInt(h.slice(2, 4), 16)
  const b = parseInt(h.slice(4, 6), 16)
  return `rgba(${r}, ${g}, ${b}, ${alpha})`
}
