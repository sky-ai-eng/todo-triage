// Right-side drawer for a single agent run opened from the factory view.
// Wraps the existing AgentCard component so the run display matches the
// Board's delegated-column card verbatim. Fetches messages on open; the
// task + run are already passed in from the station overlay so we don't
// re-fetch what we already have.
//
// Modeled after PromptDrawer's enter/exit animation + backdrop pattern so
// the factory's interactive controls feel consistent with the rest of the
// app. The body of the drawer lives in its own inner component so state
// resets naturally on each open (no effect-body setState required).

import { useEffect, useState } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import AgentCard from '../components/AgentCard'
import type { AgentMessage, AgentRun, Task } from '../types'

interface Props {
  task: Task | null
  run: AgentRun | null
  onClose: () => void
}

export default function RunDrawer({ task, run, onClose }: Props) {
  const open = task != null && run != null

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  return (
    <AnimatePresence>
      {open && task && run && (
        <>
          <motion.div
            className="fixed inset-0 bg-black/10 backdrop-blur-sm z-40"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          <motion.div
            className="fixed top-0 right-0 bottom-0 z-50 bg-surface border-l border-border-glass shadow-2xl shadow-black/10 flex flex-col"
            style={{ width: Math.min(540, window.innerWidth * 0.9) }}
            initial={{ x: '100%' }}
            animate={{ x: 0 }}
            exit={{ x: '100%' }}
            transition={{ type: 'spring', damping: 30, stiffness: 300 }}
          >
            <RunDrawerBody task={task} run={run} onClose={onClose} />
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}

interface BodyProps {
  task: Task
  run: AgentRun
  onClose: () => void
}

// Inner body — lifetime is controlled by AnimatePresence, so we get a
// fresh mount each time the drawer opens. That means `loading` starts at
// true, gets flipped to false in the fetch's finally, and we never need a
// synchronous setState in the effect body.
function RunDrawerBody({ task, run, onClose }: BodyProps) {
  const [messages, setMessages] = useState<AgentMessage[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    fetch(`/api/agent/runs/${run.ID}/messages`)
      .then((r) => {
        if (!r.ok) throw new Error(`Failed to load messages (${r.status})`)
        return r.json() as Promise<AgentMessage[]>
      })
      .then((data) => {
        if (cancelled) return
        setMessages(data ?? [])
      })
      .catch((err) => {
        if (cancelled) return
        setError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [run.ID])

  return (
    <>
      <div className="px-5 py-4 border-b border-border-subtle flex items-center justify-between shrink-0">
        <div className="min-w-0 flex-1">
          <div className="text-[11px] font-semibold uppercase tracking-wider text-text-tertiary">
            Run
          </div>
          <div className="text-[14px] font-semibold text-text-primary truncate">
            {task.title || task.source_id}
          </div>
        </div>
        <button
          onClick={onClose}
          className="ml-3 w-7 h-7 rounded-full text-text-tertiary hover:text-text-primary hover:bg-black/[0.04] flex items-center justify-center text-[16px]"
          aria-label="Close"
        >
          ×
        </button>
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto p-5">
        {loading && messages.length === 0 ? (
          <div className="text-[12px] text-text-tertiary">Loading messages…</div>
        ) : error ? (
          <div className="text-[12px] text-dismiss">{error}</div>
        ) : (
          <AgentCard task={task} run={run} messages={messages} />
        )}
      </div>
    </>
  )
}
