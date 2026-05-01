import * as Tooltip from '@radix-ui/react-tooltip'
import { useEffect, useRef, useState } from 'react'
import { createIsoScene, type ClickedStationInfo, type IsoSceneHandle } from '../factory/iso-scene'
import { useWebSocket } from '../hooks/useWebSocket'
import type { AgentRun, FactoryEntity, FactorySnapshot, Task } from '../types'

// Production factory page — Babylon scene driven by /api/factory/snapshot.
// The page itself does almost nothing visual: it fetches the snapshot,
// hands it to the scene, and pipes WS frames into a debounced refetch.
//
// All entity placement (which station they're parked at, whether
// they're mid-flight on a bridge and how far along) is derived inside
// the scene by a per-frame reconciler reading the snapshot's
// `current_event_type` + `last_event_at` + `recent_events`. The
// backend is therefore authoritative for both station tray counts and
// chip positions; the same projection feeds both, so they cannot drift.
// See `factory/place-entity.ts` for the projection function.

// Debounce window for snapshot refetches. The router publishes one
// `event` WS frame per detection plus a batched `tasks_updated` per
// poll cycle; collapsing those into one /api/factory/snapshot call
// keeps server load down. Kept short so chip motion appears promptly
// after the WS event lands — the snapshot determines when a transit
// becomes visible (it carries the new last_event_at), and a long
// debounce delays that.
const REFETCH_DEBOUNCE_MS = 250

export default function Factory() {
  const containerRef = useRef<HTMLDivElement>(null)
  const sceneRef = useRef<IsoSceneHandle | null>(null)
  const [picked, setPicked] = useState<ClickedStationInfo | null>(null)

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let cancelled = false
    let unsubClick: (() => void) | null = null
    createIsoScene(container).then((scene) => {
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
    }
  }, [])

  // Snapshot loader. The window-level callback is what the WS
  // effect (next block) calls to schedule a debounced refetch
  // without forcing this effect's identity to change on every render.
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
          sceneRef.current?.applySnapshot(data)
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

  // WS frames are refetch hints only. Backend authority means we
  // don't try to drive any chip or count update directly from the
  // event payload — the next snapshot carries the same information
  // in a form that's coherent with everything else on the floor.
  useWebSocket((evt) => {
    if (evt.type === 'event' || evt.type === 'tasks_updated' || evt.type === 'agent_run_update') {
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

  // While the drawer is open for a station, mirror live changes from
  // the scene reconciler — entities entering or leaving the queue,
  // runs starting or completing. The scene fires only on real
  // content changes (per-station hash dedup), so this won't churn
  // re-renders on idle frames. Re-subscribes on station change;
  // unsubscribes when the drawer closes.
  const pickedId = picked?.id
  useEffect(() => {
    if (!pickedId) return
    const scene = sceneRef.current
    if (!scene) return
    return scene.onStationDataChange(pickedId, (info) => {
      setPicked(info)
    })
  }, [pickedId])

  return (
    <div className="relative -mx-8 -my-8 overflow-hidden">
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
    <Tooltip.Provider delayDuration={250}>
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
    </Tooltip.Provider>
  )
}

// Cream chassis carrying the two trays. The chassis represents the
// physical station body; the trays inside are light glass surfaces
// (frosted white panels with thin colored accent strips) so the whole
// drawer reads as the project's warm/light palette rather than a
// dark gaming HUD.
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
        accent="#c47a5a"
        widthClass="w-[28%]"
        emptyMessage="Idle — no entities waiting"
        items={queued.map((e) => ({
          key: e.id,
          dot: '#c47a5a',
          body: <QueuedEntityRow entity={e} />,
          href: e.url || undefined,
          tooltip: <EntityTooltip entity={e} />,
        }))}
      />
      <Tray
        label={info?.label ?? '—'}
        accent="#5a8c6a"
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

// One light glass tray panel. Frosted white interior, thin accent
// strip across the top header (project-palette color), warm hairline
// border, soft outer shadow for the floating-pane feel. Header text
// is dark on light — readable, restrained — with a small accent dot
// to the left and the tray label in tracked caps.
interface TrayItem {
  key: string
  dot: string
  body: React.ReactNode
  /** When set, the row becomes an `<a>` opening the URL in a new tab. */
  href?: string
  /** When set, hovering the row reveals this content in a Radix tooltip. */
  tooltip?: React.ReactNode
}

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
  items: TrayItem[]
  emptyMessage: string
}) {
  return (
    <div
      className={`relative flex flex-col overflow-hidden rounded-xl ${widthClass}`}
      style={{
        background:
          'linear-gradient(180deg, rgba(255,255,255,0.85) 0%, rgba(255,255,255,0.6) 100%)',
        boxShadow: [
          'inset 0 1px 0 rgba(255,255,255,0.9)',
          'inset 0 0 0 1px rgba(255,255,255,0.7)',
          '0 1px 2px rgba(0,0,0,0.04)',
          '0 6px 18px rgba(0,0,0,0.06)',
        ].join(', '),
      }}
    >
      {/* Thin accent strip — replaces the heavy LED border. Sits
          at the very top edge of the tray as a single colored line. */}
      <div className="absolute inset-x-0 top-0 h-[2px]" style={{ background: accent }} />
      <header
        className="flex items-center justify-center gap-2 px-4 py-2.5"
        style={{ borderBottom: '1px solid rgba(0,0,0,0.05)' }}
      >
        <span
          aria-hidden
          className="inline-block h-1.5 w-1.5 rounded-full"
          style={{ background: accent, boxShadow: `0 0 4px ${hexToRgba(accent, 0.55)}` }}
        />
        <span className="text-[11px] font-semibold uppercase tracking-[0.18em] text-text-secondary">
          {label}
        </span>
      </header>
      <ul className="flex flex-1 flex-col gap-1.5 overflow-y-auto px-3 py-3">
        {items.length === 0 ? (
          <li className="px-2 py-1 text-[11px] italic text-text-tertiary">{emptyMessage}</li>
        ) : (
          items.map((it) => <TrayRow key={it.key} item={it} />)
        )}
      </ul>
    </div>
  )
}

// Single tray row. Conditionally renders the row content as an `<a>`
// when `href` is set so clicking opens the entity in a new tab; the
// inner content adopts `display: contents` so the anchor doesn't
// affect the row's flex layout. Wrapping with Radix Tooltip is also
// conditional — only rows that pass `tooltip` get the hover popup,
// which keeps Run rows untouched.
function TrayRow({ item }: { item: TrayItem }) {
  const interactive = !!item.href
  const innerClasses = 'flex flex-1 min-w-0 items-center gap-2.5'
  const inner = (
    <>
      <span
        aria-hidden
        className="inline-block h-1.5 w-1.5 shrink-0 rounded-full"
        style={{ background: item.dot, boxShadow: `0 0 6px ${item.dot}` }}
      />
      {item.body}
    </>
  )
  // The row's outer `<li>` keeps its inset LED border + bg; the
  // anchor sits inside as a "contents" element so its rect is the
  // same as the li's content area — clicking anywhere on the row
  // (except the dot, which is aria-hidden) hits the link.
  const rowContent = item.href ? (
    <a
      href={item.href}
      target="_blank"
      rel="noopener noreferrer"
      className={`${innerClasses} cursor-pointer`}
    >
      {inner}
    </a>
  ) : (
    <div className={innerClasses}>{inner}</div>
  )
  const li = (
    <li
      className={`flex items-center gap-2.5 rounded-md px-2.5 py-1.5 transition-all ${
        interactive ? 'hover:-translate-y-px hover:bg-white' : ''
      }`}
      style={{
        background: 'rgba(255,255,255,0.55)',
        boxShadow: 'inset 0 0 0 1px rgba(255,255,255,0.85), 0 1px 2px rgba(0,0,0,0.03)',
      }}
    >
      {rowContent}
    </li>
  )
  if (!item.tooltip) return li
  return (
    <Tooltip.Root>
      <Tooltip.Trigger asChild>{li}</Tooltip.Trigger>
      <Tooltip.Portal>
        <Tooltip.Content
          side="top"
          align="start"
          sideOffset={6}
          className="z-[100] max-w-[320px] rounded-lg border border-border-glass px-3 py-2.5 text-[12px] text-text-primary leading-relaxed animate-in fade-in-0 zoom-in-95"
          style={{
            // Opaque base + top-light gradient → bottom-shaded gives the
            // liquid-glass sheen without bleed-through. Inset highlight
            // on the top edge sells "polished pane"; layered drop shadow
            // gives the float/separation from the tray underneath.
            background: 'linear-gradient(180deg, #fbf9f4 0%, #f1ece0 100%)',
            boxShadow: [
              'inset 0 1px 0 rgba(255,255,255,0.7)',
              'inset 0 -1px 0 rgba(0,0,0,0.04)',
              '0 1px 2px rgba(0,0,0,0.06)',
              '0 8px 24px rgba(0,0,0,0.18)',
            ].join(', '),
          }}
        >
          {item.tooltip}
          <Tooltip.Arrow style={{ fill: '#f1ece0' }} />
        </Tooltip.Content>
      </Tooltip.Portal>
    </Tooltip.Root>
  )
}

function QueuedEntityRow({ entity }: { entity: FactoryEntity }) {
  const label = entityLabel(entity)
  const title = entity.title || entity.source_id
  return (
    <div className="flex min-w-0 flex-1 items-baseline gap-2">
      <span className="font-mono text-[11px] text-text-primary">{label}</span>
      <span className="truncate text-[10.5px] text-text-secondary">{title}</span>
    </div>
  )
}

// Hover popup for a queued entity row. Shows the full title (no
// truncation) plus source-specific metadata so the user can read the
// long context without having to click through. Mirrors the columns
// the old factory's StationDetailOverlay surfaced.
function EntityTooltip({ entity }: { entity: FactoryEntity }) {
  const meta: { k: string; v: string }[] = []
  if (entity.source === 'github') {
    if (entity.repo) meta.push({ k: 'repo', v: entity.repo })
    if (entity.number != null) meta.push({ k: 'number', v: `#${entity.number}` })
    if (entity.author) meta.push({ k: 'author', v: `@${entity.author}` })
    if (entity.additions != null || entity.deletions != null) {
      const add = entity.additions ?? 0
      const del = entity.deletions ?? 0
      meta.push({ k: 'diff', v: `+${add} −${del}` })
    }
  } else if (entity.source === 'jira') {
    if (entity.source_id) meta.push({ k: 'key', v: entity.source_id })
    if (entity.status) meta.push({ k: 'status', v: entity.status })
    if (entity.priority) meta.push({ k: 'priority', v: entity.priority })
    if (entity.assignee) meta.push({ k: 'assignee', v: entity.assignee })
  }
  return (
    <div className="space-y-2">
      <div className="font-medium text-[12.5px] leading-snug text-text-primary">
        {entity.title || entity.source_id || entity.id}
      </div>
      {meta.length > 0 && (
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-[11px]">
          {meta.map((m) => (
            <div key={m.k} className="contents">
              <dt className="uppercase tracking-wider text-text-tertiary">{m.k}</dt>
              <dd className="font-mono text-text-secondary">{m.v}</dd>
            </div>
          ))}
        </dl>
      )}
      {entity.url && (
        <div className="border-t border-border-glass pt-1.5 text-[10.5px] text-text-tertiary">
          Click to open in new tab →
        </div>
      )}
    </div>
  )
}

function RunRow({ run, task }: { run: AgentRun; task: Task }) {
  const ref = task.source_id || task.entity_id
  return (
    <div className="flex min-w-0 flex-1 items-baseline gap-2">
      <span className="font-mono text-[11px] text-text-primary">{ref}</span>
      <span
        className="text-[10px] uppercase tracking-wider"
        style={{ color: runStatusColor(run.Status) }}
      >
        {runStatusLabel(run.Status)}
      </span>
      <span className="ml-auto font-mono text-[10px] text-text-tertiary">{formatRunMeta(run)}</span>
    </div>
  )
}

function entityLabel(e: FactoryEntity): string {
  if (e.source === 'github' && e.number != null) return `#${e.number}`
  return e.source_id || e.id.slice(0, 8)
}

// Status-keyed colors for the run-row indicator dot and the inline
// status label. Pulled from the project palette tokens so the trays
// feel cohesive with the rest of the app: claim/sage for active,
// snooze/amber for pending, dismiss/rose for failed, neutral tertiary
// for cancelled.
function runStatusColor(status: string): string {
  switch (status) {
    case 'initializing':
    case 'cloning':
    case 'fetching':
    case 'worktree_created':
    case 'agent_starting':
    case 'running':
      return '#5a8c6a' // --color-claim (sage)
    case 'pending_approval':
      return '#b8943a' // --color-snooze (warm amber)
    case 'failed':
      return '#c45a5a' // --color-dismiss (warm rose)
    case 'cancelled':
      return '#a09a94' // --color-text-tertiary (neutral)
    default:
      return '#6b6560' // --color-text-secondary
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
