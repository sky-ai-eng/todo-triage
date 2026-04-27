import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'

// Near-zoom HTML overlay that takes over a station's interior when the
// viewport zooms in far enough for the Pixi-drawn core chamber to feel
// empty. Positioned absolutely over the Pixi station using the screen-
// space placement scene.ts publishes via `onView`. At lower zoom this
// component does not render at all — the Pixi station shows its default
// glyph + predicate chips.
//
// The overlay covers the core chamber and the chip strip (everything
// below the station header). Three stacked regions, top to bottom:
//   - active runs list (scrollable if more than fit)
//   - throughput strip (replaces predicate chips)
//   - optional wired-triggers caret (collapsed; click to expand)
//
// Clicking a run row calls `onOpenRun(run)` so the parent page can mount
// a drawer with the full AgentCard. Data is stubbed for the first pass —
// we render placeholder content until the /api/factory/runs-by-station
// wiring lands.

import { FACTORY_EVENTS } from './events'
import type { StationScreenPlacement } from './scene'
import type { AgentRun, Task } from '../types'

export interface StationRunSummary {
  run: AgentRun
  task: Task
  /** Whether the task's entity is authored by the session user. Drives the
   * ownership tint shown next to the title — matches the item tint used
   * on the belts. */
  mine: boolean
}

export interface StationThroughput {
  /** Count of items (entity events) seen at this station in the last 24h. */
  items24h: number
  /** How many of those items produced a task (matched a task_rule / trigger). */
  triggered24h: number
  /** Count of active runs at this station right now. */
  active: number
}

/** Entity parked at this station with no active run — "waiting" (nothing
 * fired on it, it's just sitting there as its latest event). Rendered in
 * the horizontally scrollable strip between the runs list and throughput. */
export interface StationWaitingEntity {
  id: string
  label: string
  title: string
  repo?: string
  author?: string
  /** Additions/deletions for GitHub PRs; undefined for Jira issues. */
  diffAdd?: number
  diffDel?: number
  mine: boolean
  url: string
}

interface Props {
  placement: StationScreenPlacement
  /** Live runs currently active at this station. May be empty — in that
   * case the list area shows a neutral "no active runs" label and the
   * throughput strip carries the signal. */
  runs?: StationRunSummary[]
  /** Entities parked at this station without an active run. */
  waiting?: StationWaitingEntity[]
  throughput?: StationThroughput
  onOpenRun: (summary: StationRunSummary) => void
}

// Fractions of the station's frame that the overlay covers. Derived from
// the world-coord constants in station.ts (HEADER_H=40, H=180, CHIPS_H=32,
// CORE_PAD_TOP=4). Kept as ratios so the overlay tracks the station rect
// regardless of scale without re-deriving screen math per region.
const HEADER_FRACTION = 44 / 180 // header + top padding
const INNER_PAD_X_FRACTION = 12 / 260

export default function StationDetailOverlay({
  placement,
  runs,
  waiting,
  throughput,
  onOpenRun,
}: Props) {
  const event = FACTORY_EVENTS[placement.eventType]
  const color = event?.tint ?? categoryColor(event?.category)

  const top = placement.screenY + placement.screenH * HEADER_FRACTION
  const left = placement.screenX + placement.screenW * INNER_PAD_X_FRACTION
  const width = placement.screenW * (1 - INNER_PAD_X_FRACTION * 2)
  const height = placement.screenH - (top - placement.screenY) - placement.screenH * 0.04

  const activeRuns = runs ?? []
  const waitingEntities = waiting ?? []
  const t = throughput ?? { items24h: 0, triggered24h: 0, active: activeRuns.length }

  return (
    <div
      className="absolute flex flex-col rounded-xl overflow-hidden pointer-events-auto"
      style={{
        top,
        left,
        width,
        height,
        background: 'rgba(247, 245, 242, 0.92)',
        backdropFilter: 'blur(8px)',
        WebkitBackdropFilter: 'blur(8px)',
        border: `1px solid ${hex(color, 0.18)}`,
        boxShadow: `inset 0 1px 0 rgba(255, 255, 255, 0.6)`,
      }}
    >
      {/* Active runs region */}
      <div className="flex-1 min-h-0 flex flex-col">
        <div
          className="px-3 pt-2 pb-1 flex items-center justify-between text-[10px] font-semibold uppercase tracking-wider"
          style={{ color: hex(color, 0.75), letterSpacing: '0.08em' }}
        >
          <span>Active runs</span>
          <span className="text-text-tertiary normal-case font-medium tracking-normal">
            {activeRuns.length === 0 ? '—' : activeRuns.length}
          </span>
        </div>

        <div className="flex-1 min-h-0 overflow-y-auto px-1 pb-1">
          {activeRuns.length === 0 ? (
            <div className="h-full flex items-center justify-center text-[11px] text-text-tertiary italic">
              No active runs
            </div>
          ) : (
            <ul className="flex flex-col gap-0.5">
              {activeRuns.map((summary) => (
                <RunRow key={summary.run.ID} summary={summary} onOpen={() => onOpenRun(summary)} />
              ))}
            </ul>
          )}
        </div>
      </div>

      {/* Waiting entities — parked at this station with no active run.
          Horizontally scrollable so a long queue doesn't push other
          overlay regions. Hidden when nothing's waiting to keep the
          resting state clean. */}
      {waitingEntities.length > 0 && (
        <div
          className="px-2 py-1 flex items-center gap-1 overflow-x-auto border-t shrink-0"
          style={{
            borderColor: hex(color, 0.15),
            background: hex(color, 0.02),
            scrollbarWidth: 'thin',
          }}
        >
          <span
            className="shrink-0 text-[9px] font-semibold uppercase tracking-wider mr-1"
            style={{ color: hex(color, 0.7), letterSpacing: '0.08em' }}
          >
            Waiting
          </span>
          {waitingEntities.map((e) => (
            <WaitingPill key={e.id} entity={e} color={color} />
          ))}
        </div>
      )}

      {/* Throughput strip — replaces the predicate chip row */}
      <div
        className="px-3 py-1.5 flex items-center justify-between text-[10px] border-t"
        style={{
          borderColor: hex(color, 0.15),
          background: hex(color, 0.04),
        }}
      >
        <span className="text-text-secondary font-medium">24h</span>
        <div className="flex items-center gap-3 text-text-tertiary">
          <Stat label="seen" value={t.items24h} />
          <Stat label="triggered" value={t.triggered24h} />
          <Stat label="active" value={t.active} />
        </div>
      </div>
    </div>
  )
}

interface RunRowProps {
  summary: StationRunSummary
  onOpen: () => void
}

function RunRow({ summary, onOpen }: RunRowProps) {
  const { run, task, mine } = summary
  const tint = mine ? '#c47a5a' : '#7a9aad'
  const statusColor =
    run.Status === 'failed' || run.Status === 'cancelled'
      ? 'text-dismiss'
      : run.Status === 'pending_approval'
        ? 'text-snooze'
        : 'text-delegate'

  const elapsed = formatElapsed(run.StartedAt)
  const cost = run.TotalCostUSD != null ? `$${run.TotalCostUSD.toFixed(2)}` : null

  return (
    <li>
      <button
        onClick={onOpen}
        className="w-full flex items-center gap-2 px-2 py-1.5 rounded-md hover:bg-white/70 text-left transition-colors"
      >
        <span
          className="shrink-0 w-1.5 h-1.5 rounded-full"
          style={{ background: tint }}
          aria-hidden
        />
        <span className="flex-1 min-w-0 truncate text-[12px] font-medium text-text-primary">
          {task.title || task.source_id}
        </span>
        <span className={`shrink-0 text-[10px] font-medium ${statusColor}`}>{elapsed}</span>
        {cost && (
          <span className="shrink-0 text-[10px] text-text-tertiary tabular-nums">{cost}</span>
        )}
      </button>
    </li>
  )
}

function WaitingPill({ entity, color }: { entity: StationWaitingEntity; color: number }) {
  const tint = entity.mine ? '#c47a5a' : '#7a9aad'
  const hasDiff = entity.diffAdd != null || entity.diffDel != null
  const [hovered, setHovered] = useState(false)
  const anchorRef = useRef<HTMLElement | null>(null)
  const [pos, setPos] = useState<{ left: number; top: number } | null>(null)

  // On hover, compute the anchor's viewport rect and place the card just
  // below it. Rendered via a portal to document.body so the overlay's
  // overflow-hidden + the strip's overflow-x-auto (which forces vertical
  // clipping too) can't hide the card. Fixed positioning keeps it
  // anchored while the user moves their cursor between pills.
  //
  // The setPos calls are synchronous within the effect; the rule
  // (react-hooks/set-state-in-effect) flags this, but reading
  // getBoundingClientRect() is exactly the DOM-measurement side-effect
  // that useEffect is for. Disabling the rule for this measurement
  // effect rather than refactoring to derived state.
  useEffect(() => {
    if (!hovered) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setPos(null)
      return
    }
    const el = anchorRef.current
    if (!el) return
    const rect = el.getBoundingClientRect()
    setPos({ left: rect.left + rect.width / 2, top: rect.bottom + 6 })
  }, [hovered])

  const body = (
    <>
      <span
        className="shrink-0 w-1.5 h-1.5 rounded-full"
        style={{ background: tint }}
        aria-hidden
      />
      <span className="text-[11px] font-medium text-text-primary">{entity.label}</span>
    </>
  )
  const pillClass =
    'shrink-0 inline-flex items-center gap-1 px-1.5 py-0.5 rounded-md border transition-colors'
  const pillStyle = {
    borderColor: hex(color, 0.2),
    background: 'rgba(255, 255, 255, 0.6)',
  }

  const card =
    pos &&
    createPortal(
      <div
        className="pointer-events-none fixed z-50"
        style={{
          left: pos.left,
          top: pos.top,
          transform: 'translateX(-50%)',
          minWidth: 180,
          maxWidth: 260,
        }}
      >
        <div
          className="rounded-md border px-2.5 py-1.5 shadow-lg"
          style={{
            borderColor: hex(color, 0.25),
            background: 'rgba(252, 250, 247, 0.98)',
            backdropFilter: 'blur(8px)',
            WebkitBackdropFilter: 'blur(8px)',
          }}
        >
          <div className="text-[11px] font-semibold text-text-primary leading-snug">
            {entity.title}
          </div>
          {entity.repo && (
            <div className="text-[10px] text-text-tertiary mt-0.5">{entity.repo}</div>
          )}
          <div className="flex items-center justify-between gap-2 mt-1">
            {entity.author && (
              <span className="text-[10px] font-medium" style={{ color: tint }}>
                {entity.author}
              </span>
            )}
            {hasDiff && (
              <span className="text-[10px] font-mono text-text-secondary tabular-nums">
                +{entity.diffAdd ?? 0} −{entity.diffDel ?? 0}
              </span>
            )}
          </div>
        </div>
      </div>,
      document.body,
    )

  const handlers = {
    onMouseEnter: () => setHovered(true),
    onMouseLeave: () => setHovered(false),
  }

  if (!entity.url) {
    return (
      <>
        <span
          ref={(el) => {
            anchorRef.current = el
          }}
          className={pillClass}
          style={pillStyle}
          {...handlers}
        >
          {body}
        </span>
        {card}
      </>
    )
  }
  return (
    <>
      <a
        ref={(el) => {
          anchorRef.current = el
        }}
        href={entity.url}
        target="_blank"
        rel="noopener noreferrer"
        className={`${pillClass} hover:bg-white/90`}
        style={pillStyle}
        {...handlers}
      >
        {body}
      </a>
      {card}
    </>
  )
}

function Stat({ label, value }: { label: string; value: number }) {
  return (
    <span>
      <span className="font-semibold text-text-secondary tabular-nums">{value}</span>{' '}
      <span>{label}</span>
    </span>
  )
}

function formatElapsed(startedAt: string): string {
  const diff = Date.now() - new Date(startedAt).getTime()
  const s = Math.floor(diff / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  const rem = s % 60
  return rem > 0 ? `${m}m ${rem}s` : `${m}m`
}

function categoryColor(category?: string): number {
  switch (category) {
    case 'pr_flow':
      return 0xc47a5a
    case 'pr_review':
      return 0x7a9aad
    case 'pr_ci':
      return 0x6ea87a
    case 'pr_signals':
      return 0x9a7aad
    case 'jira_flow':
      return 0xb8943a
    case 'jira_signals':
      return 0x8a8480
    default:
      return 0xc47a5a
  }
}

function hex(color: number, alpha: number): string {
  const r = (color >> 16) & 0xff
  const g = (color >> 8) & 0xff
  const b = color & 0xff
  return `rgba(${r}, ${g}, ${b}, ${alpha})`
}
