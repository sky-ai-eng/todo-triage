import { forwardRef } from 'react'
import type { Task } from '../types'
import EventBadge from './EventBadge'
import SourceBadge from './SourceBadge'

interface Props {
  task: Task
  style?: React.CSSProperties
  isDragging?: boolean
  onRequeue?: () => void
  // SKY-261 B+: when a task is bot-claimed but the delegate run failed
  // to spawn (or no run materialized yet for the bot-claimed task),
  // the agent-lane card surfaces the failure here. delegateFailed
  // carries the error message (e.g. "prompt not found"); onRetry fires
  // the same delegate gesture again. Claim is commitment, runs are
  // execution; this prop is how the Board reflects that divergence.
  delegateFailed?: { message: string }
  onRetry?: () => void
}

const TaskCard = forwardRef<HTMLDivElement, Props & React.HTMLAttributes<HTMLDivElement>>(
  ({ task, style, isDragging, onRequeue, delegateFailed, onRetry, ...props }, ref) => {
    const age = formatAge(task.created_at)
    // Normalize once so the condition and the prop share the same non-nullable
    // value — avoids the non-null assertion on a field typed as optional.
    const subtaskCount = task.open_subtask_count ?? 0
    // SKY-261 B+: snooze is orthogonal to claim. A claimed-and-snoozed
    // task is "owned by X, wait until Y" — first-class in the owner's
    // lane (You / Agent) rather than hidden in a separate Snoozed column.
    // We render the badge whenever snooze_until is in the future;
    // past-time means the snooze has elapsed and the wake-on-bump path
    // will move it back to status='queued' on the next event.
    const snoozedUntil = parseFutureSnooze(task.snooze_until)
    const isSnoozed = snoozedUntil !== null

    return (
      <div
        ref={ref}
        style={style}
        className={`bg-surface-raised backdrop-blur-xl border ${
          delegateFailed
            ? 'border-snooze/40'
            : isSnoozed
              ? 'border-snooze/25 opacity-80'
              : 'border-border-glass'
        } rounded-2xl p-4 shadow-sm shadow-black/[0.02] transition-shadow cursor-grab active:cursor-grabbing ${
          isDragging ? 'shadow-lg shadow-black/[0.08] border-accent/30 z-50' : ''
        }`}
        {...props}
      >
        <div className="flex items-center gap-2 mb-2">
          <SourceBadge task={task} />
          <EventBadge eventType={task.event_type} compact />
          {subtaskCount > 0 && <SubtaskHint count={subtaskCount} />}
          {isSnoozed && <SnoozedBadge until={snoozedUntil} />}
          {delegateFailed && <DelegateFailedBadge message={delegateFailed.message} />}
        </div>

        <h3 className="text-[13px] font-semibold text-text-primary leading-snug line-clamp-2 mb-1">
          {task.title}
        </h3>

        {task.ai_summary && (
          <p className="text-[12px] text-text-tertiary line-clamp-2 mb-2">{task.ai_summary}</p>
        )}

        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2 text-[11px] text-text-tertiary">
            <span>{age}</span>
          </div>

          <div className="flex items-center gap-3">
            {delegateFailed && onRetry && (
              <button
                onClick={(e) => {
                  e.stopPropagation()
                  onRetry()
                }}
                onPointerDown={(e) => e.stopPropagation()}
                className="text-[12px] text-snooze hover:text-snooze/70 font-medium transition-colors"
                title="Re-attempt the delegate run"
              >
                Retry
              </button>
            )}
            {onRequeue && (
              <button
                onClick={(e) => {
                  e.stopPropagation()
                  onRequeue()
                }}
                onPointerDown={(e) => e.stopPropagation()}
                className="text-[12px] text-text-tertiary hover:text-text-primary font-medium transition-colors"
                title="Return to queue"
              >
                Requeue
              </button>
            )}
            <a
              href={task.source_url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
              onClick={(e) => e.stopPropagation()}
              onPointerDown={(e) => e.stopPropagation()}
            >
              Open
            </a>
          </div>
        </div>
      </div>
    )
  },
)

TaskCard.displayName = 'TaskCard'
export default TaskCard

function formatAge(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'just now'
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

// SubtaskHint appears on a task card whose Jira entity has gained open
// subtasks since the task was created — the task represents scope that
// has since been decomposed. Uses the snooze color (warm amber) rather
// than dismiss/accent to read as "worth a second look, not an error".
function SubtaskHint({ count }: { count: number }) {
  const label = count === 1 ? '1 open subtask' : `${count} open subtasks`
  return (
    <span
      title="This ticket has open subtasks — the work may have been decomposed since the task was queued. Consider dismissing and working the subtasks directly."
      className="inline-flex items-center gap-1 rounded-full border border-snooze/25 bg-snooze/[0.08] px-1.5 py-0.5 text-[10px] font-medium text-snooze"
    >
      <span aria-hidden>⋮</span>
      {label}
    </span>
  )
}

// DelegateFailedBadge surfaces "the bot is claimed but the run didn't
// fire" — the SKY-261 B+ failure state. claim is commitment, runs are
// execution; when they diverge, this is how the Board tells the user.
// Uses the snooze amber to read "needs your attention" without
// escalating to red (the task isn't broken, just stuck).
function DelegateFailedBadge({ message }: { message: string }) {
  return (
    <span
      title={`The bot took this task but the run didn't fire: ${message}. Click Retry to re-attempt.`}
      className="inline-flex items-center gap-1 rounded-full border border-snooze/30 bg-snooze/[0.10] px-1.5 py-0.5 text-[10px] font-medium text-snooze"
    >
      <span aria-hidden>⚠</span>
      delegate didn't fire
    </span>
  )
}

// SnoozedBadge surfaces "task is owned but parked until X." A claimed-
// and-snoozed task lives in its owner's lane (You / Agent) — the
// badge is the signal that the owner is deferring action, not that
// the task is unowned. Uses the same snooze amber as DelegateFailed
// + the muted card opacity to read "set aside on purpose."
function SnoozedBadge({ until }: { until: Date }) {
  return (
    <span
      title={`Snoozed until ${until.toLocaleString()}. Wakes automatically on next matching event, or via the Requeue affordance.`}
      className="inline-flex items-center gap-1 rounded-full border border-snooze/30 bg-snooze/[0.08] px-1.5 py-0.5 text-[10px] font-medium text-snooze"
    >
      <span aria-hidden>⏾</span>
      wakes {formatSnoozeUntil(until)}
    </span>
  )
}

// parseFutureSnooze returns the snooze target as a Date when the
// task is currently snoozed (timestamp parseable + in the future).
// Returns null otherwise — past timestamps mean the snooze has
// elapsed and the wake-on-bump path will move the task back to
// status='queued' on the next event; in the meantime we don't need
// to render the badge as if the user still owns the deferral.
function parseFutureSnooze(snoozeUntil: string | undefined): Date | null {
  if (!snoozeUntil) return null
  const d = new Date(snoozeUntil)
  if (Number.isNaN(d.getTime())) return null
  if (d.getTime() <= Date.now()) return null
  return d
}

// formatSnoozeUntil prints "in 2h" / "in 3d" / "Mar 5" so the badge
// stays compact. Coarse buckets only — the full timestamp lives in
// the title attribute for hover-to-see.
function formatSnoozeUntil(until: Date): string {
  const diff = until.getTime() - Date.now()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'soon'
  if (hours < 24) return `in ${hours}h`
  const days = Math.floor(hours / 24)
  if (days < 7) return `in ${days}d`
  return until.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}
