import { useState, useEffect } from 'react'
import { Link } from 'react-router-dom'
import { readError } from '../lib/api'

interface SettingsResponse {
  jira: {
    enabled: boolean
    projects: string[]
  }
}

interface Props {
  jiraKey: string
  linearKey: string
  onJiraChange: (key: string) => void
  onLinearChange: (key: string) => void
}

// TrackerProjectPickers renders two independent single-selects: Jira
// (sourced from config.Jira.Projects) and Linear (always disabled
// today — Linear integration is future). The two are independent on
// purpose; a project legitimately may track work in both systems.
//
// linearKey is plumbed through but disabled for now so the component
// stays forward-compatible with the Linear integration ticket without
// a future ticket needing to refactor the create modal / detail view.
export default function TrackerProjectPickers({
  jiraKey,
  linearKey,
  onJiraChange,
  onLinearChange,
}: Props) {
  const [jiraProjects, setJiraProjects] = useState<string[]>([])
  const [jiraEnabled, setJiraEnabled] = useState(false)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const res = await fetch('/api/settings')
        if (!res.ok) {
          // Don't toast — the modal will just render the disabled
          // state, which is the same UX as "Jira not configured."
          await readError(res, 'failed to load settings')
          return
        }
        const data: SettingsResponse = await res.json()
        if (!cancelled) {
          const projects = data.jira.projects || []
          setJiraEnabled(data.jira.enabled)
          setJiraProjects(projects)
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    load()
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div className="space-y-3">
      <div>
        <div className="flex items-center justify-between mb-1">
          <span className="text-[11px] font-medium text-text-tertiary uppercase tracking-wide">
            Jira
          </span>
          {!loading && !jiraEnabled && (
            <Link to="/settings" className="text-[11px] text-accent hover:underline">
              Configure Jira
            </Link>
          )}
        </div>
        {loading ? (
          <div className="text-[12px] text-text-tertiary py-1">Loading…</div>
        ) : !jiraEnabled || jiraProjects.length === 0 ? (
          <div className="text-[12px] text-text-tertiary py-1 italic">
            {jiraEnabled
              ? 'No Jira projects in config — add them on the Settings page.'
              : 'Jira not configured.'}
          </div>
        ) : (
          <select
            value={jiraKey}
            onChange={(e) => onJiraChange(e.target.value)}
            className="
              w-full rounded-lg border border-border-subtle
              bg-white/60 px-3 py-2 text-[13px] text-text-primary
              focus:outline-none focus:border-accent focus:bg-white
            "
          >
            <option value="">— None —</option>
            {jiraProjects.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        )}
      </div>

      <div>
        <div className="flex items-center justify-between mb-1">
          <span className="text-[11px] font-medium text-text-tertiary uppercase tracking-wide">
            Linear
          </span>
        </div>
        <div className="text-[12px] text-text-tertiary py-1 italic">
          Linear integration coming soon.
        </div>
        {/* Hidden input so the parent's controlled-state plumbing doesn't
            need a special-case for "Linear is always empty" — when the
            integration ships, this swaps in a real <select> and the
            create handler keeps working unchanged. */}
        <input type="hidden" value={linearKey} readOnly />
        {/* Silence unused-prop lint for the future Linear path. */}
        <span className="hidden">{onLinearChange.length}</span>
      </div>
    </div>
  )
}
