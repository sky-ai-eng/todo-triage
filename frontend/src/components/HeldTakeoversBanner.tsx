import { useCallback, useEffect, useState } from 'react'
import type { HeldTakeover, WSEvent } from '../types'
import { useWebSocket } from '../hooks/useWebSocket'
import { readError } from '../lib/api'
import { toast } from './Toast/toastStore'
import TakeoverModal, { type TakeoverInfo } from './TakeoverModal'

// HeldTakeoversBanner shows takeover dirs the user is still holding on to.
// A held takeover is a `taken_over` run whose worktree_path is non-empty —
// the user took the run over but hasn't released the worktree yet, so the
// dir + branch ref still occupy disk and block the next delegated run on
// the same PR from fetching into the branch ref.
//
// Hidden when there's nothing held (returns null) so the Board doesn't
// gain an empty row in the steady state. Re-fetches on every
// `agent_run_update` WS event so the row disappears as soon as the
// user clicks Release on this banner OR on the AgentCard footer button.
export default function HeldTakeoversBanner() {
  const [items, setItems] = useState<HeldTakeover[] | null>(null)
  const [expanded, setExpanded] = useState(false)
  const [resumeInfo, setResumeInfo] = useState<TakeoverInfo | null>(null)
  const [pendingReleaseID, setPendingReleaseID] = useState<string | null>(null)

  const refresh = useCallback(async () => {
    try {
      const res = await fetch('/api/agent/takeovers/held')
      if (!res.ok) return
      const data = (await res.json()) as HeldTakeover[]
      setItems(data ?? [])
    } catch {
      // Silently ignore — banner is supplementary; the AgentCard
      // footer's Release button is the always-available fallback.
    }
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  const onWS = useCallback(
    (ev: WSEvent) => {
      if (ev.type === 'agent_run_update') refresh()
    },
    [refresh],
  )
  useWebSocket(onWS)

  const release = async (id: string) => {
    if (!confirm('Release this takeover? The worktree dir will be deleted.')) return
    setPendingReleaseID(id)
    try {
      const res = await fetch(`/api/agent/runs/${id}/release`, { method: 'POST' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to release takeover'))
        return
      }
      // The WS broadcast will trigger refresh(); fire one explicit
      // refresh too so the row leaves the banner even if the WS
      // connection lags.
      refresh()
    } catch (err) {
      toast.error(`Failed to release takeover: ${(err as Error).message}`)
    } finally {
      setPendingReleaseID(null)
    }
  }

  if (!items || items.length === 0) return null

  return (
    <>
      <TakeoverModal info={resumeInfo} onClose={() => setResumeInfo(null)} />
      <div className="mb-4 bg-surface-raised border border-border-glass rounded-2xl overflow-hidden">
        <button
          onClick={() => setExpanded((v) => !v)}
          className="w-full px-4 py-3 flex items-center justify-between hover:bg-black/[0.02] transition-colors"
          aria-expanded={expanded}
        >
          <div className="flex items-center gap-3">
            <span className="inline-block w-1.5 h-1.5 rounded-full bg-accent" />
            <span className="text-[13px] font-semibold text-text-primary">
              Held takeovers ({items.length})
            </span>
            <span className="text-[11px] text-text-tertiary">
              Release to free up the branch ref for a future delegated run
            </span>
          </div>
          <svg
            width="14"
            height="14"
            viewBox="0 0 16 16"
            fill="none"
            className={`text-text-tertiary transition-transform ${expanded ? 'rotate-90' : ''}`}
          >
            <path
              d="M6 3l5 5-5 5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        </button>

        {expanded && (
          <div className="border-t border-border-subtle divide-y divide-border-subtle">
            {items.map((it) => (
              <div key={it.run_id} className="px-4 py-3 flex items-start justify-between gap-4">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2 text-[12px]">
                    <span className="text-text-tertiary">{it.source_id || '—'}</span>
                    <span className="font-medium text-text-primary truncate">
                      {it.task_title || 'Untitled task'}
                    </span>
                  </div>
                  <div className="mt-1 text-[11px] font-mono text-text-tertiary truncate">
                    {it.takeover_path}
                  </div>
                </div>
                <div className="shrink-0 flex items-center gap-2">
                  <button
                    onClick={() =>
                      setResumeInfo({
                        takeover_path: it.takeover_path,
                        session_id: it.session_id,
                        resume_command: it.resume_command,
                      })
                    }
                    className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
                  >
                    Show resume info
                  </button>
                  <button
                    onClick={() => release(it.run_id)}
                    disabled={pendingReleaseID === it.run_id}
                    className="text-[12px] text-text-tertiary hover:text-dismiss disabled:opacity-50 disabled:cursor-wait font-medium transition-colors"
                  >
                    {pendingReleaseID === it.run_id ? 'Releasing…' : 'Release'}
                  </button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  )
}
