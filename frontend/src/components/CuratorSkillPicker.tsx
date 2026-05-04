import { useEffect, useRef, useState } from 'react'
import type { Prompt } from '../types'

interface Props {
  /** Current selection. Empty string means "use the seeded system default."
   *  The default option remains selectable explicitly so users who
   *  customized once can revert without having to find the seed prompt
   *  in the dropdown by name. */
  value: string
  /** Called on every change. The caller is responsible for PATCHing the
   *  project; this component just fires intents. Returning false from
   *  onChange does NOT roll back the visible selection — fast successive
   *  clicks should land on the user's last choice, not whichever earlier
   *  PATCH last succeeded. The caller should toast on failure. */
  onChange: (promptId: string) => Promise<boolean | undefined> | void
}

// CuratorSkillPicker is the per-project picker for which prompt the
// Curator materializes as its `ticket-spec` Claude Code skill. SKY-221.
// Sits inline beside other project settings; not a modal.
//
// Behavior:
//   - Lists every visible prompt (system + user). The seeded
//     `system-ticket-spec` is shown labeled (Default).
//   - "Use default" is the empty-string value — equivalent to picking
//     the seeded prompt today, but distinguishable on the wire so a
//     future change to the default flows automatically.
//   - On select, immediately PATCHes the project. The Curator's next
//     turn rewrites SKILL.md from this choice; no session reset
//     required.
export default function CuratorSkillPicker({ value, onChange }: Props) {
  const [prompts, setPrompts] = useState<Prompt[]>([])
  const [loading, setLoading] = useState(true)
  const [fetchFailed, setFetchFailed] = useState(false)

  // Per-mount AbortController so an unmount mid-fetch doesn't leak a
  // setState into a torn-down component.
  useEffect(() => {
    const ac = new AbortController()
    fetch('/api/prompts', { signal: ac.signal })
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error('list prompts'))))
      .then((data: Prompt[]) => {
        setPrompts(data)
        setLoading(false)
      })
      .catch((err) => {
        if (ac.signal.aborted) return
        setFetchFailed(true)
        setLoading(false)
        console.warn('[CuratorSkillPicker] failed to load prompts', err)
      })
    return () => ac.abort()
  }, [])

  // Coalesce overlapping selections the same shape as the tracker
  // pickers in IntegrationsPanel: queue-latest serializes server
  // writes per-field while still letting the user click rapidly
  // without dropping their final intent.
  const inflight = useRef(false)
  const target = useRef<string | null>(null)

  const drain = async () => {
    while (target.current !== null) {
      const next = target.current
      target.current = null
      const ok = await onChange(next)
      if (ok === false) {
        target.current = null
        break
      }
    }
  }

  const handleChange = async (e: React.ChangeEvent<HTMLSelectElement>) => {
    target.current = e.target.value
    if (inflight.current) return
    inflight.current = true
    try {
      await drain()
    } finally {
      inflight.current = false
    }
  }

  if (fetchFailed) {
    return (
      <p className="text-[12px] text-text-tertiary">
        Couldn't load prompts. Refresh the page to retry.
      </p>
    )
  }

  return (
    <div>
      <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
        Ticket-spec skill
      </label>
      <select
        value={value}
        onChange={handleChange}
        disabled={loading}
        className="
          w-full rounded-lg border border-border-subtle
          bg-white/60 px-3 py-2 text-[13px] text-text-primary
          focus:outline-none focus:border-accent focus:bg-white
          disabled:opacity-60
        "
      >
        <option value="">Default (system-ticket-spec)</option>
        {prompts.map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
            {p.source === 'system' ? ' · system' : ''}
          </option>
        ))}
      </select>
      <p className="text-[11px] text-text-tertiary mt-1.5 leading-relaxed">
        The Curator uses this prompt as a Claude Code skill when drafting tickets for this project.
        Edits take effect on the next turn — no session reset.
      </p>
    </div>
  )
}
